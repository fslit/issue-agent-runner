package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadAppliesDefaultsAndResolvesPaths(t *testing.T) {
	tempDir := t.TempDir()
	workspace := filepath.Join(tempDir, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	configPath := filepath.Join(tempDir, "runner.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
project:
  name: zcode
  workspace: `+workspace+`
  serviceRepo: ml-svc-zcode
repositories:
  - name: ml-svc-zcode
    path: ml-svc-zcode
    baselineBranch: ai-v2.0
  - name: ml-web-zcode
    path: ml-web-zcode
    issueBranchPrefix: cnb
    baselineBranch: poc/api-adapter-validation
    directBaseline: true
provider:
  type: cnb
  repo: dianyuniot/mlink/ml-svc-zcode
executor:
  type: codex
  bin: codex
paths:
  stateDir: .local_cache/cnb-issue-runner
  promptTemplate: templates/worker_prompt.md
`), 0o644))

	cfg, err := Load(configPath)
	require.NoError(t, err)

	require.Equal(t, 60, cfg.Runner.IntervalSeconds)
	require.Equal(t, "gpt-5.5", cfg.Executor.Model)
	require.Equal(t, "xhigh", cfg.Executor.ReasoningEffort)
	require.Equal(t, 5, cfg.Runner.MaxConcurrency)
	require.Equal(t, 3, cfg.Runner.MaxAttempts)
	require.Equal(t, 300, cfg.Runner.RetryBackoffSeconds)
	require.Equal(t, 900, cfg.Runner.CapacityBackoffSeconds)
	require.Equal(t, 3600, cfg.Runner.QuotaBackoffSeconds)
	require.Equal(t, 7200, cfg.Runner.StaleWorkerSeconds)
	require.Equal(t, 14400, cfg.Runner.HardWorkerSeconds)
	require.Equal(t, "处理中", cfg.Labels.Running)
	require.Equal(t, []string{"已处理", "处理中", "处理失败", "需人工确认"}, cfg.Labels.Skip)
	require.Equal(t, filepath.Join(workspace, ".local_cache/cnb-issue-runner"), cfg.Paths.StateDir)
	require.Equal(t, filepath.Join(workspace, ".local_cache/cnb-issue-runner", "runner.log"), cfg.Paths.RunnerLog)
	require.False(t, cfg.Callback.Enabled)
	require.Equal(t, "127.0.0.1:8791", cfg.Callback.Listen)
	require.Equal(t, "/callbacks/cnb", cfg.Callback.Path)
	require.Equal(t, "X-Issue-Agent-Token", cfg.Callback.SecretHeader)
	require.Len(t, cfg.Repositories, 2)
	require.Equal(t, filepath.Join(workspace, "ml-svc-zcode"), cfg.Repositories[0].Path)
	require.Equal(t, "cnb", cfg.Repositories[0].IssueBranchPrefix)
	require.False(t, cfg.Repositories[0].DirectBaseline)
	require.True(t, cfg.Repositories[1].DirectBaseline)
	require.Equal(t, filepath.Join(workspace, ".local_worktrees/issue-agent-runner"), cfg.Integration.WorktreeRoot)
	require.Equal(t, "no-ff", cfg.Integration.MergeMode)
	require.True(t, cfg.Integration.PushOnClose)
	require.True(t, cfg.Integration.CleanupOnClose)
	require.True(t, cfg.Integration.AllowSinglePendingVerify)
	require.Equal(t, 0, cfg.Hooks.OnIssueIntegrated.TimeoutSeconds)
	require.Equal(t, "", cfg.Hooks.OnIssueIntegrated.Command)
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "missing workspace",
			content: `
provider:
  type: cnb
executor:
  type: codex
`,
			want: "project.workspace is required",
		},
		{
			name: "unsupported provider",
			content: `
project:
  workspace: /tmp
provider:
  type: github
executor:
  type: codex
`,
			want: "unsupported provider.type",
		},
		{
			name: "unsupported executor",
			content: `
project:
  workspace: /tmp
provider:
  type: cnb
executor:
  type: shell
`,
			want: "unsupported executor.type",
		},
		{
			name: "invalid concurrency",
			content: `
project:
  workspace: /tmp
provider:
  type: cnb
executor:
  type: codex
runner:
  maxConcurrency: 0
`,
			want: "runner.maxConcurrency must be at least 1",
		},
		{
			name: "callback path must be absolute",
			content: `
project:
  workspace: /tmp
provider:
  type: cnb
executor:
  type: codex
callback:
  enabled: true
  path: callbacks/cnb
`,
			want: "callback.path must start with /",
		},
		{
			name: "repository missing baseline branch",
			content: `
project:
  workspace: /tmp
repositories:
  - name: ml-svc-zcode
    path: ml-svc-zcode
provider:
  type: cnb
executor:
  type: codex
`,
			want: "repositories[ml-svc-zcode].baselineBranch is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "runner.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte(tt.content), 0o644))

			_, err := Load(configPath)
			require.ErrorContains(t, err, tt.want)
		})
	}
}
