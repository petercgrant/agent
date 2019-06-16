package plugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilteringPlugins(t *testing.T) {
	t.Parallel()

	plugin := &Plugin{
		Location:      "github.com/buildkite/plugins/docker-compose",
		Version:       "a34fa34",
		Scheme:        "http",
		Configuration: map[string]interface{}{"container": "app"},
	}

	filter, err := ParseFilter(`plugin.scheme == "http"`)
	if err != nil {
		t.Fatal(err)
	}

	match, err := filter.Match(plugin)
	if err != nil {
		t.Fatal(err)
	}

	assert.True(t, match)
}
