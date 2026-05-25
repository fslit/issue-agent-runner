package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/config"
	"issue-agent-runner/internal/domain"
	"issue-agent-runner/internal/gitops"
	"issue-agent-runner/internal/state"
)

func TestOnceClaimsCandidateStartsWorkerAndWritesReports(t *testing.T) {
	env := newRunnerTestEnv(t)
	env.provider.issues = []domain.Issue{{Number: "7", Title: "fix issue", State: "open"}}
	env.executor.startWorker = domain.WorkerState{IssueNumber: "7", PID: 7007, Title: "fix issue", StartedAt: env.now.Unix(), LogPath: filepath.Join(env.stateDir, "issue-7.log")}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.Contains(t, loaded.Workers, "7")
	require.Equal(t, domain.IssueWorkflowStatusRunning, loaded.Issues["7"].WorkflowStatus)
	require.Equal(t, "gpt-5.5", loaded.Issues["7"].AgentModel)
	require.Equal(t, "xhigh", loaded.Issues["7"].AgentReasoningEffort)
	require.Equal(t, "cnb/issue-7/ml-svc-zcode", loaded.Issues["7"].Repositories["ml-svc-zcode"].IssueBranch)
	require.Equal(t, []labelCall{{number: "7", labels: []string{"处理中"}}}, env.provider.addedLabels)
	require.Equal(t, 1, env.executor.started)
	require.Equal(t, []string{"7:ml-svc-zcode", "7:ml-web-zcode"}, env.gitOps.prepared)
	require.FileExists(t, filepath.Join(env.stateDir, "progress-summary.md"))
	require.FileExists(t, filepath.Join(env.stateDir, "progress-summary.json"))
	require.FileExists(t, filepath.Join(env.stateDir, "runner.log"))
	entries := readRunnerLogEntries(t, filepath.Join(env.stateDir, "runner.log"))
	started := requireRunnerLogEvent(t, entries, "worker_started")
	require.Equal(t, "7", started["issue"])
	require.EqualValues(t, 7007, started["pid"])
	require.EqualValues(t, 2, started["repos"])
}

func TestOnceRunsClaimAndProgressHooks(t *testing.T) {
	env := newRunnerTestEnv(t)
	claimHookPath := filepath.Join(env.stateDir, "claim-hook")
	progressHookPath := filepath.Join(env.stateDir, "progress-hook")
	env.cfg.Hooks.OnIssueClaim.Command = "printf claim:$ISSUE_AGENT_ISSUE_NUMBER:$ISSUE_AGENT_WORKFLOW_STATUS > " + strconv.Quote(claimHookPath)
	env.cfg.Hooks.OnProgressUpdate.Command = "printf progress:$ISSUE_AGENT_EVENT > " + strconv.Quote(progressHookPath)
	env.provider.issues = []domain.Issue{{Number: "7", Title: "fix issue", State: "open"}}
	env.executor.startWorker = domain.WorkerState{IssueNumber: "7", PID: 7007, Title: "fix issue", StartedAt: env.now.Unix()}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	claimOutput, err := os.ReadFile(claimHookPath)
	require.NoError(t, err)
	require.Equal(t, "claim:7:RUNNING", string(claimOutput))
	progressOutput, err := os.ReadFile(progressHookPath)
	require.NoError(t, err)
	require.Equal(t, "progress:onProgressUpdate", string(progressOutput))
}

func TestOnceRespectsActiveWorkerCapacity(t *testing.T) {
	env := newRunnerTestEnv(t)
	env.cfg.Runner.MaxConcurrency = 1
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{
			"6": {IssueNumber: "6", PID: 6006, Title: "active", StartedAt: env.now.Unix()},
		},
		Issues: map[string]domain.IssueState{},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "candidate", State: "open"}}
	env.executor.alive = map[string]bool{"6": true}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	require.Equal(t, 0, env.executor.started)
	require.Empty(t, env.provider.addedLabels)
}

