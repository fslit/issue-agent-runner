package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"issue-agent-runner/internal/config"
	"issue-agent-runner/internal/domain"
	"issue-agent-runner/internal/executor"
	"issue-agent-runner/internal/gitops"
	"issue-agent-runner/internal/integration"
	"issue-agent-runner/internal/prompt"
	"issue-agent-runner/internal/provider"
	"issue-agent-runner/internal/report"
	"issue-agent-runner/internal/state"
)

const (
	legacyFailedLabel     = "ai:failed"
	legacyDoneLabel       = "ai:fixed"
	legacyQueuedLabel     = "ai:queued"
	legacyRunningLabel    = "ai:in-progress"
	readyToIntegrateLabel = "待集成"
	awaitingVerifyLabel   = "待验证"
)

type Dependencies struct {
	Config   *config.Config
	Store    *state.Store
	Provider provider.IssueProvider
	Executor executor.Executor
	GitOps   GitOps
	Now      func() time.Time
	DryRun   bool
}

type GitOps interface {
	PrepareIssueWorktree(ctx context.Context, worktreeRoot string, issueNumber string, repo config.RepositoryConfig) (domain.TaskRepository, error)
	MergeIssueBranch(ctx context.Context, repoPath string, baselineBranch string, issueBranch string, mergeMode string) (gitops.MergeResult, error)
	RevertMergeCommit(ctx context.Context, repoPath string, baselineBranch string, mergeCommit string) (string, error)
	PushBaselineBranch(ctx context.Context, repoPath string, baselineBranch string) (string, error)
	CleanupIssueWorktree(ctx context.Context, repoPath string, worktreePath string, issueBranch string) error
}

type Runner struct {
	cfg      *config.Config
	store    *state.Store
	provider provider.IssueProvider
	executor executor.Executor
	gitOps   GitOps
	now      func() time.Time
	dryRun   bool
}

type currentValidation struct {
	Number         string                     `json:"number"`
	Title          string                     `json:"title,omitempty"`
	WorkflowStatus domain.IssueWorkflowStatus `json:"workflowStatus"`
	IntegratedAt   int64                      `json:"integratedAt,omitempty"`
	Summary        string                     `json:"summary,omitempty"`
	Verification   []string                   `json:"verification,omitempty"`
	Repositories   []currentValidationRepo    `json:"repositories,omitempty"`
}

type currentValidationRepo struct {
	Repo                string `json:"repo,omitempty"`
	BaselineBranch      string `json:"baselineBranch,omitempty"`
	BaselineMergeCommit string `json:"baselineMergeCommit,omitempty"`
	IssueBranch         string `json:"issueBranch,omitempty"`
	CommitSHA           string `json:"commitSha,omitempty"`
}

func New(deps Dependencies) *Runner {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{
		cfg:      deps.Config,
		store:    deps.Store,
		provider: deps.Provider,
		executor: deps.Executor,
		gitOps:   deps.GitOps,
		now:      now,
		dryRun:   deps.DryRun,
	}
}

func (r *Runner) Once(ctx context.Context) error {
	r.logEvent("poll_start", nil)
	issues, err := r.provider.ListOpen(ctx)
	if err != nil {
		r.logEvent("poll_failed", map[string]any{"stage": "list_open", "error": err.Error()})
		return err
	}

	return r.store.WithLock(ctx, func(st *domain.State) error {
		now := r.now()
		if r.dryRun {
			r.logEvent("dry_run_poll_complete", map[string]any{"open": len(issues)})
			return r.writeReports(issues, *st, now)
		}
		handled := map[string]bool{}
		if err := r.releaseRemoteDoneWorkers(ctx, issues, st, handled); err != nil {
			return err
		}
		if err := r.pruneWorkers(ctx, st, now, handled); err != nil {
			return err
		}
		if err := r.ensureAwaitingVerifyPushed(ctx, st); err != nil {
			return err
		}
		if err := r.processClosedAwaitingVerify(ctx, st, now); err != nil {
			return err
		}
		if err := r.processIntegrationQueue(ctx, issues, st, now, handled); err != nil {
			return err
		}
		if err := r.syncKnownWorkflowLabels(ctx, issues, *st, handled); err != nil {
			return err
		}

		capacity := r.cfg.Runner.MaxConcurrency - len(st.Workers)
		if capacity > 0 {
			for _, issue := range issues {
				if capacity == 0 {
					break
				}
				if handled[issue.Number] {
					continue
				}
				if !IsCandidate(issue, *st, r.cfg.Labels, now) {
					continue
				}
				if err := r.claimAndStart(ctx, issue, st, now); err != nil {
					r.logEvent("claim_failed", map[string]any{"issue": issue.Number, "error": err.Error()})
					return err
				}
				capacity--
			}
		}
		r.logEvent("poll_complete", map[string]any{"open": len(issues), "active": len(st.Workers)})
		if err := r.writeReports(issues, *st, now); err != nil {
			return err
		}
		r.runHook(ctx, "onProgressUpdate", r.cfg.Hooks.OnProgressUpdate, domain.Issue{}, domain.IssueState{})
		return nil
	})
}

func (r *Runner) releaseRemoteDoneWorkers(ctx context.Context, issues []domain.Issue, st *domain.State, handled map[string]bool) error {
	done := map[string]bool{}
	for _, issue := range issues {
		if containsLabel(issue.Labels, r.cfg.Labels.Done) {
			done[issue.Number] = true
		}
	}
	for number, worker := range st.Workers {
		if !done[number] {
			continue
		}
		if err := r.executor.Terminate(ctx, worker); err != nil {
			return err
		}
		delete(st.Workers, number)
		handled[number] = true
	}
	return nil
}

