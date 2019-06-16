package agent

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/agent/api"
	"github.com/buildkite/agent/experiments"
	"github.com/buildkite/agent/logger"
	"github.com/buildkite/agent/metrics"
	"github.com/buildkite/agent/process"
	"github.com/buildkite/agent/retry"
	"github.com/buildkite/shellwords"
)

type JobRunnerConfig struct {
	// The configuration of the agent from the CLI
	AgentConfiguration AgentConfiguration

	// Whether to set debug in the job
	Debug bool
}

type JobRunner struct {
	// The configuration for the job runner
	conf JobRunnerConfig

	// The logger to use
	logger logger.Logger

	// The registered agent API record running this job
	agent *api.AgentRegisterResponse

	// The job being run
	job *api.Job

	// The APIClient that will be used when updating the job
	apiClient APIClient

	// A scope for metrics within a job
	metrics *metrics.Scope

	// Go context for goroutine supervision
	context       context.Context
	contextCancel context.CancelFunc

	// The internal process of the job
	process *process.Process

	// The internal buffer of the process output
	output *process.Buffer

	// The internal header time streamer
	headerTimesStreamer *headerTimesStreamer

	// The internal log streamer
	logStreamer *LogStreamer

	// If the job is being cancelled
	cancelled bool

	// Used to wait on various routines that we spin up
	routineWaitGroup sync.WaitGroup

	// A lock to protect concurrent calls to cancel
	cancelLock sync.Mutex

	// File containing a copy of the job env
	envFile *os.File
}

// Initializes the job runner
func NewJobRunner(l logger.Logger, scope *metrics.Scope, ag *api.AgentRegisterResponse, j *api.Job, apiClient APIClient, conf JobRunnerConfig) (*JobRunner, error) {
	runner := &JobRunner{
		agent:     ag,
		job:       j,
		logger:    l,
		conf:      conf,
		metrics:   scope,
		apiClient: apiClient,
	}

	runner.context, runner.contextCancel = context.WithCancel(context.Background())

	// Create our header times struct
	runner.headerTimesStreamer = newHeaderTimesStreamer(l, runner.onUploadHeaderTime)

	// The log streamer that will take the output chunks, and send them to
	// the Buildkite Agent API
	runner.logStreamer = NewLogStreamer(l, runner.onUploadChunk, LogStreamerConfig{
		Concurrency:       3,
		MaxChunkSizeBytes: j.ChunksMaxSizeBytes,
	})

	// TempDir is not guaranteed to exist
	tempDir := os.TempDir()
	if _, err := os.Stat(tempDir); os.IsNotExist(err) {
		if err = os.MkdirAll(tempDir, 0777); err != nil {
			return nil, err
		}
	}

	// Prepare a file to recieve the given job environment
	if file, err := ioutil.TempFile(tempDir, fmt.Sprintf("job-env-%s", j.ID)); err != nil {
		return runner, err
	} else {
		l.Debug("[JobRunner] Created env file: %s", file.Name())
		runner.envFile = file
	}

	env, err := runner.createEnvironment()
	if err != nil {
		return nil, err
	}

	// The bootstrap-script gets parsed based on the operating system
	cmd, err := shellwords.Split(conf.AgentConfiguration.BootstrapScript)
	if err != nil {
		return nil, fmt.Errorf("Failed to split bootstrap-script (%q) into tokens: %v",
			conf.AgentConfiguration.BootstrapScript, err)
	}

	// Our log streamer works off a buffer of output
	runner.output = &process.Buffer{}

	// The writer that output from the process goes into
	var processWriter io.Writer

	// If we have timestamp lines on, we have to buffer lines before we flush them
	if conf.AgentConfiguration.TimestampLines {
		var pr *io.PipeReader
		pr, processWriter = io.Pipe()

		go func() {
			// Use a scanner to process output line by line
			err := process.NewScanner(l).ScanLines(pr, func(line string) {
				// Send to our header streamer and determine if it's a header
				isHeader := runner.headerTimesStreamer.Scan(line)

				// Prefix non-header log lines with timestamps
				if !(isHeaderExpansion(line) || isHeader) {
					line = fmt.Sprintf("[%s] %s", time.Now().UTC().Format(time.RFC3339), line)
				}

				// Write the log line to the buffer
				runner.output.Write([]byte(line + "\n"))
			})
			if err != nil {
				l.Error("[LineScanner] Encountered error %v", err)
			}
		}()
	} else {
		pr, pw := io.Pipe()

		// Write output directly to the line buffer so we
		processWriter = io.MultiWriter(pw, runner.output)

		// Use a scanner to process output for headers only
		go func() {
			err := process.NewScanner(l).ScanLines(pr, func(line string) {
				runner.headerTimesStreamer.Scan(line)
			})
			if err != nil {
				l.Error("[LineScanner] Encountered error %v", err)
			}
		}()
	}

	// Copy the current processes ENV and merge in the new ones. We do this
	// so the sub process gets PATH and stuff. We merge our path in over
	// the top of the current one so the ENV from Buildkite and the agent
	// take precedence over the agent
	processEnv := append(os.Environ(), env...)

	// The process that will run the bootstrap script
	runner.process = process.New(l, process.Config{
		Path:   cmd[0],
		Args:   cmd[1:],
		Env:    processEnv,
		PTY:    conf.AgentConfiguration.RunInPty,
		Stdout: processWriter,
		Stderr: processWriter,
	})

	// Kick off our callback when the process starts
	go func() {
		<-runner.process.Started()
		runner.onProcessStartCallback()
	}()

	return runner, nil
}

