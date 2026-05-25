package runner

import (
	"strconv"
	"strings"
	"time"

	"issue-agent-runner/internal/config"
	"issue-agent-runner/internal/domain"
)

func IsCandidate(issue domain.Issue, state domain.State, labels config.LabelConfig, now time.Time) bool {
	if issue.State != "" && issue.State != "open" {
		return false
	}
	if _, active := state.Workers[issue.Number]; active {
		return false
	}

	meta := state.Issues[issue.Number]
	if meta.Paused || meta.NextRetryAt > now.Unix() {
		return false
	}
	orphanRunningRetry := meta.WorkflowStatus == domain.IssueWorkflowStatusRunning && meta.Attempts > 0
	switch meta.WorkflowStatus {
	case domain.IssueWorkflowStatusRunning:
		if !orphanRunningRetry {
			return false
		}
	case domain.IssueWorkflowStatusReadyToIntegrate,
		domain.IssueWorkflowStatusAwaitingVerify,
		domain.IssueWorkflowStatusMergeConflict,
		domain.IssueWorkflowStatusClosedPushed,
		domain.IssueWorkflowStatusCleaned:
		return false
	}

	issueLabels := labelSet(issue.Labels)
	skipLabels := labelSet(labels.Skip)
	if !hasAnyLabel(issueLabels, skipLabels) {
		return true
	}

	if issueLabels[labels.Running] && meta.Attempts > 0 && (meta.WorkflowStatus == domain.IssueWorkflowStatusUnknown || orphanRunningRetry) {
		return true
	}
	return false
}

func ClassifyError(message string, exitCode *int) domain.ErrorKind {
	if exitCode != nil && *exitCode == 0 {
		return domain.ErrorNone
	}

	lowered := strings.ToLower(message)
	switch {
	case strings.Contains(lowered, "model is at capacity"),
		strings.Contains(lowered, "selected model is at capacity"):
		return domain.ErrorModelCapacity
	case strings.Contains(lowered, "quota"),
		strings.Contains(lowered, "rate limit"),
		strings.Contains(lowered, "429"):
		return domain.ErrorQuota
	case strings.Contains(lowered, "unauthorized"),
		strings.Contains(lowered, "forbidden"),
		strings.Contains(lowered, "authentication"),
		strings.Contains(lowered, "401"),
		strings.Contains(lowered, "403"):
		return domain.ErrorAuth
	case strings.Contains(lowered, "stale"):
		return domain.ErrorStale
	case strings.Contains(lowered, "timeout"):
		return domain.ErrorTimeout
	case strings.Contains(lowered, "test failed"),
		strings.Contains(lowered, "go test"),
		strings.Contains(lowered, "npm test"),
		strings.Contains(lowered, "pytest"),
		strings.Contains(message, "FAIL"):
		return domain.ErrorTestFailure
	default:
		return domain.ErrorUnknown
	}
}

func ApplyFailure(issueNumber string, worker domain.WorkerState, result domain.WorkerResult, now time.Time, cfg config.RunnerConfig, state *domain.State) {
	state.Normalize()

	meta := state.Issues[issueNumber]
	attempts := meta.Attempts + 1
	errorMessage := result.ErrorMessage
	if errorMessage == "" {
		errorMessage = "worker exited without success"
		if result.ExitCode != nil {
			errorMessage = "worker exited without success code"
		}
	}

	kind := result.ErrorKind
	if kind == "" || kind == domain.ErrorNone {
		kind = ClassifyError(errorMessage, result.ExitCode)
	}

	meta.Attempts = attempts
	meta.LastExitCode = result.ExitCode
	meta.LastError = errorMessage
	meta.ErrorKind = kind
	meta.LastFinishedAt = now.Unix()
	meta.NextRetryAt = now.Unix() + backoffSeconds(kind, cfg)
	meta.Title = worker.Title
	meta.WorkflowStatus = domain.IssueWorkflowStatusUnknown

	if kind == domain.ErrorQuota || kind == domain.ErrorAuth {
		meta.Paused = true
		meta.PauseReason = errorMessage
	} else if attempts >= cfg.MaxAttempts {
		meta.Paused = true
		meta.PauseReason = "failed " + strconv.Itoa(attempts) + " attempts"
	}

	state.Issues[issueNumber] = meta
}

func backoffSeconds(kind domain.ErrorKind, cfg config.RunnerConfig) int64 {
	switch kind {
	case domain.ErrorModelCapacity:
		return int64(cfg.CapacityBackoffSeconds)
	case domain.ErrorQuota:
		return int64(cfg.QuotaBackoffSeconds)
	default:
		return int64(cfg.RetryBackoffSeconds)
	}
}

func labelSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		if label != "" {
			set[label] = true
		}
	}
	return set
}

func hasAnyLabel(left map[string]bool, right map[string]bool) bool {
	for label := range left {
		if right[label] {
			return true
		}
	}
	return false
}