func (r *Runner) pruneWorkers(ctx context.Context, st *domain.State, now time.Time, handled map[string]bool) error {
	for number, worker := range st.Workers {
		if r.executor.Alive(ctx, worker) {
			if r.workerExceededHardTimeout(worker, now) {
				if err := r.executor.Terminate(ctx, worker); err != nil {
					return err
				}
				ApplyFailure(number, worker, domain.WorkerResult{
					ErrorKind:    domain.ErrorTimeout,
					ErrorMessage: "worker hard timeout; terminated for retry",
				}, now, r.cfg.Runner, st)
				r.logWorkerFailed(number, st.Issues[number])
				r.runPausedHookIfNeeded(ctx, number, st.Issues[number])
				delete(st.Workers, number)
				handled[number] = true
				continue
			}
			if r.workerIsStale(worker, now) {
				if err := r.executor.Terminate(ctx, worker); err != nil {
					return err
				}
				ApplyFailure(number, worker, domain.WorkerResult{
					ErrorKind:    domain.ErrorStale,
					ErrorMessage: "worker log stale; terminated for retry",
				}, now, r.cfg.Runner, st)
				r.logWorkerFailed(number, st.Issues[number])
				r.runPausedHookIfNeeded(ctx, number, st.Issues[number])
				delete(st.Workers, number)
				handled[number] = true
			}
			continue
		}

		result := r.executor.Result(ctx, worker)
		if workerSucceeded(result) {
			if err := r.applySuccess(ctx, number, worker, result, st, now); err != nil {
				return err
			}
		} else {
			ApplyFailure(number, worker, result, now, r.cfg.Runner, st)
			r.logWorkerFailed(number, st.Issues[number])
			r.runPausedHookIfNeeded(ctx, number, st.Issues[number])
		}
		delete(st.Workers, number)
		handled[number] = true
	}
	return nil
}

func (r *Runner) logWorkerFailed(number string, meta domain.IssueState) {
	fields := map[string]any{
		"issue":     number,
		"errorKind": meta.ErrorKind,
		"attempts":  meta.Attempts,
	}
	if meta.LastError != "" {
		fields["error"] = meta.LastError
	}
	if meta.LastExitCode != nil {
		fields["exitCode"] = *meta.LastExitCode
	}
	r.logEvent("worker_failed", fields)
}

func (r *Runner) claimAndStart(ctx context.Context, issue domain.Issue, st *domain.State, now time.Time) error {
	fresh, err := r.provider.Get(ctx, issue.Number)
	if err != nil {
		return err
	}
	if fresh.Number != "" {
		issue = fresh
	}
	if containsLabel(issue.Labels, r.cfg.Labels.Done) {
		return nil
	}

	if r.dryRun {
		return nil
	}
	if labelsToRemove := legacyClaimLabels(issue.Labels); len(labelsToRemove) > 0 {
		if err := r.provider.RemoveLabels(ctx, issue.Number, labelsToRemove); err != nil {
			return err
		}
	}
	if err := r.provider.AddLabels(ctx, issue.Number, []string{r.cfg.Labels.Running}); err != nil {
		return err
	}

	taskRepos, err := r.prepareTaskRepositories(ctx, issue.Number)
	if err != nil {
		return err
	}
	task, err := r.buildTask(issue, taskRepos)
	if err != nil {
		return err
	}
	worker, err := r.executor.Start(ctx, task)
	if err != nil {
		ApplyFailure(issue.Number, domain.WorkerState{IssueNumber: issue.Number, Title: issue.Title}, domain.WorkerResult{
			ErrorMessage: err.Error(),
		}, now, r.cfg.Runner, st)
		r.logEvent("worker_start_failed", map[string]any{"issue": issue.Number, "error": err.Error()})
		return nil
	}
	worker.IssueNumber = issue.Number
	if worker.Title == "" {
		worker.Title = issue.Title
	}
	if worker.StartedAt == 0 {
		worker.StartedAt = now.Unix()
	}

	st.Workers[issue.Number] = worker
	meta := st.Issues[issue.Number]
	meta.Title = issue.Title
	meta.LastStartedAt = now.Unix()
	meta.NextRetryAt = 0
	meta.Paused = false
	meta.PauseReason = ""
	meta.WorkflowStatus = domain.IssueWorkflowStatusRunning
	meta.AgentModel = r.cfg.Executor.Model
	meta.AgentReasoningEffort = r.cfg.Executor.ReasoningEffort
	meta.Repositories = toIssueRepoState(taskRepos)
	st.Issues[issue.Number] = meta
	r.logEvent("worker_started", map[string]any{"issue": issue.Number, "pid": worker.PID, "repos": len(taskRepos)})
	r.runHook(ctx, "onIssueClaim", r.cfg.Hooks.OnIssueClaim, issue, meta)
	return nil
}

func legacyClaimLabels(labels []string) []string {
	for _, label := range labels {
		if label == legacyFailedLabel {
			return []string{legacyFailedLabel}
		}
	}
	return nil
}