func TestOnceRecordsFinishedWorkerSuccess(t *testing.T) {
	env := newRunnerTestEnv(t)
	zero := 0
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{
			"7": {IssueNumber: "7", PID: 7007, Title: "done", StartedAt: env.now.Add(-time.Minute).Unix()},
		},
		Issues: map[string]domain.IssueState{},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "done", State: "open", Labels: []string{"处理中"}}}
	env.executor.results = map[string]domain.WorkerResult{
		"7": {
			ExitCode:     &zero,
			Status:       "success",
			Summary:      "fixed",
			Verification: []string{"rtk go test ./..."},
			RepoResults: []domain.RepoWorkerResult{
				{
					Repo:           "ml-svc-zcode",
					RepoPath:       filepath.Join(env.stateDir, "ml-svc-zcode"),
					WorktreePath:   filepath.Join(env.stateDir, ".local_worktrees", "issue-7", "ml-svc-zcode"),
					IssueBranch:    "cnb/issue-7/ml-svc-zcode",
					BaselineBranch: "ai-v2.0",
					HeadSHA:        "head-7",
					CommitSHA:      "commit-7",
				},
			},
		},
	}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.NotContains(t, loaded.Workers, "7")
	require.Equal(t, &zero, loaded.Issues["7"].LastExitCode)
	require.Equal(t, domain.IssueWorkflowStatusAwaitingVerify, loaded.Issues["7"].WorkflowStatus)
	require.Equal(t, "head-7", loaded.Issues["7"].Repositories["ml-svc-zcode"].IssueHeadSHA)
	require.Equal(t, "commit-7", loaded.Issues["7"].Repositories["ml-svc-zcode"].CommitSHA)
	require.Equal(t, "merge-ml-svc-zcode", loaded.Issues["7"].Repositories["ml-svc-zcode"].BaselineMergeCommit)
	require.Empty(t, env.provider.removedLabels)
	require.Len(t, env.provider.comments, 1)
	require.Contains(t, env.provider.comments[0], "Model: `gpt-5.5` (reasoning: `xhigh`)")
}

func TestOnceRecordsFinishedWorkerFailureAndClearsRunningStatus(t *testing.T) {
	env := newRunnerTestEnv(t)
	exitCode := 2
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{
			"7": {IssueNumber: "7", PID: 7007, Title: "failed", StartedAt: env.now.Add(-time.Minute).Unix()},
		},
		Issues: map[string]domain.IssueState{
			"7": {WorkflowStatus: domain.IssueWorkflowStatusRunning},
		},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "failed", State: "open", Labels: []string{"处理中"}}}
	env.executor.results = map[string]domain.WorkerResult{
		"7": {
			ExitCode:     &exitCode,
			ErrorMessage: "error: unexpected argument '--ask-for-approval' found",
		},
	}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.NotContains(t, loaded.Workers, "7")
	meta := loaded.Issues["7"]
	require.Equal(t, 1, meta.Attempts)
	require.Equal(t, &exitCode, meta.LastExitCode)
	require.Contains(t, meta.LastError, "unexpected argument")
	require.Equal(t, domain.IssueWorkflowStatusUnknown, meta.WorkflowStatus)
	require.Equal(t, int64(1300), meta.NextRetryAt)

	entries := readRunnerLogEntries(t, filepath.Join(env.stateDir, "runner.log"))
	failed := requireRunnerLogEvent(t, entries, "worker_failed")
	require.Equal(t, "7", failed["issue"])
	require.Equal(t, "UNKNOWN", failed["errorKind"])
	require.EqualValues(t, 2, failed["exitCode"])
}

func TestOnceRunsPausedHookWhenWorkerFailurePausesIssue(t *testing.T) {
	env := newRunnerTestEnv(t)
	pausedHookPath := filepath.Join(env.stateDir, "paused-hook")
	env.cfg.Hooks.OnIssuePaused.Command = "printf paused:$ISSUE_AGENT_ISSUE_NUMBER:$ISSUE_AGENT_WORKFLOW_STATUS > " + strconv.Quote(pausedHookPath)
	exitCode := 1
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{
			"7": {IssueNumber: "7", PID: 7007, Title: "quota", StartedAt: env.now.Add(-time.Minute).Unix()},
		},
		Issues: map[string]domain.IssueState{
			"7": {WorkflowStatus: domain.IssueWorkflowStatusRunning},
		},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "quota", State: "open", Labels: []string{"处理中"}}}
	env.executor.results = map[string]domain.WorkerResult{
		"7": {
			ExitCode:     &exitCode,
			ErrorMessage: "429 quota exceeded",
		},
	}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	pausedOutput, err := os.ReadFile(pausedHookPath)
	require.NoError(t, err)
	require.Equal(t, "paused:7:", string(pausedOutput))
}

