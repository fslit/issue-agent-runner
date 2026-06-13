# IssueAgent Runner MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go MVP that can safely replace the existing Python CNB issue runner for the zcode workspace.

**Architecture:** Keep the runner core deterministic and platform-agnostic: scheduling, state, retry, pause, stale-worker detection, and progress reporting live in `internal/runner`, `internal/state`, and `internal/report`. CNB, Codex, browser verification, and hooks are adapters behind small interfaces. The MVP must preserve the existing `.local_cache/cnb-issue-runner` state shape while adding versioned fields in an additive way.

**Tech Stack:** Go 1.22+, cobra, `gopkg.in/yaml.v3`, `log/slog`, `net/http`, `os/exec`, `github.com/gofrs/flock`, `testify`.

---

## Scope

MVP includes:

- `run`, `once`, `status`, `summary`, `pause`, `resume`, `reset-attempts`, `stop-worker`.
- YAML config loading with defaults and validation.
- Legacy runner state compatibility.
- CNB provider for list/get/labels/comments.
- Codex executor with process group handling, log file, prompt file, and result file.
- Core scheduler with max concurrency, retry backoff, capacity backoff, quota/auth pause, max attempts pause, and stale terminate.
- Markdown and JSON progress summaries.
- Unit tests for core deterministic logic and integration tests with fake provider/executor.

Out of MVP:

- Browser verification implementation.
- GitHub/GitLab providers.
- HTTP webhook and external command hooks.
- launchd/systemd templates.
- Web UI or database-backed queue.

## File Structure

Create:

- `go.mod`: module definition and dependencies.
- `cmd/issue-agent-runner/main.go`: CLI entrypoint.
- `internal/config/config.go`: config structs, defaults, validation, path resolution.
- `internal/config/config_test.go`: config tests.
- `internal/domain/types.go`: shared issue, task, worker, result, error-kind, and state transition types.
- `internal/state/store.go`: lock, load, save, atomic write, version migration.
- `internal/state/store_test.go`: state compatibility and atomic write tests.
- `internal/provider/provider.go`: provider interface.
- `internal/provider/cnb/client.go`: CNB API client.
- `internal/provider/cnb/client_test.go`: CNB request/response tests using `httptest`.
- `internal/executor/executor.go`: executor interface.
- `internal/executor/codex/executor.go`: Codex process executor.
- `internal/executor/codex/executor_test.go`: command construction and result parsing tests.
- `internal/prompt/render.go`: Go template rendering.
- `internal/prompt/render_test.go`: prompt rendering tests.
- `internal/runner/runner.go`: scheduler orchestration.
- `internal/runner/policy.go`: candidate, retry, pause, and error-classification policy.
- `internal/runner/policy_test.go`: deterministic policy tests.
- `internal/runner/runner_test.go`: fake provider/executor integration tests.
- `internal/report/report.go`: Markdown/JSON progress rendering.
- `internal/report/report_test.go`: report tests.
- `internal/logx/logx.go`: slog construction helpers.

Modify:

- `README.md`: add quick start and implementation plan link.
- `configs/runner.example.yaml`: align state directory with zcode migration.
- `templates/worker_prompt.md`: define worker result ownership and avoid remote label/comment side effects.

## State Compatibility Contract

The Go runner must load the current Python runner state:

```json
{
  "workers": {},
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
      "title": "..."
    }
  }
}
```

The Go runner may add `version`, `errorKind`, `promptPath`, `logPath`, `resultPath`, and RFC3339 timestamps, but it must continue to accept Unix integer timestamps and the legacy worker `log` field.

## Task 1: Go Module And CLI Skeleton

**Files:**

- Create: `go.mod`
- Create: `cmd/issue-agent-runner/main.go`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`

**Acceptance criteria:**

- `issue-agent-runner --help` lists all MVP commands.
- `once -c configs/runner.example.yaml --dry-run` loads config and exits without network or worker side effects.
- Config validation rejects missing workspace, unsupported provider, unsupported executor, and invalid concurrency.

**Steps:**

- [ ] Create `go.mod` with module path `issue-agent-runner` and dependencies:

```go
module issue-agent-runner

go 1.22