// Runs the job
func (r *JobRunner) Run() error {
	r.logger.Info("Starting job %s", r.job.ID)

	startedAt := time.Now()

	// Start the build in the Buildkite Agent API. This is the first thing
	// we do so if it fails, we don't have to worry about cleaning things
	// up like started log streamer workers, etc.
	if err := r.startJob(startedAt); err != nil {
		return err
	}

	// Start the header time streamer
	if err := r.headerTimesStreamer.Start(); err != nil {
		return err
	}

	// Start the log streamer
	if err := r.logStreamer.Start(); err != nil {
		return err
	}

	// Run the process. This will block until it finishes.
	if err := r.process.Run(); err != nil {
		// Send the error as output
		r.logStreamer.Process(fmt.Sprintf("%s", err))
	} else {
		// Add the final output to the streamer
		r.logStreamer.Process(r.output.String())
	}

	// Store the finished at time
	finishedAt := time.Now()

	// Stop the header time streamer. This will block until all the chunks
	// have been uploaded
	r.headerTimesStreamer.Stop()

	// Stop the log streamer. This will block until all the chunks have
	// been uploaded
	r.logStreamer.Stop()

	// Warn about failed chunks
	if count := r.logStreamer.FailedChunks(); count > 0 {
		r.logger.Warn("%d chunks failed to upload for this job", count)
	}

	// Wait for the routines that we spun up to finish
	r.logger.Debug("[JobRunner] Waiting for all other routines to finish")
	r.contextCancel()
	r.routineWaitGroup.Wait()

	// Remove the env file, if any
	if r.envFile != nil {
		if err := os.Remove(r.envFile.Name()); err != nil {
			r.logger.Warn("[JobRunner] Error cleaning up env file: %s", err)
		}
		r.logger.Debug("[JobRunner] Deleted env file: %s", r.envFile.Name())
	}

	exitStatus := fmt.Sprintf("%d", r.process.WaitStatus().ExitStatus())

	jobMetrics := r.metrics.With(metrics.Tags{
		"exit_code": exitStatus,
	})

	// Write some metrics about the job run
	if exitStatus == "0" {
		jobMetrics.Timing(`jobs.duration.success`, finishedAt.Sub(startedAt))
		jobMetrics.Count(`jobs.success`, 1)
	} else {
		jobMetrics.Timing(`jobs.duration.error`, finishedAt.Sub(startedAt))
		jobMetrics.Count(`jobs.failed`, 1)
	}

	// Finish the build in the Buildkite Agent API
	//
	// Once we tell the API we're finished it might assign us new work, so make
	// sure everything else is done first.
	r.finishJob(finishedAt, exitStatus, r.logStreamer.FailedChunks())

	r.logger.Info("Finished job %s", r.job.ID)

	return nil
}

func (r *JobRunner) Cancel() error {
	r.cancelLock.Lock()
	defer r.cancelLock.Unlock()

	if r.cancelled {
		return nil
	}

	if r.process == nil {
		r.logger.Error("No process to kill")
		return nil
	}

	r.logger.Info("Canceling job %s with a grace period of %ds",
		r.job.ID, r.conf.AgentConfiguration.CancelGracePeriod)

	// First we interrupt the process (ctrl-c or SIGINT)
	if err := r.process.Interrupt(); err != nil {
		return err
	}

	select {
	// Grace period for cancelling
	case <-time.After(time.Second * time.Duration(r.conf.AgentConfiguration.CancelGracePeriod)):
		r.logger.Info("Job %s hasn't stopped in time, terminating", r.job.ID)

		// Terminate the process as we've exceeded our context
		if err := r.process.Terminate(); err != nil {
			return err
		}

		return nil

	// Process successfully terminated
	case <-r.process.Done():
		return nil
	}
}

