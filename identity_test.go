package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalRepositoryIdentity(t *testing.T) {
	require.Equal(t, "mssql", agent.Name)

	legacyAgentName := strings.Join([]string{"external", "microsoft", "sql", "server"}, "-")
	legacyModulePath := "github.com/codefly-dev/service-" + legacyAgentName

	checks := []struct {
		path       string
		want       string
		mustReject string
	}{
		{
			path:       "agent.codefly.yaml",
			want:       "kind: codefly:service\nname: mssql",
			mustReject: "name: " + legacyAgentName,
		},
		{
			path:       "go.mod",
			want:       "module github.com/codefly-dev/service-mssql",
			mustReject: "module " + legacyModulePath,
		},
		{
			path:       "runtime.go",
			want:       `"github.com/codefly-dev/service-mssql/migrations"`,
			mustReject: `"` + legacyModulePath + `/migrations"`,
		},
		{
			path:       ".goreleaser.yaml",
			want:       "name: service-mssql",
			mustReject: "name: service-" + legacyAgentName,
		},
	}

	for _, check := range checks {
		t.Run(check.path, func(t *testing.T) {
			content, err := os.ReadFile(check.path)
			require.NoError(t, err)
			require.Contains(t, string(content), check.want)
			require.NotContains(t, string(content), check.mustReject)
		})
	}
}