func TestOnceTerminatesHardTimedOutWorker(t *testing.T) {
	env := newRunnerTestEnv(t)
	env.cfg.Runner.HardWorkerSeconds = 600
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{
			"7": {IssueNumber: "7", PID: 7007, Title: "too long", StartedAt: env.now.Add(-time.Hour).Unix()},
		},
		Issues: map[string]domain.IssueState{
			"7": {WorkflowStatus: domain.IssueWorkflowStatusRunning},
		},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "too long", State: "open", Labels: []string{"处理中"}}}
	env.executor.alive = map[string]bool{"7": true}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.NotContains(t, loaded.Workers, "7")
	require.Equal(t, []string{"7"}, env.executor.terminated)
	meta := loaded.Issues["7"]
	require.Equal(t, domain.ErrorTimeout, meta.ErrorKind)
	require.Contains(t, meta.LastError, "hard timeout")
	require.Equal(t, int64(1300), meta.NextRetryAt)
}

func TestWorkerSucceededRequiresStructuredSuccessStatus(t *testing.T) {
	zero := 0

	require.False(t, workerSucceeded(domain.WorkerResult{ExitCode: &zero}))
	require.True(t, workerSucceeded(domain.WorkerResult{Status: "success", ExitCode: &zero}))
}

func TestOnceReleasesRemoteDoneWorker(t *testing.T) {
	env := newRunnerTestEnv(t)
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{
			"7": {IssueNumber: "7", PID: 7007, Title: "already done", StartedAt: env.now.Unix()},
		},
		Issues: map[string]domain.IssueState{},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "already done", State: "open", Labels: []string{"已处理"}}}
	env.executor.alive = map[string]bool{"7": true}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.NotContains(t, loaded.Workers, "7")
	require.Equal(t, []string{"7"}, env.executor.terminated)
}

func TestOnceIntegratesReadyIssueAndMarksAwaitingVerify(t *testing.T) {
	env := newRunnerTestEnv(t)
	integratedHookPath := filepath.Join(env.stateDir, "integrated-hook")
	env.cfg.Hooks.OnIssueIntegrated.Command = "printf integrated:$ISSUE_AGENT_ISSUE_NUMBER > " + strconv.Quote(integratedHookPath)
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{},
		Issues: map[string]domain.IssueState{
			"7": {
				Title:          "ready issue",
				WorkflowStatus: domain.IssueWorkflowStatusReadyToIntegrate,
				ReadyAt:        env.now.Unix() - 10,
				Result: &domain.WorkerResult{
					Summary:      "fixed",
					Verification: []string{"rtk go test ./..."},
				},
				Repositories: map[string]domain.IssueRepoState{
					"ml-svc-zcode": {
						Repo:           "ml-svc-zcode",
						RepoPath:       filepath.Join(env.stateDir, "ml-svc-zcode"),
						IssueBranch:    "cnb/issue-7/ml-svc-zcode",
						BaselineBranch: "ai-v2.0",
						CommitSHA:      "commit-7",
					},
				},
			},
		},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "ready issue", State: "open", Labels: []string{"处理中"}}}
	env.gitOps.mergeResults = map[string]gitops.MergeResult{
		"ml-svc-zcode": {MergeCommit: "merge-7"},
	}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.Equal(t, domain.IssueWorkflowStatusAwaitingVerify, loaded.Issues["7"].WorkflowStatus)
	require.Equal(t, "merge-7", loaded.Issues["7"].Repositories["ml-svc-zcode"].BaselineMergeCommit)
	require.Equal(t, []string{"ml-svc-zcode:cnb/issue-7/ml-svc-zcode"}, env.gitOps.merged)
	require.Len(t, env.provider.comments, 1)
	require.Contains(t, env.provider.comments[0], "已合入本地验证环境")
	require.Contains(t, env.provider.comments[0], "Model: `gpt-5.5` (reasoning: `xhigh`)")
	currentValidationMarkdown := filepath.Join(env.stateDir, "current-validation.md")
	currentValidationJSON := filepath.Join(env.stateDir, "current-validation.json")
	require.FileExists(t, currentValidationMarkdown)
	require.FileExists(t, currentValidationJSON)
	markdown, err := os.ReadFile(currentValidationMarkdown)
	require.NoError(t, err)
	require.Contains(t, string(markdown), "#7 ready issue")
	require.Contains(t, string(markdown), "`ml-svc-zcode` -> `ai-v2.0` (merge-7)")
	var current map[string]any
	raw, err := os.ReadFile(currentValidationJSON)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &current))
	require.Equal(t, "7", current["number"])
	require.Equal(t, "ready issue", current["title"])
	hookOutput, err := os.ReadFile(integratedHookPath)
	require.NoError(t, err)
	require.Equal(t, "integrated:7", string(hookOutput))
}

