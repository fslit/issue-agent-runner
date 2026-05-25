package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"issue-agent-runner/internal/domain"
)

type Summary struct {
	UpdatedAt        string        `json:"updatedAt"`
	OpenIssues       int           `json:"openIssues"`
	ActiveWorkers    []SummaryItem `json:"activeWorkers"`
	IntegrationQueue []SummaryItem `json:"integrationQueue"`
	AwaitingVerify   []SummaryItem `json:"awaitingVerify"`
	MergeConflicts   []SummaryItem `json:"mergeConflicts"`
	CleanedIssues    []SummaryItem `json:"cleanedIssues"`
	WaitingRetry     []SummaryItem `json:"waitingRetry"`
	PausedIssues     []SummaryItem `json:"pausedIssues"`
}

type SummaryItem struct {
	Number     string `json:"number"`
	Title      string `json:"title"`
	Attempts   int    `json:"attempts,omitempty"`
	RunningFor int64  `json:"runningFor,omitempty"`
	RetryIn    int64  `json:"retryIn,omitempty"`
	LastError  string `json:"lastError,omitempty"`
}

func RenderMarkdown(issues []domain.Issue, state domain.State, now time.Time) string {
	summary := BuildSummary(issues, state, now)

	lines := []string{
		"# CNB Issue Runner Progress",
		"",
		"Updated: " + now.Format("2006-01-02 15:04:05 MST"),
		fmt.Sprintf("Open issues: %d", summary.OpenIssues),
		fmt.Sprintf("Active workers: %d", len(summary.ActiveWorkers)),
		fmt.Sprintf("Ready to integrate: %d", len(summary.IntegrationQueue)),
		fmt.Sprintf("Awaiting verify: %d", len(summary.AwaitingVerify)),
		fmt.Sprintf("Merge conflicts: %d", len(summary.MergeConflicts)),
		fmt.Sprintf("Cleaned issues: %d", len(summary.CleanedIssues)),
		fmt.Sprintf("Retry backoff issues: %d", len(summary.WaitingRetry)),
		fmt.Sprintf("Paused issues: %d", len(summary.PausedIssues)),
		"",
		"## Active",
	}
	lines = append(lines, activeLines(summary.ActiveWorkers)...)
	lines = append(lines, "", "## Integration Queue")
	lines = append(lines, genericLines(summary.IntegrationQueue)...)
	lines = append(lines, "", "## Awaiting Verify")
	lines = append(lines, genericLines(summary.AwaitingVerify)...)
	lines = append(lines, "", "## Merge Conflicts")
	lines = append(lines, genericLines(summary.MergeConflicts)...)
	lines = append(lines, "", "## Cleaned")
	lines = append(lines, genericLines(summary.CleanedIssues)...)
	lines = append(lines, "", "## Waiting For Retry")
	lines = append(lines, retryLines(summary.WaitingRetry)...)
	lines = append(lines, "", "## Paused")
	lines = append(lines, pausedLines(summary.PausedIssues)...)
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

func RenderJSON(issues []domain.Issue, state domain.State, now time.Time) ([]byte, error) {
	summary := BuildSummary(issues, state, now)
	return json.MarshalIndent(summary, "", "  ")
}

func BuildSummary(issues []domain.Issue, state domain.State, now time.Time) Summary {
	state.Normalize()
	issueTitles := map[string]string{}
	for _, issue := range issues {
		issueTitles[issue.Number] = issue.Title
	}

	active := make([]SummaryItem, 0, len(state.Workers))
	for number, worker := range state.Workers {
		title := worker.Title
		if title == "" {
			title = issueTitles[number]
		}
		active = append(active, SummaryItem{
			Number:     number,
			Title:      title,
			RunningFor: maxInt64(0, now.Unix()-worker.StartedAt),
		})
	}

	integrationQueue := []SummaryItem{}
	awaitingVerify := []SummaryItem{}
	mergeConflicts := []SummaryItem{}
	cleaned := []SummaryItem{}
	waiting := []SummaryItem{}
	paused := []SummaryItem{}
	for number, meta := range state.Issues {
		title := meta.Title
		if title == "" {
			title = issueTitles[number]
		}
		switch meta.WorkflowStatus {
		case domain.IssueWorkflowStatusReadyToIntegrate:
			integrationQueue = append(integrationQueue, SummaryItem{Number: number, Title: title})
			continue
		case domain.IssueWorkflowStatusAwaitingVerify:
			awaitingVerify = append(awaitingVerify, SummaryItem{Number: number, Title: title})
			continue
		case domain.IssueWorkflowStatusMergeConflict:
			mergeConflicts = append(mergeConflicts, SummaryItem{Number: number, Title: title, Attempts: meta.Attempts, LastError: meta.PauseReason})
			continue
		case domain.IssueWorkflowStatusCleaned:
			cleaned = append(cleaned, SummaryItem{Number: number, Title: title})
			continue
		}
		if meta.Paused {
			paused = append(paused, SummaryItem{
				Number:    number,
				Title:     title,
				Attempts:  meta.Attempts,
				LastError: meta.LastError,
			})
			continue
		}
		if meta.NextRetryAt > now.Unix() {
			waiting = append(waiting, SummaryItem{
				Number:   number,
				Title:    title,
				Attempts: meta.Attempts,
				RetryIn:  meta.NextRetryAt - now.Unix(),
			})
		}
	}

	sortItems(active)
	sortItems(integrationQueue)
	sortItems(awaitingVerify)
	sortItems(mergeConflicts)
	sortItems(cleaned)
	sortItems(waiting)
	sortItems(paused)
	return Summary{
		UpdatedAt:        now.Format(time.RFC3339),
		OpenIssues:       len(issues),
		ActiveWorkers:    active,
		IntegrationQueue: integrationQueue,
		AwaitingVerify:   awaitingVerify,
		MergeConflicts:   mergeConflicts,
		CleanedIssues:    cleaned,
		WaitingRetry:     waiting,
		PausedIssues:     paused,
	}
}

func activeLines(items []SummaryItem) []string {
	if len(items) == 0 {
		return []string{"- none"}
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("- #%s %s running_for=%ds", item.Number, item.Title, item.RunningFor))
	}
	return lines
}

func retryLines(items []SummaryItem) []string {
	if len(items) == 0 {
		return []string{"- none"}
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("- #%s retry_in=%ds attempts=%d %s", item.Number, item.RetryIn, item.Attempts, item.Title))
	}
	return lines
}

func genericLines(items []SummaryItem) []string {
	if len(items) == 0 {
		return []string{"- none"}
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("- #%s %s", item.Number, item.Title))
	}
	return lines
}

func pausedLines(items []SummaryItem) []string {
	if len(items) == 0 {
		return []string{"- none"}
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("- #%s attempts=%d %s error=%s", item.Number, item.Attempts, item.Title, item.LastError))
	}
	return lines
}

func sortItems(items []SummaryItem) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].Number < items[j].Number
	})
}

func maxInt64(left int64, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
