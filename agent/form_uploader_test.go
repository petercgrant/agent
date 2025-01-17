package agent

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/agent/api"
	"github.com/buildkite/agent/logger"
)

func TestFormUploading(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case `/buildkiteartifacts.com`:
			if req.ContentLength <= 0 {
				t.Error("Expected a Content-Length header")
				http.Error(rw, "Bad requests", http.StatusBadRequest)
				return
			}

			err := req.ParseMultipartForm(5 * 1024 * 1024)
			if err != nil {
				t.Error(err)
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}

			// Check the ${artifact:path} interpolation is working
			path := req.FormValue("path")
			if path != "llamas.txt" {
				t.Errorf("Bad path content %q", path)
				http.Error(rw, "Bad path content", http.StatusInternalServerError)
				return
			}

			file, _, err := req.FormFile("file")
			if err != nil {
				t.Error(err)
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
			defer file.Close()

			b := &bytes.Buffer{}
			_, _ = io.Copy(b, file)

			// Check the file is attached correctly
			if b.String() != "llamas" {
				t.Errorf("Bad file content %q", b.String())
				http.Error(rw, "Bad file content", http.StatusInternalServerError)
				return
			}

		default:
			t.Errorf("Unknown path %s %s", req.Method, req.URL.Path)
			http.Error(rw, "Not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	temp, err := ioutil.TempDir("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(temp)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	tc := []string{temp, cwd}

	runtest := func(wd string) {
		abspath := filepath.Join(wd, "llamas.txt")
		err = ioutil.WriteFile(abspath, []byte("llamas"), 0700)
		defer os.Remove(abspath)

		uploader := NewFormUploader(logger.Discard, FormUploaderConfig{})
		artifact := &api.Artifact{
			ID:           "xxxxx-xxxx-xxxx-xxxx-xxxxxxxxxx",
			Path:         "llamas.txt",
			AbsolutePath: abspath,
			GlobPath:     "llamas.txt",
			ContentType:  "text/plain",
			UploadInstructions: &api.ArtifactUploadInstructions{
				Data: map[string]string{
					"path": "${artifact:path}",
				},
				Action: struct {
					URL       string "json:\"url,omitempty\""
					Method    string "json:\"method\""
					Path      string "json:\"path\""
					FileInput string "json:\"file_input\""
				}{
					URL:       server.URL,
					Method:    "POST",
					Path:      "buildkiteartifacts.com",
					FileInput: "file",
				}},
		}

		if err := uploader.Upload(artifact); err != nil {
			t.Fatal(err)
		}
	}

	for _, wd := range tc {
		runtest(wd)
	}
}