func (r *Runner) buildTask(issue domain.Issue, repos []domain.TaskRepository) (domain.Task, error) {
	base := filepath.Join(r.cfg.Paths.StateDir, "issue-"+issue.Number)
	task := domain.Task{
		Issue:      issue,
		RepoURL:    "https://cnb.cool/" + r.cfg.Provider.Repo,
		Workspace:  r.cfg.Project.Workspace,
		PromptPath: base + ".prompt.md",
		LogPath:    base + ".log",
		ResultPath: base + ".result.json",
		Repos:      repos,
	}

	rendered, err := prompt.RenderFile(r.cfg.Paths.PromptTemplate, prompt.Data{
		RepoURL:       task.RepoURL,
		Number:        issue.Number,
		Title:         issue.Title,
		Body:          issue.Body,
		ResultPath:    task.ResultPath,
		CommandPrefix: "rtk",
		Repositories:  toPromptRepositories(task.Repos),
	})
	if err != nil {
		return domain.Task{}, err
	}
	task.Prompt = rendered
	return task, nil
}

func toPromptRepositories(repos []domain.TaskRepository) []prompt.RepositoryData {
	values := make([]prompt.RepositoryData, 0, len(repos))
	for _, repo := range repos {
		values = append(values, prompt.RepositoryData{
			Name:           repo.Name,
			Path:           repo.Path,
			WorktreePath:   repo.WorktreePath,
			IssueBranch:    repo.IssueBranch,
			BaselineBranch: repo.BaselineBranch,
		})
	}
	return values
}

func toIssueRepoState(repos []domain.TaskRepository) map[string]domain.IssueRepoState {
	if len(repos) == 0 {
		return nil
	}
	values := make(map[string]domain.IssueRepoState, len(repos))
	for _, repo := range repos {
		values[repo.Name] = domain.IssueRepoState{
			Repo:           repo.Name,
			RepoPath:       repo.Path,
			WorktreePath:   repo.WorktreePath,
			IssueBranch:    repo.IssueBranch,
			BaselineBranch: repo.BaselineBranch,
		}
	}
	return values
}

func applyRepoResults(meta *domain.IssueState, results []domain.RepoWorkerResult) {
	if len(results) == 0 {
		return
	}
	if meta.Repositories == nil {
		meta.Repositories = map[string]domain.IssueRepoState{}
	}
	for _, result := range results {
		repoState := meta.Repositories[result.Repo]
		repoState.Repo = result.Repo
		repoState.RepoPath = result.RepoPath
		repoState.WorktreePath = result.WorktreePath
		repoState.IssueBranch = result.IssueBranch
		repoState.BaselineBranch = result.BaselineBranch
		repoState.IssueHeadSHA = result.HeadSHA
		repoState.CommitSHA = result.CommitSHA
		meta.Repositories[result.Repo] = repoState
	}
}

func (r *Runner) prepareTaskRepositories(ctx context.Context, issueNumber string) ([]domain.TaskRepository, error) {
	if r.gitOps == nil || len(r.cfg.Repositories) == 0 {
		return nil, nil
	}
	repos := make([]domain.TaskRepository, 0, len(r.cfg.Repositories))
	for _, repo := range r.cfg.Repositories {
		taskRepo, err := r.gitOps.PrepareIssueWorktree(ctx, r.cfg.Integration.WorktreeRoot, issueNumber, repo)
		if err != nil {
			return nil, err
		}
		repos = append(repos, taskRepo)
	}
	return repos, nil
}

func (r *Runner) processIntegrationQueue(ctx context.Context, issues []domain.Issue, st *domain.State, now time.Time, handled map[string]bool) error {
	if r.gitOps == nil || hasAwaitingVerify(st) {
		return nil
	}

	number, ok := nextReadyIssue(st)
	if !ok {
		return nil
	}

	issue := issueByNumber(issues, number)
	meta := st.Issues[number]
	mergedRepos := make([]string, 0, len(meta.Repositories))
	for _, repoName := range touchedRepoNames(meta) {
		repoState := meta.Repositories[repoName]
		result, err := r.gitOps.MergeIssueBranch(ctx, repoState.RepoPath, repoState.BaselineBranch, repoState.IssueBranch, r.cfg.Integration.MergeMode)
		if err != nil {
			meta = r.rollbackMergedRepos(ctx, meta, mergedRepos)
			var conflictErr *gitops.MergeConflictError
			if errors.As(err, &conflictErr) {
				repoState.ConflictFiles = append([]string(nil), conflictErr.Files...)
				meta.Repositories[repoName] = repoState
				meta.WorkflowStatus = domain.IssueWorkflowStatusMergeConflict
				meta.Paused = true
				meta.PauseReason = err.Error()
				st.Issues[number] = meta
				r.logEvent("merge_conflict", map[string]any{"issue": number, "repo": repoName, "files": append([]string(nil), conflictErr.Files...)})
				if removeErr := r.provider.RemoveLabels(ctx, number, workflowStateLabels(r.cfg.Labels)); removeErr != nil {
					return removeErr
				}
				if addErr := r.provider.AddLabels(ctx, number, []string{r.cfg.Labels.NeedsHuman}); addErr != nil {
					return addErr
				}
				if commentErr := r.provider.AddComment(ctx, number, integration.MergeConflictComment(issue, meta, r.agentInfo(meta))); commentErr != nil {
					return commentErr
				}
				handled[number] = true
				r.runHook(ctx, "onIssuePaused", r.cfg.Hooks.OnIssuePaused, issue, meta)
				return nil
			}
			return err
		}
		repoState.BaselineMergeCommit = result.MergeCommit
		repoState.ConflictFiles = nil
		meta.Repositories[repoName] = repoState
		mergedRepos = append(mergedRepos, repoName)
		r.logEvent("merged", map[string]any{"issue": number, "repo": repoName, "baseline": repoState.BaselineBranch, "commit": result.MergeCommit})
	}

	pushedRepos := make([]string, 0, len(mergedRepos))
	for _, repoName := range mergedRepos {
		repoState := meta.Repositories[repoName]
		pushedSHA, err := r.gitOps.PushBaselineBranch(ctx, repoState.RepoPath, repoState.BaselineBranch)
		if err != nil {
			meta = r.rollbackMergedRepos(ctx, meta, mergedRepos)
			if rollbackErr := r.pushRollbackRepos(ctx, meta, pushedRepos); rollbackErr != nil {
				return rollbackErr
			}
			return err
		}
		repoState.PushedBaselineSHA = pushedSHA
		meta.Repositories[repoName] = repoState
		pushedRepos = append(pushedRepos, repoName)
		r.logEvent("pushed_for_verification", map[string]any{"issue": number, "repo": repoName, "branch": repoState.BaselineBranch, "sha": pushedSHA})
	}

	meta.WorkflowStatus = domain.IssueWorkflowStatusAwaitingVerify
	meta.IntegratedAt = now.Unix()
	st.Issues[number] = meta
	r.logEvent("awaiting_verify", map[string]any{"issue": number})
	if err := r.syncWorkflowLabels(ctx, number, domain.IssueWorkflowStatusAwaitingVerify); err != nil {
		return err
	}
	handled[number] = true
	if err := r.writeCurrentValidationFiles(issues, *st); err != nil {
		return err
	}
	r.runHook(ctx, "onIssueIntegrated", r.cfg.Hooks.OnIssueIntegrated, issue, meta)
	return r.provider.AddComment(ctx, number, integration.AwaitingVerifyComment(issue, meta, r.agentInfo(meta)))
}