func TestOnceContinuesWhenIntegratedHookFails(t *testing.T) {
	env := newRunnerTestEnv(t)
	env.cfg.Hooks.OnIssueIntegrated.Command = "printf hook-boom; exit 42"
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{},
		Issues: map[string]domain.IssueState{
			"7": {
				Title:          "ready issue",
				WorkflowStatus: domain.IssueWorkflowStatusReadyToIntegrate,
				ReadyAt:        env.now.Unix() - 10,
				Result: &domain.WorkerResult{
					Summary:      "fixed",
					Verification: []string{"rtk go test ./..."},
				},
				Repositories: map[string]domain.IssueRepoState{
					"ml-svc-zcode": {
						Repo:           "ml-svc-zcode",
						RepoPath:       filepath.Join(env.stateDir, "ml-svc-zcode"),
						IssueBranch:    "cnb/issue-7/ml-svc-zcode",
						BaselineBranch: "ai-v2.0",
						CommitSHA:      "commit-7",
					},
				},
			},
		},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "ready issue", State: "open", Labels: []string{"处理中"}}}
	env.gitOps.mergeResults = map[string]gitops.MergeResult{
		"ml-svc-zcode": {MergeCommit: "merge-7"},
	}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.Equal(t, domain.IssueWorkflowStatusAwaitingVerify, loaded.Issues["7"].WorkflowStatus)
	entries := readRunnerLogEntries(t, filepath.Join(env.stateDir, "runner.log"))
	failed := requireRunnerLogEvent(t, entries, "hook_failed")
	require.Equal(t, "onIssueIntegrated", failed["hook"])
	require.Equal(t, "7", failed["issue"])
	require.Contains(t, failed["error"], "exit status 42")
	require.Contains(t, failed["output"], "hook-boom")
}

func TestRunHookPostsConfiguredWebhook(t *testing.T) {
	env := newRunnerTestEnv(t)
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "application/json", req.Header.Get("Content-Type"))
		require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	env.cfg.Hooks.OnIssueIntegrated.Webhook.URL = server.URL
	env.cfg.Hooks.OnIssueIntegrated.Webhook.Method = http.MethodPost

	env.runner.runHook(context.Background(), "onIssueIntegrated", env.cfg.Hooks.OnIssueIntegrated, domain.Issue{Number: "7", Title: "ready"}, domain.IssueState{
		WorkflowStatus:       domain.IssueWorkflowStatusAwaitingVerify,
		AgentModel:           "gpt-5.5",
		AgentReasoningEffort: "xhigh",
	})

	require.Equal(t, "onIssueIntegrated", payload["event"])
	require.Equal(t, "7", payload["issueNumber"])
	require.Equal(t, "ready", payload["issueTitle"])
	require.Equal(t, "AWAITING_VERIFY", payload["workflowStatus"])
	require.Equal(t, "gpt-5.5", payload["model"])
	require.Equal(t, "xhigh", payload["reasoningEffort"])
	entries := readRunnerLogEntries(t, filepath.Join(env.stateDir, "runner.log"))
	succeeded := requireRunnerLogEvent(t, entries, "webhook_succeeded")
	require.Equal(t, "onIssueIntegrated", succeeded["hook"])
	require.EqualValues(t, http.StatusAccepted, succeeded["statusCode"])
}