// Creates the environment variables that will be used in the process and writes a flat environment file
func (r *JobRunner) createEnvironment() ([]string, error) {
	// Create a clone of our jobs environment. We'll then set the
	// environment variables provided by the agent, which will override any
	// sent by Buildkite. The variables below should always take
	// precedence.
	env := make(map[string]string)
	for key, value := range r.job.Env {
		env[key] = value
	}

	// The agent registration token should never make it into the job environment
	if _, exists := env[`BUILDKITE_AGENT_TOKEN`]; exists {
		delete(env, `BUILDKITE_AGENT_TOKEN`)
	}

	// Write out the job environment to a file, in k="v" format, with newlines escaped
	// We present only the clean environment - i.e only variables configured
	// on the job upstream - and expose the path in another environment variable.
	if r.envFile != nil {
		for key, value := range env {
			if _, err := r.envFile.WriteString(fmt.Sprintf("%s=%q\n", key, value)); err != nil {
				return nil, err
			}
		}
		if err := r.envFile.Close(); err != nil {
			return nil, err
		}
		env["BUILDKITE_ENV_FILE"] = r.envFile.Name()
	}

	// Certain env can only be set by agent configuration.
	// We show the user a warning in the bootstrap if they use any of these at a job level.

	var protectedEnv = []string{
		`BUILDKITE_AGENT_ENDPOINT`,
		`BUILDKITE_AGENT_ACCESS_TOKEN`,
		`BUILDKITE_AGENT_DEBUG`,
		`BUILDKITE_AGENT_PID`,
		`BUILDKITE_BIN_PATH`,
		`BUILDKITE_CONFIG_PATH`,
		`BUILDKITE_BUILD_PATH`,
		`BUILDKITE_GIT_MIRRORS_PATH`,
		`BUILDKITE_HOOKS_PATH`,
		`BUILDKITE_PLUGINS_PATH`,
		`BUILDKITE_SSH_KEYSCAN`,
		`BUILDKITE_GIT_SUBMODULES`,
		`BUILDKITE_COMMAND_EVAL`,
		`BUILDKITE_PLUGINS_ENABLED`,
		`BUILDKITE_LOCAL_HOOKS_ENABLED`,
		`BUILDKITE_GIT_CLONE_FLAGS`,
		`BUILDKITE_GIT_FETCH_FLAGS`,
		`BUILDKITE_GIT_CLONE_MIRROR_FLAGS`,
		`BUILDKITE_GIT_MIRRORS_LOCK_TIMEOUT`,
		`BUILDKITE_GIT_CLEAN_FLAGS`,
		`BUILDKITE_SHELL`,
	}

	var ignoredEnv []string

	// Check if the user has defined any protected env
	for _, p := range protectedEnv {
		if _, exists := r.job.Env[p]; exists {
			ignoredEnv = append(ignoredEnv, p)
		}
	}

	// Set BUILDKITE_IGNORED_ENV so the bootstrap can show warnings
	if len(ignoredEnv) > 0 {
		env["BUILDKITE_IGNORED_ENV"] = strings.Join(ignoredEnv, ",")
	}

	// Add the API configuration
	apiConfig := r.apiClient.Config()
	env["BUILDKITE_AGENT_ENDPOINT"] = apiConfig.Endpoint
	env["BUILDKITE_AGENT_ACCESS_TOKEN"] = apiConfig.Token

	// Add agent environment variables
	env["BUILDKITE_AGENT_DEBUG"] = fmt.Sprintf("%t", r.conf.Debug)
	env["BUILDKITE_AGENT_PID"] = fmt.Sprintf("%d", os.Getpid())

	// We know the BUILDKITE_BIN_PATH dir, because it's the path to the
	// currently running file (there is only 1 binary)
	dir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	env["BUILDKITE_BIN_PATH"] = dir

	// Add options from the agent configuration
	env["BUILDKITE_CONFIG_PATH"] = r.conf.AgentConfiguration.ConfigPath
	env["BUILDKITE_BUILD_PATH"] = r.conf.AgentConfiguration.BuildPath
	env["BUILDKITE_GIT_MIRRORS_PATH"] = r.conf.AgentConfiguration.GitMirrorsPath
	env["BUILDKITE_HOOKS_PATH"] = r.conf.AgentConfiguration.HooksPath
	env["BUILDKITE_PLUGINS_PATH"] = r.conf.AgentConfiguration.PluginsPath
	env["BUILDKITE_SSH_KEYSCAN"] = fmt.Sprintf("%t", r.conf.AgentConfiguration.SSHKeyscan)
	env["BUILDKITE_GIT_SUBMODULES"] = fmt.Sprintf("%t", r.conf.AgentConfiguration.GitSubmodules)
	env["BUILDKITE_COMMAND_EVAL"] = fmt.Sprintf("%t", r.conf.AgentConfiguration.CommandEval)
	env["BUILDKITE_PLUGINS_ENABLED"] = fmt.Sprintf("%t", r.conf.AgentConfiguration.PluginsEnabled)
	env["BUILDKITE_FILTER_PLUGINS"] = fmt.Sprintf("%s", r.conf.AgentConfiguration.FilterPlugins)
	env["BUILDKITE_LOCAL_HOOKS_ENABLED"] = fmt.Sprintf("%t", r.conf.AgentConfiguration.LocalHooksEnabled)
	env["BUILDKITE_GIT_CLONE_FLAGS"] = r.conf.AgentConfiguration.GitCloneFlags
	env["BUILDKITE_GIT_FETCH_FLAGS"] = r.conf.AgentConfiguration.GitFetchFlags
	env["BUILDKITE_GIT_CLONE_MIRROR_FLAGS"] = r.conf.AgentConfiguration.GitCloneMirrorFlags
	env["BUILDKITE_GIT_CLEAN_FLAGS"] = r.conf.AgentConfiguration.GitCleanFlags
	env["BUILDKITE_GIT_MIRRORS_LOCK_TIMEOUT"] = fmt.Sprintf("%d", r.conf.AgentConfiguration.GitMirrorsLockTimeout)
	env["BUILDKITE_SHELL"] = r.conf.AgentConfiguration.Shell
	env["BUILDKITE_AGENT_EXPERIMENT"] = strings.Join(experiments.Enabled(), ",")

	// Whether to enable profiling in the bootstrap
	if r.conf.AgentConfiguration.Profile != "" {
		env["BUILDKITE_AGENT_PROFILE"] = r.conf.AgentConfiguration.Profile
	}

	enablePluginValidation := r.conf.AgentConfiguration.PluginValidation

	// Allow BUILDKITE_PLUGIN_VALIDATION to be enabled from env for easier
	// per-pipeline testing
	if pluginValidation, ok := env["BUILDKITE_PLUGIN_VALIDATION"]; ok {
		switch pluginValidation {
		case "true", "1", "on":
			enablePluginValidation = true
		}
	}

	env["BUILDKITE_PLUGIN_VALIDATION"] = fmt.Sprintf("%t", enablePluginValidation)

	// Convert the env map into a slice (which is what the script gear
	// needs)
	envSlice := []string{}
	for key, value := range env {
		envSlice = append(envSlice, fmt.Sprintf("%s=%s", key, value))
	}

	return envSlice, nil
}