require (
	github.com/gofrs/flock v0.12.1
	github.com/spf13/cobra v1.8.1
	github.com/stretchr/testify v1.9.0
	gopkg.in/yaml.v3 v3.0.1
)
```

- [ ] Add config structs with explicit defaults:

```go
type Config struct {
	Project  ProjectConfig  `yaml:"project"`
	Provider ProviderConfig `yaml:"provider"`
	Executor ExecutorConfig `yaml:"executor"`
	Runner   RunnerConfig   `yaml:"runner"`
	Labels   LabelConfig    `yaml:"labels"`
	Paths    PathConfig     `yaml:"paths"`
	Browser  BrowserConfig  `yaml:"browser"`
	Hooks    HookConfig      `yaml:"hooks"`
}

type RunnerConfig struct {
	IntervalSeconds        int `yaml:"intervalSeconds"`
	MaxConcurrency         int `yaml:"maxConcurrency"`
	MaxAttempts            int `yaml:"maxAttempts"`
	RetryBackoffSeconds    int `yaml:"retryBackoffSeconds"`
	CapacityBackoffSeconds int `yaml:"capacityBackoffSeconds"`
	QuotaBackoffSeconds    int `yaml:"quotaBackoffSeconds"`
	StaleWorkerSeconds     int `yaml:"staleWorkerSeconds"`
}
```

- [ ] Add `Load(path string) (*Config, error)` that applies these defaults before YAML decode:

```go
Runner: RunnerConfig{
	IntervalSeconds:        60,
	MaxConcurrency:         5,
	MaxAttempts:            3,
	RetryBackoffSeconds:    300,
	CapacityBackoffSeconds: 900,
	QuotaBackoffSeconds:    3600,
	StaleWorkerSeconds:     7200,
},
Labels: LabelConfig{
	Running:    "处理中",
	Done:       "已处理",
	Failed:     "处理失败",
	NeedsHuman: "需人工确认",
	Skip:       []string{"已处理", "处理中", "处理失败", "需人工确认"},
},
```

- [ ] Add tests:

```bash
rtk go test ./internal/config -count=1
```

Expected: tests pass and invalid config cases return errors.

- [ ] Add cobra commands with no-op handlers for this task:

```text
run
once
status
summary
pause
resume
reset-attempts
stop-worker
```

- [ ] Verify:

```bash
rtk go test ./...
rtk go run ./cmd/issue-agent-runner --help
rtk go run ./cmd/issue-agent-runner once -c configs/runner.example.yaml --dry-run
```

- [ ] Commit:

```bash
rtk git add go.mod cmd internal/config
rtk git commit -m "feat: scaffold issue runner CLI and config"
```

## Task 2: Domain Types And State Store

**Files:**

- Create: `internal/domain/types.go`
- Create: `internal/state/store.go`
- Create: `internal/state/store_test.go`

**Acceptance criteria:**

- Loads existing Python state without `version`.
- Saves state atomically through lock + temp file + rename.
- Preserves unknown issue metadata fields when rewriting.
- Accepts both legacy worker `log` and new worker `logPath`.

**Steps:**

- [ ] Define additive domain types:

```go
type ErrorKind string

const (
	ErrorNone          ErrorKind = "NONE"
	ErrorModelCapacity ErrorKind = "MODEL_CAPACITY"
	ErrorQuota         ErrorKind = "QUOTA"
	ErrorAuth          ErrorKind = "AUTH"
	ErrorTestFailure   ErrorKind = "TEST_FAILURE"
	ErrorStale         ErrorKind = "STALE"
	ErrorUnknown       ErrorKind = "UNKNOWN"
)

type State struct {
	Version int                    `json:"version,omitempty"`
	Workers map[string]WorkerState `json:"workers"`
	Issues  map[string]IssueState  `json:"issues,omitempty"`
}