func TestRunHookLogsWebhookFailure(t *testing.T) {
	env := newRunnerTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	env.cfg.Hooks.OnIssueIntegrated.Webhook.URL = server.URL

	env.runner.runHook(context.Background(), "onIssueIntegrated", env.cfg.Hooks.OnIssueIntegrated, domain.Issue{Number: "7"}, domain.IssueState{})

	entries := readRunnerLogEntries(t, filepath.Join(env.stateDir, "runner.log"))
	failed := requireRunnerLogEvent(t, entries, "webhook_failed")
	require.Equal(t, "onIssueIntegrated", failed["hook"])
	require.Equal(t, "7", failed["issue"])
	require.EqualValues(t, http.StatusInternalServerError, failed["statusCode"])
}

func TestOnceMarksNeedsHumanOnMergeConflict(t *testing.T) {
	env := newRunnerTestEnv(t)
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{},
		Issues: map[string]domain.IssueState{
			"7": {
				Title:          "conflict issue",
				WorkflowStatus: domain.IssueWorkflowStatusReadyToIntegrate,
				ReadyAt:        env.now.Unix() - 10,
				Repositories: map[string]domain.IssueRepoState{
					"ml-svc-zcode": {
						Repo:           "ml-svc-zcode",
						RepoPath:       filepath.Join(env.stateDir, "ml-svc-zcode"),
						IssueBranch:    "cnb/issue-7/ml-svc-zcode",
						BaselineBranch: "ai-v2.0",
						CommitSHA:      "commit-7",
					},
				},
			},
		},
	}))
	env.provider.issues = []domain.Issue{{Number: "7", Title: "conflict issue", State: "open", Labels: []string{"处理中"}}}
	env.gitOps.mergeErrs = map[string]error{
		"ml-svc-zcode": &gitops.MergeConflictError{
			RepoPath:       filepath.Join(env.stateDir, "ml-svc-zcode"),
			BaselineBranch: "ai-v2.0",
			IssueBranch:    "cnb/issue-7/ml-svc-zcode",
			Files:          []string{"internal/logic/flow/instance.go"},
		},
	}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.Equal(t, domain.IssueWorkflowStatusMergeConflict, loaded.Issues["7"].WorkflowStatus)
	require.True(t, loaded.Issues["7"].Paused)
	require.Equal(t, []string{"internal/logic/flow/instance.go"}, loaded.Issues["7"].Repositories["ml-svc-zcode"].ConflictFiles)
	require.Equal(t, []labelCall{{number: "7", labels: []string{"处理中"}}}, env.provider.removedLabels)
	require.Equal(t, []labelCall{{number: "7", labels: []string{"需人工确认"}}}, env.provider.addedLabels)
	require.Len(t, env.provider.comments, 1)
	require.Contains(t, env.provider.comments[0], "无法自动合入基准分支")
	require.Contains(t, env.provider.comments[0], "Model: `gpt-5.5` (reasoning: `xhigh`)")
}

func TestRejectRevertsAwaitingVerifyIssue(t *testing.T) {
	env := newRunnerTestEnv(t)
	rejectedHookPath := filepath.Join(env.stateDir, "rejected-hook")
	env.cfg.Hooks.OnIssueRejected.Command = "printf rejected:$ISSUE_AGENT_ISSUE_NUMBER > " + strconv.Quote(rejectedHookPath)
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{},
		Issues: map[string]domain.IssueState{
			"7": {
				WorkflowStatus: domain.IssueWorkflowStatusAwaitingVerify,
				Repositories: map[string]domain.IssueRepoState{
					"ml-svc-zcode": {
						Repo:                "ml-svc-zcode",
						RepoPath:            filepath.Join(env.stateDir, "ml-svc-zcode"),
						IssueBranch:         "cnb/issue-7/ml-svc-zcode",
						BaselineBranch:      "ai-v2.0",
						BaselineMergeCommit: "merge-7",
						CommitSHA:           "commit-7",
					},
				},
			},
		},
	}))

	err := env.runner.Reject(context.Background(), "7")
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.Equal(t, domain.IssueWorkflowStatusUnknown, loaded.Issues["7"].WorkflowStatus)
	require.Equal(t, []string{"ml-svc-zcode:merge-7"}, env.gitOps.reverted)
	require.Equal(t, []labelCall{{number: "7", labels: []string{"处理中"}}}, env.provider.removedLabels)
	require.Len(t, env.provider.comments, 1)
	require.Contains(t, env.provider.comments[0], "人工验证未通过")
	require.Contains(t, env.provider.comments[0], "Model: `gpt-5.5` (reasoning: `xhigh`)")
	hookOutput, err := os.ReadFile(rejectedHookPath)
	require.NoError(t, err)
	require.Equal(t, "rejected:7", string(hookOutput))
}

