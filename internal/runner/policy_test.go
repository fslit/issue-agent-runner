package runner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/config"
	"issue-agent-runner/internal/domain"
)

func TestIsCandidateFiltersIssueStateAndLabels(t *testing.T) {
	now := time.Unix(500, 0)
	labels := testLabels()
	state := domain.State{
		Workers: map[string]domain.WorkerState{
			"2": {PID: 2002},
		},
		Issues: map[string]domain.IssueState{
			"3": {Paused: true},
			"4": {NextRetryAt: 600},
		},
	}
	state.Normalize()

	require.True(t, IsCandidate(domain.Issue{Number: "1", State: "open"}, state, labels, now))
	require.False(t, IsCandidate(domain.Issue{Number: "1", State: "closed"}, state, labels, now))
	require.False(t, IsCandidate(domain.Issue{Number: "2", State: "open"}, state, labels, now))
	require.False(t, IsCandidate(domain.Issue{Number: "3", State: "open"}, state, labels, now))
	require.False(t, IsCandidate(domain.Issue{Number: "4", State: "open"}, state, labels, now))
	require.False(t, IsCandidate(domain.Issue{Number: "5", State: "open", Labels: []string{"已处理"}}, state, labels, now))
}

func TestIsCandidateAllowsOrphanRunningLabelWhenRetryReady(t *testing.T) {
	now := time.Unix(500, 0)
	labels := testLabels()
	state := domain.State{
		Workers: map[string]domain.WorkerState{},
		Issues: map[string]domain.IssueState{
			"7": {Attempts: 1, NextRetryAt: 400, WorkflowStatus: domain.IssueWorkflowStatusRunning},
			"8": {Attempts: 1, NextRetryAt: 600, WorkflowStatus: domain.IssueWorkflowStatusRunning},
		},
	}
	state.Normalize()

	require.True(t, IsCandidate(domain.Issue{Number: "7", State: "open", Labels: []string{"处理中"}}, state, labels, now))
	require.False(t, IsCandidate(domain.Issue{Number: "8", State: "open", Labels: []string{"处理中"}}, state, labels, now))
}

func TestClassifyError(t *testing.T) {
	one := 1
	zero := 0

	require.Equal(t, domain.ErrorNone, ClassifyError("", &zero))
	require.Equal(t, domain.ErrorModelCapacity, ClassifyError("ERROR: Selected model is at capacity. Please try later.", &one))
	require.Equal(t, domain.ErrorQuota, ClassifyError("429 quota exceeded", &one))
	require.Equal(t, domain.ErrorAuth, ClassifyError("401 unauthorized", &one))
	require.Equal(t, domain.ErrorStale, ClassifyError("worker log stale; terminated for retry", nil))
	require.Equal(t, domain.ErrorTimeout, ClassifyError("worker hard timeout; terminated for retry", nil))
	require.Equal(t, domain.ErrorTestFailure, ClassifyError("go test ./... FAIL", &one))
	require.Equal(t, domain.ErrorUnknown, ClassifyError("unexpected failure", &one))
}

func TestApplyFailureBackoffAndPause(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := config.RunnerConfig{
		MaxAttempts:            3,
		RetryBackoffSeconds:    300,
		CapacityBackoffSeconds: 900,
		QuotaBackoffSeconds:    3600,
	}
	exitCode := 1
	state := domain.State{Workers: map[string]domain.WorkerState{}, Issues: map[string]domain.IssueState{}}
	state.Normalize()

	state.Issues["10"] = domain.IssueState{WorkflowStatus: domain.IssueWorkflowStatusRunning}
	ApplyFailure("10", domain.WorkerState{Title: "capacity"}, domain.WorkerResult{
		ExitCode:     &exitCode,
		ErrorMessage: "Selected model is at capacity",
	}, now, cfg, &state)

	meta := state.Issues["10"]
	require.Equal(t, 1, meta.Attempts)
	require.Equal(t, domain.ErrorModelCapacity, meta.ErrorKind)
	require.Equal(t, int64(1900), meta.NextRetryAt)
	require.False(t, meta.Paused)
	require.Equal(t, domain.IssueWorkflowStatusUnknown, meta.WorkflowStatus)

	state.Issues["11"] = domain.IssueState{Attempts: 2}
	ApplyFailure("11", domain.WorkerState{Title: "unknown"}, domain.WorkerResult{
		ExitCode:     &exitCode,
		ErrorMessage: "unexpected failure",
	}, now, cfg, &state)

	meta = state.Issues["11"]
	require.Equal(t, 3, meta.Attempts)
	require.True(t, meta.Paused)
	require.Equal(t, "failed 3 attempts", meta.PauseReason)

	ApplyFailure("12", domain.WorkerState{Title: "quota"}, domain.WorkerResult{
		ExitCode:     &exitCode,
		ErrorMessage: "quota exceeded",
	}, now, cfg, &state)

	meta = state.Issues["12"]
	require.Equal(t, domain.ErrorQuota, meta.ErrorKind)
	require.True(t, meta.Paused)
	require.Equal(t, "quota exceeded", meta.PauseReason)
}

func testLabels() config.LabelConfig {
	return config.LabelConfig{
		Running:    "处理中",
		Done:       "已处理",
		Failed:     "处理失败",
		NeedsHuman: "需人工确认",
		Skip:       []string{"已处理", "处理中", "处理失败", "需人工确认"},
	}
}
