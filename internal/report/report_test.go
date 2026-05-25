package report

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/domain"
)

func TestRenderMarkdownIncludesActiveRetryAndPaused(t *testing.T) {
	now := time.Unix(1000, 0)
	state := domain.State{
		Workers: map[string]domain.WorkerState{
			"7": {Title: "running issue", StartedAt: 900},
		},
		Issues: map[string]domain.IssueState{
			"8":  {Attempts: 1, NextRetryAt: 1300, Title: "waiting issue"},
			"9":  {Attempts: 3, Paused: true, LastError: "failed", Title: "paused issue"},
			"10": {WorkflowStatus: domain.IssueWorkflowStatusReadyToIntegrate, Title: "queued issue"},
			"11": {WorkflowStatus: domain.IssueWorkflowStatusAwaitingVerify, Title: "verify issue"},
			"12": {WorkflowStatus: domain.IssueWorkflowStatusMergeConflict, PauseReason: "merge conflict", Title: "conflict issue"},
			"13": {WorkflowStatus: domain.IssueWorkflowStatusCleaned, Title: "cleaned issue"},
		},
	}
	state.Normalize()

	markdown := RenderMarkdown([]domain.Issue{{Number: "7"}, {Number: "8"}, {Number: "9"}, {Number: "10"}, {Number: "11"}, {Number: "12"}, {Number: "13"}}, state, now)

	require.Contains(t, markdown, "Open issues: 7")
	require.Contains(t, markdown, "Active workers: 1")
	require.Contains(t, markdown, "Ready to integrate: 1")
	require.Contains(t, markdown, "Awaiting verify: 1")
	require.Contains(t, markdown, "Merge conflicts: 1")
	require.Contains(t, markdown, "Cleaned issues: 1")
	require.Contains(t, markdown, "#7 running issue running_for=100s")
	require.Contains(t, markdown, "#10 queued issue")
	require.Contains(t, markdown, "#11 verify issue")
	require.Contains(t, markdown, "#12 conflict issue")
	require.Contains(t, markdown, "#13 cleaned issue")
	require.Contains(t, markdown, "#8 retry_in=300s attempts=1 waiting issue")
	require.Contains(t, markdown, "#9 attempts=3 paused issue error=failed")
}

func TestRenderJSONHasScriptFriendlyShape(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	state := domain.State{
		Workers: map[string]domain.WorkerState{"7": {Title: "running issue", StartedAt: 900}},
		Issues:  map[string]domain.IssueState{"9": {Paused: true, Attempts: 3, Title: "paused issue"}},
	}
	state.Normalize()

	raw, err := RenderJSON([]domain.Issue{{Number: "7"}, {Number: "9"}}, state, now)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, float64(2), payload["openIssues"])
	require.Len(t, payload["activeWorkers"], 1)
	require.Len(t, payload["integrationQueue"], 0)
	require.Len(t, payload["awaitingVerify"], 0)
	require.Len(t, payload["pausedIssues"], 1)
}
