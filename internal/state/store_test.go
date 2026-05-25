package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/domain"
)

func TestLoadAcceptsLegacyPythonState(t *testing.T) {
	stateDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(`{
  "workers": {
    "35": {
      "pid": 12345,
      "title": "legacy worker",
      "startedAt": 1710000000,
      "log": "/tmp/issue-35.log"
    }
  },
  "issues": {
    "34": {
      "attempts": 3,
      "paused": true,
      "pauseReason": "failed 3 attempts",
      "lastExitCode": 1,
      "lastError": "ERROR: Selected model is at capacity. Please try a different model.",
      "lastStartedAt": 1710000000,
      "lastFinishedAt": 1710000300,
      "nextRetryAt": 1710001200,
      "title": "legacy issue",
      "workflowStatus": "READY_TO_INTEGRATE",
      "readyAt": 1710000400,
      "repositories": {
        "ml-svc-zcode": {
          "repo": "ml-svc-zcode",
          "issueBranch": "cnb/issue-34/ml-svc-zcode",
          "baselineBranch": "ai-v2.0"
        }
      },
      "customField": "keep me"
    }
  }
}`), 0o644))

	loaded, err := New(stateDir).Load()
	require.NoError(t, err)

	require.Equal(t, 1, loaded.Version)
	require.Equal(t, "/tmp/issue-35.log", loaded.Workers["35"].LogPathValue())
	require.Equal(t, 3, loaded.Issues["34"].Attempts)
	require.True(t, loaded.Issues["34"].Paused)
	require.Equal(t, domain.IssueWorkflowStatusReadyToIntegrate, loaded.Issues["34"].WorkflowStatus)
	require.Equal(t, "ai-v2.0", loaded.Issues["34"].Repositories["ml-svc-zcode"].BaselineBranch)
	require.Equal(t, "keep me", stringValue(t, loaded.Issues["34"].Extra["customField"]))
}

func TestSavePreservesUnknownIssueFields(t *testing.T) {
	stateDir := t.TempDir()
	store := New(stateDir)
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(`{
  "issues": {
    "34": {
      "attempts": 1,
      "customField": "keep me"
    }
  },
  "workers": {}
}`), 0o644))

	loaded, err := store.Load()
	require.NoError(t, err)
	issue := loaded.Issues["34"]
	issue.Attempts = 2
	issue.WorkflowStatus = domain.IssueWorkflowStatusAwaitingVerify
	issue.Repositories = map[string]domain.IssueRepoState{
		"ml-svc-zcode": {
			Repo:                "ml-svc-zcode",
			IssueBranch:         "cnb/issue-34/ml-svc-zcode",
			BaselineBranch:      "ai-v2.0",
			BaselineMergeCommit: "abc123",
		},
	}
	loaded.Issues["34"] = issue
	require.NoError(t, store.Save(loaded))

	raw, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	require.NoError(t, err)
	require.JSONEq(t, `{
  "version": 1,
  "workers": {},
  "issues": {
    "34": {
      "attempts": 2,
      "workflowStatus": "AWAITING_VERIFY",
      "repositories": {
        "ml-svc-zcode": {
          "repo": "ml-svc-zcode",
          "issueBranch": "cnb/issue-34/ml-svc-zcode",
          "baselineBranch": "ai-v2.0",
          "baselineMergeCommit": "abc123"
        }
      },
      "customField": "keep me"
    }
  }
}`, string(raw))
}

func TestWithLockLoadsMutatesAndSaves(t *testing.T) {
	stateDir := t.TempDir()
	store := New(stateDir)

	err := store.WithLock(context.Background(), func(st *domain.State) error {
		st.Issues["9"] = domain.IssueState{Attempts: 1}
		return nil
	})
	require.NoError(t, err)

	loaded, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, 1, loaded.Issues["9"].Attempts)
	require.NoFileExists(t, filepath.Join(stateDir, "state.tmp"))
}

func stringValue(t *testing.T, raw json.RawMessage) string {
	t.Helper()

	var value string
	require.NoError(t, json.Unmarshal(raw, &value))
	return value
}
