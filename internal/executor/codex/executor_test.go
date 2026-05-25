package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/domain"
)

func TestBuildArgsUsesConfiguredCodexOptions(t *testing.T) {
	executor := New(Config{
		Bin:              "/usr/local/bin/codex",
		Model:            "gpt-5.5",
		Workspace:        "/repo",
		Sandbox:          "danger-full-access",
		Approval:         "never",
		ReasoningEffort:  "xhigh",
		SkipGitRepoCheck: true,
	})

	require.Equal(t, []string{
		"--ask-for-approval", "never",
		"exec",
		"--cd", "/repo",
		"--sandbox", "danger-full-access",
		"--model", "gpt-5.5",
		"-c", `model_reasoning_effort="xhigh"`,
		"--skip-git-repo-check",
		"-",
	}, executor.BuildArgs())
}

func TestResultReadsStructuredResultJSON(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(resultPath, []byte(`{
  "status": "success",
  "summary": "fixed",
  "verification": ["rtk go test ./..."],
  "changedFiles": [{
    "path": "internal/runner/runner.go",
    "line": 123,
    "symbols": ["applySuccess"],
    "newSymbols": ["queueForIntegration"],
    "description": "queue integration instead of tagging done"
  }],
  "repoResults": [{
    "repo": "ml-svc-zcode",
    "repoPath": "/repo/ml-svc-zcode",
    "worktreePath": "/repo/.local_worktrees/issue-35/ml-svc-zcode",
    "issueBranch": "cnb/issue-35/ml-svc-zcode",
    "baselineBranch": "ai-v2.0",
    "headSha": "abc123",
    "commitSha": "abc123"
  }],
  "errorKind": "NONE",
  "errorMessage": ""
}`), 0o644))

	result := New(Config{}).Result(context.Background(), domain.WorkerState{ResultPath: resultPath})

	require.Equal(t, "success", result.Status)
	require.Equal(t, "fixed", result.Summary)
	require.Equal(t, []string{"rtk go test ./..."}, result.Verification)
	require.Len(t, result.ChangedFiles, 1)
	require.Equal(t, "internal/runner/runner.go", result.ChangedFiles[0].Path)
	require.Len(t, result.RepoResults, 1)
	require.Equal(t, "ml-svc-zcode", result.RepoResults[0].Repo)
	require.Equal(t, domain.ErrorNone, result.ErrorKind)
}

func TestResultFallsBackToLogMarkerAndErrorLine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "issue.log")
	require.NoError(t, os.WriteFile(logPath, []byte(`
=== worker start 2026-05-08 10:00:00 ===
ERROR: Selected model is at capacity. Please try a different model.
=== worker exit code 1 2026-05-08 10:05:00 ===
`), 0o644))

	result := New(Config{}).Result(context.Background(), domain.WorkerState{LogPath: logPath})

	require.NotNil(t, result.ExitCode)
	require.Equal(t, 1, *result.ExitCode)
	require.Contains(t, result.ErrorMessage, "Selected model is at capacity")
}

func TestResultFallsBackToLowercaseCodexCLIError(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "issue.log")
	require.NoError(t, os.WriteFile(logPath, []byte(`
=== worker start 2026-05-15 11:02:30 ===
error: unexpected argument '--ask-for-approval' found
=== worker exit code 2 2026-05-15 11:02:30 ===
`), 0o644))

	result := New(Config{}).Result(context.Background(), domain.WorkerState{LogPath: logPath})

	require.NotNil(t, result.ExitCode)
	require.Equal(t, 2, *result.ExitCode)
	require.Contains(t, result.ErrorMessage, "unexpected argument")
}

func TestTerminateKillsProcessGroup(t *testing.T) {
	var killedPID int
	executor := New(Config{})
	executor.killProcessGroup = func(pid int) error {
		killedPID = pid
		return nil
	}

	require.NoError(t, executor.Terminate(context.Background(), domain.WorkerState{PID: 42}))
	require.Equal(t, 42, killedPID)
}