func (r *Runner) ensureAwaitingVerifyPushed(ctx context.Context, st *domain.State) error {
	if r.gitOps == nil {
		return nil
	}
	for number, meta := range st.Issues {
		if meta.WorkflowStatus != domain.IssueWorkflowStatusAwaitingVerify {
			continue
		}
		for _, repoName := range touchedRepoNames(meta) {
			repoState := meta.Repositories[repoName]
			if repoState.BaselineMergeCommit == "" || repoState.PushedBaselineSHA != "" {
				continue
			}
			sha, err := r.gitOps.PushBaselineBranch(ctx, repoState.RepoPath, repoState.BaselineBranch)
			if err != nil {
				r.logEvent("push_failed", map[string]any{"issue": number, "repo": repoName, "error": err.Error()})
				return err
			}
			repoState.PushedBaselineSHA = sha
			meta.Repositories[repoName] = repoState
			r.logEvent("pushed_for_verification", map[string]any{"issue": number, "repo": repoName, "branch": repoState.BaselineBranch, "sha": sha})
		}
		st.Issues[number] = meta
	}
	return nil
}

func (r *Runner) processClosedAwaitingVerify(ctx context.Context, st *domain.State, now time.Time) error {
	if r.gitOps == nil {
		return nil
	}
	for number, meta := range st.Issues {
		if meta.WorkflowStatus != domain.IssueWorkflowStatusAwaitingVerify {
			continue
		}
		issue, err := r.provider.Get(ctx, number)
		if err != nil {
			return err
		}
		if issue.State != "closed" {
			continue
		}

		for _, repoName := range touchedRepoNames(meta) {
			repoState := meta.Repositories[repoName]
			if repoState.PushedBaselineSHA != "" {
				continue
			}
			sha, err := r.gitOps.PushBaselineBranch(ctx, repoState.RepoPath, repoState.BaselineBranch)
			if err != nil {
				r.logEvent("push_failed", map[string]any{"issue": number, "repo": repoName, "error": err.Error()})
				return err
			}
			repoState.PushedBaselineSHA = sha
			meta.Repositories[repoName] = repoState
			r.logEvent("pushed", map[string]any{"issue": number, "repo": repoName, "branch": repoState.BaselineBranch, "sha": sha})
		}
		meta.WorkflowStatus = domain.IssueWorkflowStatusClosedPushed
		meta.ClosedAt = now.Unix()
		st.Issues[number] = meta
		if err := r.syncWorkflowLabels(ctx, number, domain.IssueWorkflowStatusClosedPushed); err != nil {
			return err
		}
		if err := r.provider.AddComment(ctx, number, integration.ClosedPushedComment(issue, meta, r.agentInfo(meta))); err != nil {
			return err
		}
		r.logEvent("issue_closed_pushed", map[string]any{"issue": number})
		if r.cfg.Integration.CleanupOnClose {
			if err := r.cleanupIssue(ctx, number, st, now); err != nil {
				return err
			}
		}
		if err := r.writeCurrentValidationFiles(nil, *st); err != nil {
			return err
		}
		r.runHook(ctx, "onIssueClosedPushed", r.cfg.Hooks.OnIssueClosedPushed, issue, meta)
	}
	return nil
}

func (r *Runner) rollbackMergedRepos(ctx context.Context, meta domain.IssueState, repoNames []string) domain.IssueState {
	if r.gitOps == nil {
		return meta
	}
	for _, repoName := range repoNames {
		repoState := meta.Repositories[repoName]
		if repoState.BaselineMergeCommit == "" {
			continue
		}
		_, _ = r.gitOps.RevertMergeCommit(ctx, repoState.RepoPath, repoState.BaselineBranch, repoState.BaselineMergeCommit)
		repoState.BaselineMergeCommit = ""
		meta.Repositories[repoName] = repoState
	}
	return meta
}