// Starts the job in the Buildkite Agent API. We'll retry on connection-related
// issues, but if a connection succeeds and we get an error response back from
// Buildkite, we won't bother retrying. For example, a "no such host" will
// retry, but a 422 from Buildkite won't.
func (r *JobRunner) startJob(startedAt time.Time) error {
	r.job.StartedAt = startedAt.UTC().Format(time.RFC3339Nano)

	return retry.Do(func(s *retry.Stats) error {
		_, err := r.apiClient.StartJob(r.job)

		if err != nil {
			if api.IsRetryableError(err) {
				r.logger.Warn("%s (%s)", err, s)
			} else {
				r.logger.Warn("Buildkite rejected the call to start the job (%s)", err)
				s.Break()
			}
		}

		return err
	}, &retry.Config{Maximum: 30, Interval: 5 * time.Second})
}

// Finishes the job in the Buildkite Agent API. This call will keep on retrying
// forever until it finally gets a successfull response from the API.
func (r *JobRunner) finishJob(finishedAt time.Time, exitStatus string, failedChunkCount int) error {
	r.job.FinishedAt = finishedAt.UTC().Format(time.RFC3339Nano)
	r.job.ExitStatus = exitStatus
	r.job.ChunksFailedCount = failedChunkCount

	return retry.Do(func(s *retry.Stats) error {
		response, err := r.apiClient.FinishJob(r.job)
		if err != nil {
			// If the API returns with a 422, that means that we
			// succesfully tried to finish the job, but Buildkite
			// rejected the finish for some reason. This can
			// sometimes mean that Buildkite has cancelled the job
			// before we get a chance to send the final API call
			// (maybe this agent took too long to kill the
			// process). In that case, we don't want to keep trying
			// to finish the job forever so we'll just bail out and
			// go find some more work to do.
			if response != nil && response.StatusCode == 422 {
				r.logger.Warn("Buildkite rejected the call to finish the job (%s)", err)
				s.Break()
			} else {
				r.logger.Warn("%s (%s)", err, s)
			}
		}

		return err
	}, &retry.Config{Forever: true, Interval: 1 * time.Second})
}