func TestOncePushesClosedAwaitingVerifyIssueAndCleansUp(t *testing.T) {
	env := newRunnerTestEnv(t)
	closedHookPath := filepath.Join(env.stateDir, "closed-hook")
	env.cfg.Hooks.OnIssueClosedPushed.Command = "printf closed:$ISSUE_AGENT_ISSUE_NUMBER > " + strconv.Quote(closedHookPath)
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{},
		Issues: map[string]domain.IssueState{
			"7": {
				WorkflowStatus: domain.IssueWorkflowStatusAwaitingVerify,
				Repositories: map[string]domain.IssueRepoState{
					"ml-svc-zcode": {
						Repo:                "ml-svc-zcode",
						RepoPath:            filepath.Join(env.stateDir, "ml-svc-zcode"),
						WorktreePath:        filepath.Join(env.stateDir, ".local_worktrees", "issue-7", "ml-svc-zcode"),
						IssueBranch:         "cnb/issue-7/ml-svc-zcode",
						BaselineBranch:      "ai-v2.0",
						BaselineMergeCommit: "merge-7",
						CommitSHA:           "commit-7",
					},
				},
			},
		},
	}))
	env.provider.getIssues = map[string]domain.Issue{
		"7": {Number: "7", Title: "closed issue", State: "closed"},
	}

	err := env.runner.Once(context.Background())
	require.NoError(t, err)

	loaded, err := env.store.Load()
	require.NoError(t, err)
	require.Equal(t, domain.IssueWorkflowStatusCleaned, loaded.Issues["7"].WorkflowStatus)
	require.Equal(t, "push-ml-svc-zcode", loaded.Issues["7"].Repositories["ml-svc-zcode"].PushedBaselineSHA)
	require.Equal(t, []string{"ml-svc-zcode:ai-v2.0"}, env.gitOps.pushed)
	require.Equal(t, []string{"ml-svc-zcode:cnb/issue-7/ml-svc-zcode"}, env.gitOps.cleaned)
	require.Len(t, env.provider.comments, 1)
	require.Contains(t, env.provider.comments[0], "基准分支已推送")
	require.Contains(t, env.provider.comments[0], "Model: `gpt-5.5` (reasoning: `xhigh`)")
	require.NoFileExists(t, filepath.Join(env.stateDir, "current-validation.md"))
	require.NoFileExists(t, filepath.Join(env.stateDir, "current-validation.json"))
	hookOutput, err := os.ReadFile(closedHookPath)
	require.NoError(t, err)
	require.Equal(t, "closed:7", string(hookOutput))
}

func TestClearRemoteRunningLabels(t *testing.T) {
	env := newRunnerTestEnv(t)

	err := env.runner.ClearRemoteRunningLabels(context.Background(), []string{"7", "8"})
	require.NoError(t, err)

	require.Equal(t, []labelCall{
		{number: "7", labels: []string{"处理中"}},
		{number: "8", labels: []string{"处理中"}},
	}, env.provider.removedLabels)
}

func TestCurrentValidationMarkdownReturnsCurrentSlot(t *testing.T) {
	env := newRunnerTestEnv(t)
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{},
		Issues: map[string]domain.IssueState{
			"7": {
				Title:          "ready issue",
				WorkflowStatus: domain.IssueWorkflowStatusAwaitingVerify,
				IntegratedAt:   env.now.Unix(),
				Result: &domain.WorkerResult{
					Summary:      "fixed",
					Verification: []string{"rtk go test ./..."},
				},
				Repositories: map[string]domain.IssueRepoState{
					"ml-svc-zcode": {
						Repo:                "ml-svc-zcode",
						IssueBranch:         "cnb/issue-7/ml-svc-zcode",
						BaselineBranch:      "ai-v2.0",
						BaselineMergeCommit: "merge-7",
						CommitSHA:           "commit-7",
					},
				},
			},
		},
	}))

	markdown, err := env.runner.CurrentValidationMarkdown(context.Background())
	require.NoError(t, err)

	require.Contains(t, markdown, "#7 ready issue")
	require.Contains(t, markdown, "`ml-svc-zcode` -> `ai-v2.0` (merge-7)")
	require.Contains(t, markdown, "issue-agent-runner reject 7")
}