func (r *Runner) pushRollbackRepos(ctx context.Context, meta domain.IssueState, repoNames []string) error {
	if r.gitOps == nil {
		return nil
	}
	for _, repoName := range repoNames {
		repoState := meta.Repositories[repoName]
		sha, err := r.gitOps.PushBaselineBranch(ctx, repoState.RepoPath, repoState.BaselineBranch)
		if err != nil {
			return err
		}
		repoState.PushedBaselineSHA = sha
		meta.Repositories[repoName] = repoState
	}
	return nil
}

func hasAwaitingVerify(st *domain.State) bool {
	for _, meta := range st.Issues {
		if meta.WorkflowStatus == domain.IssueWorkflowStatusAwaitingVerify {
			return true
		}
	}
	return false
}

func nextReadyIssue(st *domain.State) (string, bool) {
	type candidate struct {
		number  string
		readyAt int64
	}
	items := make([]candidate, 0)
	for number, meta := range st.Issues {
		if meta.WorkflowStatus == domain.IssueWorkflowStatusReadyToIntegrate {
			items = append(items, candidate{number: number, readyAt: meta.ReadyAt})
		}
	}
	if len(items) == 0 {
		return "", false
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].readyAt == items[j].readyAt {
			return items[i].number < items[j].number
		}
		return items[i].readyAt < items[j].readyAt
	})
	return items[0].number, true
}

func issueByNumber(issues []domain.Issue, number string) domain.Issue {
	for _, issue := range issues {
		if issue.Number == number {
			return issue
		}
	}
	return domain.Issue{Number: number}
}