func (r *JobRunner) onProcessStartCallback() {
	// Since we're spinning up 2 routines here, we might as well add them
	// to the routine wait group here.
	r.routineWaitGroup.Add(2)

	// Start a routine that will grab the output every few seconds and send
	// it back to Buildkite
	go func() {
		defer func() {
			r.routineWaitGroup.Done()
			r.logger.Debug("[JobRunner] Routine that processes the log has finished")
		}()

		for {
			// Send the output of the process to the log streamer
			// for processing
			r.logStreamer.Process(r.output.String())

			// Sleep for a bit, or until the job is finished
			select {
			case <-time.After(1 * time.Second):
			case <-r.context.Done():
				return
			case <-r.process.Done():
				return
			}
		}

		// The final output after the process has finished is processed in Run()
	}()

	// Start a routine that will constantly ping Buildkite to see if the
	// job has been canceled
	go func() {
		defer func() {
			// Mark this routine as done in the wait group
			r.routineWaitGroup.Done()

			r.logger.Debug("[JobRunner] Routine that refreshes the job has finished")
		}()
		for {
			// Re-get the job and check it's status to see if it's been
			// cancelled
			jobState, _, err := r.apiClient.GetJobState(r.job.ID)
			if err != nil {
				// We don't really care if it fails, we'll just
				// try again soon anyway
				r.logger.Warn("Problem with getting job state %s (%s)", r.job.ID, err)
			} else if jobState.State == "canceling" || jobState.State == "canceled" {
				r.Cancel()
			}

			// Sleep for a bit, or until the job is finished
			select {
			case <-time.After(time.Duration(r.agent.JobStatusInterval) * time.Second):
			case <-r.context.Done():
				return
			case <-r.process.Done():
				return
			}
		}
	}()
}

func (r *JobRunner) onUploadHeaderTime(cursor int, total int, times map[string]string) {
	retry.Do(func(s *retry.Stats) error {
		response, err := r.apiClient.SaveHeaderTimes(r.job.ID, &api.HeaderTimes{Times: times})
		if err != nil {
			if response != nil && (response.StatusCode >= 400 && response.StatusCode <= 499) {
				r.logger.Warn("Buildkite rejected the header times (%s)", err)
				s.Break()
			} else {
				r.logger.Warn("%s (%s)", err, s)
			}
		}

		return err
	}, &retry.Config{Maximum: 10, Interval: 5 * time.Second})
}

// Call when a chunk is ready for upload.
func (r *JobRunner) onUploadChunk(chunk *LogStreamerChunk) error {
	// We consider logs to be an important thing, and we shouldn't give up
	// on sending the chunk data back to Buildkite. In the event Buildkite
	// is having downtime or there are connection problems, we'll want to
	// hold onto chunks until it's back online to upload them.
	//
	// This code will retry forever until we get back a successful response
	// from Buildkite that it's considered the chunk (a 4xx will be
	// returned if the chunk is invalid, and we shouldn't retry on that)
	return retry.Do(func(s *retry.Stats) error {
		response, err := r.apiClient.UploadChunk(r.job.ID, &api.Chunk{
			Data:     chunk.Data,
			Sequence: chunk.Order,
			Offset:   chunk.Offset,
			Size:     chunk.Size,
		})
		if err != nil {
			if response != nil && (response.StatusCode >= 400 && response.StatusCode <= 499) {
				r.logger.Warn("Buildkite rejected the chunk upload (%s)", err)
				s.Break()
			} else {
				r.logger.Warn("%s (%s)", err, s)
			}
		}

		return err
	}, &retry.Config{Forever: true, Jitter: true, Interval: 5 * time.Second})
}