func TestCurrentValidationMarkdownReturnsEmptyMessage(t *testing.T) {
	env := newRunnerTestEnv(t)
	require.NoError(t, env.store.Save(&domain.State{
		Version: 1,
		Workers: map[string]domain.WorkerState{},
		Issues:  map[string]domain.IssueState{},
	}))

	markdown, err := env.runner.CurrentValidationMarkdown(context.Background())
	require.NoError(t, err)

	require.Equal(t, "No issue is currently awaiting manual validation.\n", markdown)
}

type runnerTestEnv struct {
	stateDir string
	cfg      *config.Config
	store    *state.Store
	provider *fakeProvider
	executor *fakeExecutor
	gitOps   *fakeGitOps
	runner   *Runner
	now      time.Time
}

func newRunnerTestEnv(t *testing.T) *runnerTestEnv {
	t.Helper()

	stateDir := t.TempDir()
	templatePath := filepath.Join(stateDir, "worker.md")
	require.NoError(t, os.WriteFile(templatePath, []byte("Issue #{{ .Number }} -> {{ .ResultPath }}"), 0o644))
	now := time.Unix(1000, 0)
	cfg := &config.Config{
		Project: config.ProjectConfig{Workspace: stateDir, Name: "test"},
		Repositories: []config.RepositoryConfig{
			{Name: "ml-svc-zcode", Path: filepath.Join(stateDir, "ml-svc-zcode"), IssueBranchPrefix: "cnb", BaselineBranch: "ai-v2.0"},
			{Name: "ml-web-zcode", Path: filepath.Join(stateDir, "ml-web-zcode"), IssueBranchPrefix: "cnb", BaselineBranch: "poc/api-adapter-validation"},
		},
		Executor: config.ExecutorConfig{Model: "gpt-5.5", ReasoningEffort: "xhigh"},
		Integration: config.IntegrationConfig{
			WorktreeRoot:             filepath.Join(stateDir, ".local_worktrees"),
			MergeMode:                "no-ff",
			PushOnClose:              true,
			CleanupOnClose:           true,
			AllowSinglePendingVerify: true,
		},
		Runner: config.RunnerConfig{
			MaxConcurrency:         5,
			MaxAttempts:            3,
			RetryBackoffSeconds:    300,
			CapacityBackoffSeconds: 900,
			QuotaBackoffSeconds:    3600,
			StaleWorkerSeconds:     7200,
		},
		Labels: config.LabelConfig{
			Running:    "处理中",
			Done:       "已处理",
			Failed:     "处理失败",
			NeedsHuman: "需人工确认",
			Skip:       []string{"已处理", "处理中", "处理失败", "需人工确认"},
		},
		Paths: config.PathConfig{
			StateDir:       stateDir,
			PromptTemplate: templatePath,
			RunnerLog:      filepath.Join(stateDir, "runner.log"),
		},
	}
	store := state.New(stateDir)
	provider := &fakeProvider{}
	executor := &fakeExecutor{alive: map[string]bool{}, results: map[string]domain.WorkerResult{}}
	gitOps := &fakeGitOps{}
	runner := New(Dependencies{
		Config:   cfg,
		Store:    store,
		Provider: provider,
		Executor: executor,
		GitOps:   gitOps,
		Now:      func() time.Time { return now },
	})
	return &runnerTestEnv{stateDir: stateDir, cfg: cfg, store: store, provider: provider, executor: executor, gitOps: gitOps, runner: runner, now: now}
}

type labelCall struct {
	number string
	labels []string
}

type fakeProvider struct {
	issues        []domain.Issue
	getIssues     map[string]domain.Issue
	addedLabels   []labelCall
	removedLabels []labelCall
	comments      []string
}

func (f *fakeProvider) ListOpen(ctx context.Context) ([]domain.Issue, error) {
	return f.issues, nil
}

func (f *fakeProvider) Get(ctx context.Context, number string) (domain.Issue, error) {
	if issue, ok := f.getIssues[number]; ok {
		return issue, nil
	}
	for _, issue := range f.issues {
		if issue.Number == number {
			return issue, nil
		}
	}
	return domain.Issue{}, nil
}