func touchedRepoNames(meta domain.IssueState) []string {
	names := make([]string, 0, len(meta.Repositories))
	for name, repo := range meta.Repositories {
		if repo.CommitSHA == "" && repo.IssueHeadSHA == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Runner) Reject(ctx context.Context, number string) error {
	return r.store.WithLock(ctx, func(st *domain.State) error {
		meta := st.Issues[number]
		if meta.WorkflowStatus != domain.IssueWorkflowStatusAwaitingVerify {
			return fmt.Errorf("issue %s is not awaiting verification", number)
		}
		for _, repoName := range touchedRepoNames(meta) {
			repoState := meta.Repositories[repoName]
			if repoState.BaselineMergeCommit == "" {
				continue
			}
			if _, err := r.gitOps.RevertMergeCommit(ctx, repoState.RepoPath, repoState.BaselineBranch, repoState.BaselineMergeCommit); err != nil {
				return err
			}
			r.logEvent("reverted", map[string]any{"issue": number, "repo": repoName, "merge": repoState.BaselineMergeCommit})
			repoState.BaselineMergeCommit = ""
			pushedSHA, err := r.gitOps.PushBaselineBranch(ctx, repoState.RepoPath, repoState.BaselineBranch)
			if err != nil {
				return err
			}
			repoState.PushedBaselineSHA = pushedSHA
			meta.Repositories[repoName] = repoState
		}
		meta.WorkflowStatus = domain.IssueWorkflowStatusUnknown
		meta.RejectedAt = r.now().Unix()
		meta.ReadyAt = 0
		meta.IntegratedAt = 0
		st.Issues[number] = meta
		if err := r.syncWorkflowLabels(ctx, number, domain.IssueWorkflowStatusUnknown); err != nil {
			return err
		}
		r.logEvent("issue_rejected", map[string]any{"issue": number})
		if err := r.provider.AddComment(ctx, number, integration.RejectComment(domain.Issue{Number: number}, r.agentInfo(meta))); err != nil {
			return err
		}
		if err := r.writeCurrentValidationFiles(nil, *st); err != nil {
			return err
		}
		r.runHook(ctx, "onIssueRejected", r.cfg.Hooks.OnIssueRejected, domain.Issue{Number: number}, meta)
		return nil
	})
}

func (r *Runner) runPausedHookIfNeeded(ctx context.Context, number string, meta domain.IssueState) {
	if !meta.Paused {
		return
	}
	r.runHook(ctx, "onIssuePaused", r.cfg.Hooks.OnIssuePaused, issueFromState(number, meta), meta)
}

func issueFromState(number string, meta domain.IssueState) domain.Issue {
	return domain.Issue{
		Number: number,
		Title:  meta.Title,
	}
}

func (r *Runner) agentInfo(meta domain.IssueState) integration.AgentInfo {
	model := meta.AgentModel
	if model == "" {
		model = r.cfg.Executor.Model
	}
	reasoningEffort := meta.AgentReasoningEffort
	if reasoningEffort == "" {
		reasoningEffort = r.cfg.Executor.ReasoningEffort
	}
	return integration.AgentInfo{
		Model:           model,
		ReasoningEffort: reasoningEffort,
	}
}

func (r *Runner) syncWorkflowLabels(ctx context.Context, number string, status domain.IssueWorkflowStatus) error {
	remove := workflowStateLabels(r.cfg.Labels)
	if err := r.provider.RemoveLabels(ctx, number, remove); err != nil {
		return err
	}

	label := workflowStatusLabel(status)
	if label == "" {
		return nil
	}
	return r.provider.AddLabels(ctx, number, []string{label})
}

func (r *Runner) syncKnownWorkflowLabels(ctx context.Context, issues []domain.Issue, st domain.State, handled map[string]bool) error {
	for _, issue := range issues {
		if handled[issue.Number] {
			continue
		}
		meta := st.Issues[issue.Number]
		if err := r.syncKnownIssueWorkflowLabels(ctx, issue, meta.WorkflowStatus); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) syncKnownIssueWorkflowLabels(ctx context.Context, issue domain.Issue, status domain.IssueWorkflowStatus) error {
	desired := workflowStatusLabel(status)
	if desired == "" {
		return nil
	}

	current := labelSet(issue.Labels)
	remove := make([]string, 0)
	for _, label := range workflowStateLabels(r.cfg.Labels) {
		if label != desired && current[label] {
			remove = append(remove, label)
		}
	}
	if len(remove) > 0 {
		if err := r.provider.RemoveLabels(ctx, issue.Number, remove); err != nil {
			return err
		}
	}
	if !current[desired] {
		return r.provider.AddLabels(ctx, issue.Number, []string{desired})
	}
	return nil
}

func workflowStatusLabel(status domain.IssueWorkflowStatus) string {
	switch status {
	case domain.IssueWorkflowStatusReadyToIntegrate:
		return readyToIntegrateLabel
	case domain.IssueWorkflowStatusAwaitingVerify:
		return awaitingVerifyLabel
	default:
		return ""
	}
}

func workflowStateLabels(labels config.LabelConfig) []string {
	return []string{
		labels.Running,
		labels.Done,
		legacyDoneLabel,
		legacyQueuedLabel,
		legacyRunningLabel,
		readyToIntegrateLabel,
		awaitingVerifyLabel,
	}
}

func (r *Runner) cleanupIssue(ctx context.Context, number string, st *domain.State, now time.Time) error {
	if r.gitOps == nil {
		return nil
	}
	meta := st.Issues[number]
	for _, repoState := range meta.Repositories {
		if err := r.gitOps.CleanupIssueWorktree(ctx, repoState.RepoPath, repoState.WorktreePath, repoState.IssueBranch); err != nil {
			return err
		}
		r.logEvent("cleanup_repo", map[string]any{"issue": number, "repo": repoState.Repo, "branch": repoState.IssueBranch})
	}
	meta.WorkflowStatus = domain.IssueWorkflowStatusCleaned
	meta.CleanedAt = now.Unix()
	st.Issues[number] = meta
	if err := r.syncWorkflowLabels(ctx, number, domain.IssueWorkflowStatusCleaned); err != nil {
		return err
	}
	r.logEvent("issue_cleaned", map[string]any{"issue": number})
	return nil
}

func (r *Runner) applySuccess(ctx context.Context, number string, worker domain.WorkerState, result domain.WorkerResult, st *domain.State, now time.Time) error {
	meta := st.Issues[number]
	zero := 0
	if result.ExitCode != nil {
		meta.LastExitCode = result.ExitCode
	} else {
		meta.LastExitCode = &zero
	}
	meta.LastError = ""
	meta.ErrorKind = domain.ErrorNone
	meta.LastFinishedAt = now.Unix()
	meta.NextRetryAt = 0
	meta.Title = worker.Title
	meta.WorkflowStatus = domain.IssueWorkflowStatusReadyToIntegrate
	meta.ReadyAt = now.Unix()
	meta.Result = &result
	applyRepoResults(&meta, result.RepoResults)
	st.Issues[number] = meta
	fields := map[string]any{
		"issue":              number,
		"status":             result.Status,
		"repos":              len(result.RepoResults),
		"verification_steps": len(result.Verification),
	}
	if result.ExitCode != nil {
		fields["exit_code"] = *result.ExitCode
	}
	r.logEvent("worker_succeeded", fields)
	return r.syncWorkflowLabels(ctx, number, domain.IssueWorkflowStatusReadyToIntegrate)
}

func (r *Runner) workerIsStale(worker domain.WorkerState, now time.Time) bool {
	if r.cfg.Runner.StaleWorkerSeconds <= 0 {
		return false
	}
	logPath := worker.LogPathValue()
	if logPath != "" {
		info, err := os.Stat(logPath)
		if err == nil {
			return now.Unix()-info.ModTime().Unix() >= int64(r.cfg.Runner.StaleWorkerSeconds)
		}
	}
	if worker.StartedAt == 0 {
		return false
	}
	return now.Unix()-worker.StartedAt >= int64(r.cfg.Runner.StaleWorkerSeconds)
}

func (r *Runner) workerExceededHardTimeout(worker domain.WorkerState, now time.Time) bool {
	if r.cfg.Runner.HardWorkerSeconds <= 0 || worker.StartedAt == 0 {
		return false
	}
	return now.Unix()-worker.StartedAt >= int64(r.cfg.Runner.HardWorkerSeconds)
}

func (r *Runner) writeReports(issues []domain.Issue, st domain.State, now time.Time) error {
	if err := os.MkdirAll(r.cfg.Paths.StateDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.cfg.Paths.StateDir, "progress-summary.md"), []byte(report.RenderMarkdown(issues, st, now)), 0o644); err != nil {
		return err
	}
	raw, err := report.RenderJSON(issues, st, now)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(r.cfg.Paths.StateDir, "progress-summary.json"), raw, 0o644); err != nil {
		return err
	}
	return r.writeCurrentValidationFiles(issues, st)
}