type WorkerState struct {
	IssueNumber string `json:"issueNumber,omitempty"`
	PID         int    `json:"pid"`
	Title       string `json:"title,omitempty"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	Log         string `json:"log,omitempty"`
	LogPath     string `json:"logPath,omitempty"`
	PromptPath  string `json:"promptPath,omitempty"`
	ResultPath  string `json:"resultPath,omitempty"`
}

type IssueState struct {
	Attempts       int       `json:"attempts,omitempty"`
	Paused         bool      `json:"paused,omitempty"`
	PauseReason    string    `json:"pauseReason,omitempty"`
	LastExitCode   *int      `json:"lastExitCode,omitempty"`
	LastError      string    `json:"lastError,omitempty"`
	ErrorKind      ErrorKind `json:"errorKind,omitempty"`
	LastStartedAt  int64     `json:"lastStartedAt,omitempty"`
	LastFinishedAt int64     `json:"lastFinishedAt,omitempty"`
	NextRetryAt    int64     `json:"nextRetryAt,omitempty"`
	Title          string    `json:"title,omitempty"`
}
```

- [ ] Implement `Store.WithLock(ctx, func(*State) error) error` using `flock`.
- [ ] Implement `Load` so missing files produce `{workers:{}, issues:{}}`.
- [ ] Implement `Save` with `state.json.tmp` then rename.
- [ ] Add tests that load a literal legacy state JSON and assert fields survive.
- [ ] Verify:

```bash
rtk go test ./internal/state -count=1
rtk go test ./...
```

- [ ] Commit:

```bash
rtk git add internal/domain internal/state
rtk git commit -m "feat: add compatible state store"
```

## Task 3: Runner Policy

**Files:**

- Create: `internal/runner/policy.go`
- Create: `internal/runner/policy_test.go`

**Acceptance criteria:**

- Candidate filtering handles open issues, skip labels, active workers, paused issues, retry time, and orphan `处理中` labels.
- Error classification distinguishes capacity, quota, auth, stale, test failure, and unknown.
- Failure handling applies configured backoff and pauses at max attempts.

**Steps:**

- [ ] Define issue model:

```go
type Issue struct {
	Number string
	Title  string
	Body   string
	State  string
	Labels []string
	URL    string
}
```

- [ ] Implement `IsCandidate(issue Issue, state State, labels LabelConfig, now time.Time) bool`.
- [ ] Keep the legacy orphan-running behavior: if an issue has the running label but no active worker and its retry time is ready, it may be re-claimed.
- [ ] Implement `ClassifyError(message string, exitCode *int) ErrorKind`.
- [ ] Implement `ApplyFailure(issueNumber string, worker WorkerState, result WorkerResult, now time.Time, cfg RunnerConfig, state *State)`.
- [ ] Add tests matching the Python runner test cases plus quota/auth pause cases.
- [ ] Verify:

```bash
rtk go test ./internal/runner -run 'Test.*Policy|Test.*Failure|Test.*Candidate' -count=1
rtk go test ./...
```

- [ ] Commit:

```bash
rtk git add internal/runner
rtk git commit -m "feat: add deterministic runner policy"
```

## Task 4: Prompt Rendering And Report Output

**Files:**

- Create: `internal/prompt/render.go`
- Create: `internal/prompt/render_test.go`
- Create: `internal/report/report.go`
- Create: `internal/report/report_test.go`

**Acceptance criteria:**

- Prompt template receives repo URL, issue fields, command prefix, result path, and artifact paths.
- `progress-summary.md` matches the existing human-readable sections.
- `progress-summary.json` exposes the same counts and lists for scripts.

**Steps:**

- [ ] Implement prompt rendering with `text/template` and `missingkey=error`.
- [ ] Implement Markdown report sections:

```text
Updated
Open issues
Active workers
Retry backoff issues
Paused issues
Active
Waiting For Retry
Paused
```

- [ ] Implement JSON report:

```json
{
  "updatedAt": "2026-05-08T08:44:28+08:00",
  "openIssues": 26,
  "activeWorkers": [],
  "waitingRetry": [],
  "pausedIssues": []
}
```

- [ ] Verify:

```bash
rtk go test ./internal/prompt ./internal/report -count=1
rtk go test ./...
```

- [ ] Commit:

```bash
rtk git add internal/prompt internal/report
rtk git commit -m "feat: add prompt and progress rendering"
```

## Task 5: Fake Provider/Executor Runner Integration

**Files:**

- Create: `internal/provider/provider.go`
- Create: `internal/executor/executor.go`
- Create: `internal/runner/runner.go`
- Create: `internal/runner/runner_test.go`

**Acceptance criteria:**

- `Once` can claim an issue, start a fake worker, update state, and write reports.
- Existing active workers count against concurrency.
- Completed workers clear active state and update issue metadata.
- Remote done labels release workers.

**Steps:**

- [ ] Define provider interface with additive label operations:

```go
type IssueProvider interface {
	ListOpen(ctx context.Context) ([]domain.Issue, error)
	Get(ctx context.Context, number string) (domain.Issue, error)
	AddLabels(ctx context.Context, number string, labels []string) error
	RemoveLabels(ctx context.Context, number string, labels []string) error
	AddComment(ctx context.Context, number string, body string) error
}
```

- [ ] Define executor interface:

```go
type Executor interface {
	Start(ctx context.Context, task domain.Task) (domain.WorkerState, error)
	Alive(ctx context.Context, worker domain.WorkerState) bool
	Result(ctx context.Context, worker domain.WorkerState) domain.WorkerResult
	Terminate(ctx context.Context, worker domain.WorkerState) error
}
```

- [ ] Implement `Runner.Once(ctx)` in this order:

```text
load locked state
list open issues
release workers whose issue has done label
prune finished/stale workers
calculate capacity
filter candidates
claim candidates by adding running label
render prompt and start executor
save state
write markdown/json reports
```

- [ ] Add fake integration tests for success, failure, stale, max attempts, and resume-ready issue.
- [ ] Verify:

```bash
rtk go test ./internal/runner -count=1
rtk go test ./...
```

- [ ] Commit:

```bash
rtk git add internal/provider internal/executor internal/runner
rtk git commit -m "feat: run scheduler with fake adapters"
```

## Task 6: CNB Provider

**Files:**

- Create: `internal/provider/cnb/client.go`
- Create: `internal/provider/cnb/client_test.go`
- Modify: `internal/config/config.go`

**Acceptance criteria:**

- Supports token from git remote for zcode.
- Lists open issues with pagination.
- Gets one issue before claim.
- Adds/removes labels without overwriting unrelated labels.
- Adds comments with structured error handling.

**Steps:**

- [ ] Implement git remote token parsing compatible with:

```text
https://cnb:<token>@cnb.cool/<owner>/<group>/<repo>.git
```

- [ ] Implement CNB request helper with timeout and structured error:

```go
type ProviderError struct {
	StatusCode int
	Body       string
	Kind       domain.ErrorKind
}
```

- [ ] Treat 401/403 as `AUTH`, 429 as `QUOTA`, and 5xx as retryable `UNKNOWN`.
- [ ] Validate third-party JSON response shape before converting to `domain.Issue`.
- [ ] Add `httptest` cases for pagination, label merge, auth failure, and malformed JSON.
- [ ] Verify:

```bash
rtk go test ./internal/provider/cnb -count=1
rtk go test ./...
```

- [ ] Commit:

```bash
rtk git add internal/provider/cnb internal/config
rtk git commit -m "feat: add CNB issue provider"
```

## Task 7: Codex Executor

**Files:**

- Create: `internal/executor/codex/executor.go`
- Create: `internal/executor/codex/executor_test.go`
- Modify: `templates/worker_prompt.md`

**Acceptance criteria:**

- Starts Codex in the configured workspace with configured sandbox and approval policy.
- Writes prompt, log, and result paths under state directory.
- Uses process groups so stale termination kills child processes.
- Parses result JSON when present and falls back to log marker classification.

**Steps:**

- [ ] Build the command from config:

```text
codex --ask-for-approval never exec --cd <workspace> --sandbox danger-full-access -
```

- [ ] Include `--skip-git-repo-check` only when config explicitly enables it.
- [ ] Write log markers:

```text
=== worker start <timestamp> ===
=== worker exit code <code> <timestamp> ===
```

- [ ] Require the prompt to instruct workers to write:

```json
{
  "status": "success",
  "summary": "short human summary",
  "verification": ["rtk go test ./internal/runner -count=1"],
  "changedFiles": ["internal/runner/runner.go"],
  "errorKind": "NONE",
  "errorMessage": ""
}
```

- [ ] Add tests for command args, result JSON parsing, missing result fallback, and stale terminate.
- [ ] Verify:

```bash
rtk go test ./internal/executor/codex -count=1
rtk go test ./...
```

- [ ] Commit:

```bash
rtk git add internal/executor/codex templates/worker_prompt.md
rtk git commit -m "feat: add Codex executor"
```

## Task 8: CLI Commands Wire-Up

**Files:**

- Modify: `cmd/issue-agent-runner/main.go`
- Modify: `internal/runner/runner.go`

**Acceptance criteria:**

- `once` runs one scheduling pass.
- `run` loops with interval and handles SIGINT/SIGTERM.
- `status` prints active, waiting, and paused counts.
- `summary` prints the Markdown summary path and content.
- `pause`, `resume`, `reset-attempts`, and `stop-worker` mutate state safely under lock.

**Steps:**

- [ ] Wire adapter construction from config.
- [ ] Implement `run` with context cancellation and interval sleep.
- [ ] Implement state mutation commands without contacting CNB except when `resume --clear-remote-running-label` is used.
- [ ] Verify:

```bash
rtk go run ./cmd/issue-agent-runner once -c configs/runner.example.yaml --dry-run
rtk go run ./cmd/issue-agent-runner status -c configs/runner.example.yaml
rtk go test ./...
```

- [ ] Commit:

```bash
rtk git add cmd internal/runner
rtk git commit -m "feat: wire runner CLI commands"
```

## Task 9: Migration Dry Run

**Files:**

- Modify: `README.md`
- Modify: `configs/runner.example.yaml`

**Acceptance criteria:**

- Go runner can read the current zcode `.local_cache/cnb-issue-runner/state.json`.
- `once --dry-run` produces the same candidate/capacity summary as Python without starting workers.
- No remote labels or comments are changed during dry run.

**Steps:**

- [ ] Add README quick start:

```bash
rtk go test ./...
rtk go run ./cmd/issue-agent-runner once -c configs/runner.example.yaml --dry-run
rtk go run ./cmd/issue-agent-runner status -c configs/runner.example.yaml
```

- [ ] Run Go dry run:

```bash
rtk go run ./cmd/issue-agent-runner once -c configs/runner.example.yaml --dry-run
```

- [ ] Compare open issue count, candidate count, active worker count, paused count, and retry count.
- [ ] Commit:

```bash
rtk git add README.md configs/runner.example.yaml
rtk git commit -m "docs: document migration dry run"
```

## Checkpoints

After Tasks 1-3:

- [ ] `rtk go test ./internal/config ./internal/state ./internal/runner -count=1`
- [ ] Candidate, retry, pause, and stale policy are covered before external API work starts.

After Tasks 4-5:

- [ ] `rtk go test ./internal/prompt ./internal/report ./internal/runner -count=1`
- [ ] A fake provider/executor can complete one scheduling pass.

After Tasks 6-8:

- [ ] `rtk go test ./...`
- [ ] `rtk go run ./cmd/issue-agent-runner once -c configs/runner.example.yaml --dry-run`
- [ ] No live worker or remote mutation happens in dry run.

Before replacing Python:

- [ ] Stop Python daemon.
- [ ] Run Go `once --dry-run`.
- [ ] Run Go `once` with `maxConcurrency: 1`.
- [ ] Observe logs and progress summary.
- [ ] Restore `maxConcurrency: 5` only after one successful issue cycle.

## Risks And Mitigations

| Risk | Impact | Mitigation |
| --- | --- | --- |
| CNB API label update overwrites unrelated labels | High | Use `AddLabels`/`RemoveLabels` semantics, tested with existing labels. |
| Legacy state mismatch causes duplicate workers | High | Load old state in tests and keep `.local_cache/cnb-issue-runner` during migration. |
| Worker marks remote issue while runner also marks it | Medium | Worker writes result only; runner owns labels/comments. |
| Capacity/quota errors burn repeated attempts | High | Classify before generic failure and pause quota/auth immediately. |
| Stale termination leaves child processes | Medium | Start workers in process groups and terminate group first. |
| Browser verification scope delays MVP | Medium | Keep browser adapter interface only; implement in Phase 3. |

## Open Questions

- Should `resume --clear-remote-running-label` also remove `处理失败`, or only `处理中`?
- Should successful workers remove `处理中` before adding `已处理`, or replace both labels in one CNB call if the API supports it?
- Should quota pause be global for the runner, or per issue in MVP? Recommended MVP: global pause timestamp in state after quota/auth provider or executor errors.

## Self-Review

- Spec coverage: MVP goals from architecture sections 2, 7, 9, 10, 14, 15, 17, 19, and 20 are mapped to tasks.
- Placeholder scan: no task uses forbidden placeholder patterns.
- Type consistency: provider uses additive label methods across architecture and implementation tasks; state keeps legacy field names plus additive new fields.