func (f *fakeProvider) AddLabels(ctx context.Context, number string, labels []string) error {
	f.addedLabels = append(f.addedLabels, labelCall{number: number, labels: labels})
	return nil
}

func (f *fakeProvider) RemoveLabels(ctx context.Context, number string, labels []string) error {
	f.removedLabels = append(f.removedLabels, labelCall{number: number, labels: labels})
	return nil
}

func (f *fakeProvider) AddComment(ctx context.Context, number string, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

type fakeExecutor struct {
	startWorker domain.WorkerState
	alive       map[string]bool
	results     map[string]domain.WorkerResult
	started     int
	terminated  []string
}

func (f *fakeExecutor) Start(ctx context.Context, task domain.Task) (domain.WorkerState, error) {
	f.started++
	worker := f.startWorker
	if worker.IssueNumber == "" {
		worker.IssueNumber = task.Issue.Number
	}
	return worker, nil
}

func (f *fakeExecutor) Alive(ctx context.Context, worker domain.WorkerState) bool {
	return f.alive[worker.IssueNumber]
}

func (f *fakeExecutor) Result(ctx context.Context, worker domain.WorkerState) domain.WorkerResult {
	return f.results[worker.IssueNumber]
}

func (f *fakeExecutor) Terminate(ctx context.Context, worker domain.WorkerState) error {
	f.terminated = append(f.terminated, worker.IssueNumber)
	return nil
}

type fakeGitOps struct {
	prepared     []string
	merged       []string
	reverted     []string
	pushed       []string
	cleaned      []string
	mergeResults map[string]gitops.MergeResult
	mergeErrs    map[string]error
}

func (f *fakeGitOps) PrepareIssueWorktree(ctx context.Context, worktreeRoot string, issueNumber string, repo config.RepositoryConfig) (domain.TaskRepository, error) {
	f.prepared = append(f.prepared, issueNumber+":"+repo.Name)
	return domain.TaskRepository{
		Name:           repo.Name,
		Path:           repo.Path,
		WorktreePath:   filepath.Join(worktreeRoot, "issue-"+issueNumber, repo.Name),
		IssueBranch:    repo.IssueBranchPrefix + "/issue-" + issueNumber + "/" + repo.Name,
		BaselineBranch: repo.BaselineBranch,
	}, nil
}

func (f *fakeGitOps) MergeIssueBranch(ctx context.Context, repoPath string, baselineBranch string, issueBranch string, mergeMode string) (gitops.MergeResult, error) {
	repoName := filepath.Base(repoPath)
	f.merged = append(f.merged, repoName+":"+issueBranch)
	if err := f.mergeErrs[repoName]; err != nil {
		return gitops.MergeResult{}, err
	}
	if result, ok := f.mergeResults[repoName]; ok {
		return result, nil
	}
	return gitops.MergeResult{MergeCommit: "merge-" + repoName}, nil
}

func (f *fakeGitOps) RevertMergeCommit(ctx context.Context, repoPath string, baselineBranch string, mergeCommit string) (string, error) {
	f.reverted = append(f.reverted, filepath.Base(repoPath)+":"+mergeCommit)
	return "revert-" + mergeCommit, nil
}

func (f *fakeGitOps) PushBaselineBranch(ctx context.Context, repoPath string, baselineBranch string) (string, error) {
	f.pushed = append(f.pushed, filepath.Base(repoPath)+":"+baselineBranch)
	return "push-" + filepath.Base(repoPath), nil
}

func (f *fakeGitOps) CleanupIssueWorktree(ctx context.Context, repoPath string, worktreePath string, issueBranch string) error {
	f.cleaned = append(f.cleaned, filepath.Base(repoPath)+":"+issueBranch)
	return nil
}

func readRunnerLogEntries(t *testing.T, path string) []map[string]any {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		entries = append(entries, entry)
	}
	return entries
}

func requireRunnerLogEvent(t *testing.T, entries []map[string]any, event string) map[string]any {
	t.Helper()

	for _, entry := range entries {
		if entry["event"] == event {
			return entry
		}
	}
	t.Fatalf("runner log event %q not found", event)
	return nil
}