func (r *Runner) writeCurrentValidationFiles(issues []domain.Issue, st domain.State) error {
	current := buildCurrentValidation(issues, st)
	markdownPath := filepath.Join(r.cfg.Paths.StateDir, "current-validation.md")
	jsonPath := filepath.Join(r.cfg.Paths.StateDir, "current-validation.json")
	if current == nil {
		if err := os.Remove(markdownPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(jsonPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.WriteFile(markdownPath, []byte(renderCurrentValidationMarkdown(*current)), 0o644); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(jsonPath, raw, 0o644)
}

func buildCurrentValidation(issues []domain.Issue, st domain.State) *currentValidation {
	st.Normalize()
	issueTitles := map[string]string{}
	for _, issue := range issues {
		issueTitles[issue.Number] = issue.Title
	}

	type candidate struct {
		number string
		meta   domain.IssueState
	}
	candidates := []candidate{}
	for number, meta := range st.Issues {
		if meta.WorkflowStatus == domain.IssueWorkflowStatusAwaitingVerify {
			candidates = append(candidates, candidate{number: number, meta: meta})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].meta.IntegratedAt == candidates[j].meta.IntegratedAt {
			return candidates[i].number < candidates[j].number
		}
		return candidates[i].meta.IntegratedAt < candidates[j].meta.IntegratedAt
	})

	selected := candidates[0]
	title := selected.meta.Title
	if title == "" {
		title = issueTitles[selected.number]
	}
	current := &currentValidation{
		Number:         selected.number,
		Title:          title,
		WorkflowStatus: selected.meta.WorkflowStatus,
		IntegratedAt:   selected.meta.IntegratedAt,
	}
	if selected.meta.Result != nil {
		current.Summary = selected.meta.Result.Summary
		current.Verification = append([]string(nil), selected.meta.Result.Verification...)
	}
	for _, repoName := range sortedRepoNames(selected.meta.Repositories) {
		repo := selected.meta.Repositories[repoName]
		current.Repositories = append(current.Repositories, currentValidationRepo{
			Repo:                repoName,
			BaselineBranch:      repo.BaselineBranch,
			BaselineMergeCommit: repo.BaselineMergeCommit,
			IssueBranch:         repo.IssueBranch,
			CommitSHA:           repo.CommitSHA,
		})
	}
	return current
}

func renderCurrentValidationMarkdown(current currentValidation) string {
	lines := []string{
		"# Current Validation",
		"",
		fmt.Sprintf("- Issue: #%s %s", current.Number, current.Title),
		fmt.Sprintf("- Status: %s", current.WorkflowStatus),
	}
	if current.IntegratedAt > 0 {
		lines = append(lines, fmt.Sprintf("- Integrated at: %s", time.Unix(current.IntegratedAt, 0).Format("2006-01-02 15:04:05 MST")))
	}
	if current.Summary != "" {
		lines = append(lines, "", "## Summary", current.Summary)
	}
	if len(current.Repositories) > 0 {
		lines = append(lines, "", "## Baseline Branches")
		for _, repo := range current.Repositories {
			if repo.BaselineMergeCommit != "" {
				lines = append(lines, fmt.Sprintf("- `%s` -> `%s` (%s)", repo.Repo, repo.BaselineBranch, repo.BaselineMergeCommit))
				continue
			}
			lines = append(lines, fmt.Sprintf("- `%s` -> `%s`", repo.Repo, repo.BaselineBranch))
		}
	}
	if len(current.Verification) > 0 {
		lines = append(lines, "", "## Worker Verification")
		for _, command := range current.Verification {
			lines = append(lines, "- `"+command+"`")
		}
	}
	lines = append(lines, "", fmt.Sprintf("验证通过后关闭 CNB issue #%s；验证失败执行 `issue-agent-runner reject %s`。", current.Number, current.Number), "")
	return strings.Join(lines, "\n")
}

func sortedRepoNames(items map[string]domain.IssueRepoState) []string {
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Runner) runHook(ctx context.Context, name string, hook config.HookEndpoint, issue domain.Issue, meta domain.IssueState) {
	if hook.Command == "" && hook.Webhook.URL == "" {
		return
	}
	timeout := hook.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	if hook.Command != "" {
		r.runCommandHook(hookCtx, name, hook, issue, meta)
	}
	if hook.Webhook.URL != "" {
		r.runWebhookHook(hookCtx, name, hook, issue, meta)
	}
}

func (r *Runner) runCommandHook(ctx context.Context, name string, hook config.HookEndpoint, issue domain.Issue, meta domain.IssueState) {
	command := exec.CommandContext(ctx, "sh", "-c", hook.Command)
	if r.cfg.Project.Workspace != "" {
		command.Dir = r.cfg.Project.Workspace
	}
	agent := r.agentInfo(meta)
	command.Env = append(os.Environ(),
		"ISSUE_AGENT_EVENT="+name,
		"ISSUE_AGENT_ISSUE_NUMBER="+issue.Number,
		"ISSUE_AGENT_ISSUE_TITLE="+hookIssueTitle(issue, meta),
		"ISSUE_AGENT_STATE_DIR="+r.cfg.Paths.StateDir,
		"ISSUE_AGENT_WORKSPACE="+r.cfg.Project.Workspace,
		"ISSUE_AGENT_MODEL="+agent.Model,
		"ISSUE_AGENT_REASONING_EFFORT="+agent.ReasoningEffort,
		"ISSUE_AGENT_WORKFLOW_STATUS="+string(meta.WorkflowStatus),
	)
	output, err := command.CombinedOutput()
	fields := map[string]any{
		"hook":  name,
		"issue": issue.Number,
	}
	if text := strings.TrimSpace(string(output)); text != "" {
		fields["output"] = text
	}
	if err != nil {
		fields["error"] = err.Error()
		r.logEvent("hook_failed", fields)
		return
	}
	r.logEvent("hook_succeeded", fields)
}

func (r *Runner) runWebhookHook(ctx context.Context, name string, hook config.HookEndpoint, issue domain.Issue, meta domain.IssueState) {
	payload, err := json.Marshal(r.hookPayload(name, issue, meta))
	if err != nil {
		r.logEvent("webhook_failed", map[string]any{"hook": name, "issue": issue.Number, "error": err.Error()})
		return
	}
	method := hook.Webhook.Method
	if method == "" {
		method = http.MethodPost
	}
	request, err := http.NewRequestWithContext(ctx, method, hook.Webhook.URL, bytes.NewReader(payload))
	if err != nil {
		r.logEvent("webhook_failed", map[string]any{"hook": name, "issue": issue.Number, "error": err.Error()})
		return
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	fields := map[string]any{
		"hook":   name,
		"issue":  issue.Number,
		"method": method,
	}
	if err != nil {
		fields["error"] = err.Error()
		r.logEvent("webhook_failed", fields)
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	fields["statusCode"] = response.StatusCode
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		r.logEvent("webhook_failed", fields)
		return
	}
	r.logEvent("webhook_succeeded", fields)
}

func (r *Runner) hookPayload(name string, issue domain.Issue, meta domain.IssueState) map[string]any {
	agent := r.agentInfo(meta)
	return map[string]any{
		"event":           name,
		"issueNumber":     issue.Number,
		"issueTitle":      hookIssueTitle(issue, meta),
		"stateDir":        r.cfg.Paths.StateDir,
		"workspace":       r.cfg.Project.Workspace,
		"model":           agent.Model,
		"reasoningEffort": agent.ReasoningEffort,
		"workflowStatus":  string(meta.WorkflowStatus),
	}
}

func hookIssueTitle(issue domain.Issue, meta domain.IssueState) string {
	if issue.Title != "" {
		return issue.Title
	}
	return meta.Title
}

func (r *Runner) logEvent(event string, fields map[string]any) {
	if r.cfg == nil || r.cfg.Paths.RunnerLog == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.Paths.RunnerLog), 0o755); err != nil {
		return
	}
	handle, err := os.OpenFile(r.cfg.Paths.RunnerLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer handle.Close()
	entry := map[string]any{
		"timestamp": r.now().Format(time.RFC3339Nano),
		"event":     event,
	}
	for key, value := range fields {
		entry[key] = value
	}
	_ = json.NewEncoder(handle).Encode(entry)
}

func workerSucceeded(result domain.WorkerResult) bool {
	return result.Status == "success"
}

func successComment(result domain.WorkerResult) string {
	lines := []string{"IssueAgent Runner completed this issue."}
	if result.Summary != "" {
		lines = append(lines, "", result.Summary)
	}
	if len(result.Verification) > 0 {
		lines = append(lines, "", "Verification:")
		for _, command := range result.Verification {
			lines = append(lines, "- `"+command+"`")
		}
	}
	return strings.Join(lines, "\n")
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func (r *Runner) Status(ctx context.Context) (report.Summary, error) {
	issues, err := r.provider.ListOpen(ctx)
	if err != nil {
		return report.Summary{}, err
	}
	st, err := r.store.Load()
	if err != nil {
		return report.Summary{}, err
	}
	return report.BuildSummary(issues, *st, r.now()), nil
}

func (r *Runner) SummaryMarkdown(ctx context.Context) (string, error) {
	issues, err := r.provider.ListOpen(ctx)
	if err != nil {
		return "", err
	}
	st, err := r.store.Load()
	if err != nil {
		return "", err
	}
	return report.RenderMarkdown(issues, *st, r.now()), nil
}

func (r *Runner) CurrentValidationMarkdown(ctx context.Context) (string, error) {
	st, err := r.store.Load()
	if err != nil {
		return "", err
	}
	current := buildCurrentValidation(nil, *st)
	if current == nil {
		return "No issue is currently awaiting manual validation.\n", nil
	}
	return renderCurrentValidationMarkdown(*current), nil
}

func (r *Runner) Pause(ctx context.Context, numbers []string, reason string) error {
	return r.store.WithLock(ctx, func(st *domain.State) error {
		for _, number := range numbers {
			meta := st.Issues[number]
			meta.Paused = true
			meta.PauseReason = reason
			st.Issues[number] = meta
			r.runHook(ctx, "onIssuePaused", r.cfg.Hooks.OnIssuePaused, issueFromState(number, meta), meta)
		}
		return nil
	})
}

func (r *Runner) Resume(ctx context.Context, numbers []string) error {
	return r.store.WithLock(ctx, func(st *domain.State) error {
		for _, number := range numbers {
			meta := st.Issues[number]
			meta.Paused = false
			meta.PauseReason = ""
			meta.NextRetryAt = 0
			st.Issues[number] = meta
		}
		return nil
	})
}

func (r *Runner) ClearRemoteRunningLabels(ctx context.Context, numbers []string) error {
	for _, number := range numbers {
		if err := r.provider.RemoveLabels(ctx, number, []string{r.cfg.Labels.Running}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) ResetAttempts(ctx context.Context, numbers []string) error {
	return r.store.WithLock(ctx, func(st *domain.State) error {
		for _, number := range numbers {
			meta := st.Issues[number]
			meta.Attempts = 0
			meta.NextRetryAt = 0
			st.Issues[number] = meta
		}
		return nil
	})
}

func (r *Runner) StopWorker(ctx context.Context, number string) error {
	return r.store.WithLock(ctx, func(st *domain.State) error {
		worker, ok := st.Workers[number]
		if !ok {
			return fmt.Errorf("worker for issue %s not found", number)
		}
		if err := r.executor.Terminate(ctx, worker); err != nil {
			return err
		}
		delete(st.Workers, number)
		return nil
	})
}
