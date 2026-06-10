# IssueAgent Runner

IssueAgent Runner is an issue-driven agent runner for isolated fixes, baseline
integration, and verification workflow.

It is a reusable Go-based issue automation runner.

It polls issue platforms such as CNB or GitHub, dispatches AI workers such as
Codex with bounded concurrency, tracks retry/backoff state, pauses repeated
failures, writes progress summaries, and can drive an issue closure workflow
based on isolated worktrees, local baseline integration, manual verification,
push-on-close, and cleanup.

## Project Goals

- Keep the runner core deterministic: scheduling, state, retries, locks, and worker lifecycle.
- Keep project-specific behavior extensible through providers, executors, prompt templates, and hooks.
- Avoid wasting AI tokens with max-attempt limits, backoff, stale-worker detection, and pause controls.
- Support browser verification as an optional acceptance stage for frontend issues.
- Keep shared verification simple: users validate the normal baseline branches, not per-issue branches.

## Planned Commands

```bash
issue-agent-runner run -c configs/runner.example.yaml
issue-agent-runner once -c configs/runner.example.yaml
issue-agent-runner status -c configs/runner.example.yaml
issue-agent-runner summary -c configs/runner.example.yaml
issue-agent-runner current -c configs/runner.example.yaml
issue-agent-runner pause -c configs/runner.example.yaml 35
issue-agent-runner resume -c configs/runner.example.yaml 35 36 --clear-remote-running-label
issue-agent-runner reject -c configs/runner.example.yaml 35
```

## Workflow

1. The runner claims an open issue and creates local issue worktrees for each configured repo.
2. Codex works only inside those issue worktrees and commits locally on issue branches.
3. When the worker succeeds, the runner records structured git metadata and marks the issue `READY_TO_INTEGRATE`.
4. The runner merges at most one ready issue at a time into the local baseline branches:
   - `ml-svc-zcode -> ai-v2.0`
   - `ml-web-zcode -> ai-v2.0`
5. Users verify the normal baseline environment. They do not need to know the issue branch names.
6. If verification fails, run `issue-agent-runner reject ISSUE` to revert the pending baseline merge commit and return the issue to the queue.
7. If verification passes, close the CNB issue. The runner detects closure, pushes the touched baseline branches, comments the pushed SHAs, and cleans local issue branches/worktrees.

## Quick Start

```bash
rtk go test ./...
rtk go run ./cmd/issue-agent-runner --help
rtk go run ./cmd/issue-agent-runner once -c configs/runner.example.yaml --dry-run
rtk go run ./cmd/issue-agent-runner status -c configs/runner.example.yaml
```

## Legacy Python Runner

The legacy Python CNB runner is kept under `automation/` for zcode migration
and rollback scenarios:

```bash
rtk python3 -m unittest automation.test_cnb_issue_runner
rtk python3 automation/cnb_issue_runner.py --workspace /path/to/workspace --dry-run
rtk ./automation/start_cnb_issue_runner_daemon.sh start
```

It works directly in the currently checked-out workspace branches and does not
create per-issue branches or worktrees.

During the zcode migration, `configs/runner.example.yaml` intentionally points at the
legacy Python runner state directory: `.local_cache/cnb-issue-runner`.

Compare Python and Go dry runs before replacing the daemon:

```bash
rtk python3 /Users/m4/Documents/code/zcode/automation/cnb_issue_runner.py --workspace /Users/m4/Documents/code/zcode --dry-run
rtk go run ./cmd/issue-agent-runner once -c configs/runner.example.yaml --dry-run
```

Runner process events are appended to `paths.runnerLog` as JSONL. Each line is a
structured event such as `poll_start`, `worker_started`, `merge_conflict`,
`awaiting_verify`, `pushed`, or `issue_cleaned`.

## Local Runbook

For the current zcode workspace, the private local config is:

```bash
/Users/m4/Documents/code/zcode/issue-agent-runner/.local_cache/cnb-issue-runner/runner.local.yaml
```

Typical operations:

```bash
cd /Users/m4/Documents/code/zcode/issue-agent-runner

# One real polling pass
rtk go run ./cmd/issue-agent-runner once -c .local_cache/cnb-issue-runner/runner.local.yaml

# Status / summary
rtk go run ./cmd/issue-agent-runner status -c .local_cache/cnb-issue-runner/runner.local.yaml
rtk go run ./cmd/issue-agent-runner summary -c .local_cache/cnb-issue-runner/runner.local.yaml
rtk go run ./cmd/issue-agent-runner current -c .local_cache/cnb-issue-runner/runner.local.yaml

# Run as daemon in tmux
tmux new-session -d -s issue-agent-runner \
  "cd /Users/m4/Documents/code/zcode/issue-agent-runner && \
   rtk go run ./cmd/issue-agent-runner run -c .local_cache/cnb-issue-runner/runner.local.yaml"

# Attach / stop
tmux attach -t issue-agent-runner
tmux kill-session -t issue-agent-runner
```

Runtime files:

```bash
/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/progress-summary.md
/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/runner.log
/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/current-validation.md
/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/current-validation.json
```

`current-validation.*` is created only while an issue is merged into the local
baseline branches and is waiting for human verification. It records the current
issue number, touched baseline branches, merge commits, worker summary, and
worker verification commands. It is removed after `reject` or after the issue is
closed, pushed, and cleaned.

## CNB Callback

The runner can expose a small HTTP callback endpoint so CNB webhooks or other
automation can wake the runner immediately when issue state changes or new work
arrives. The callback does not bypass scheduling rules: it only triggers one
polling pass, and `runner.maxConcurrency` still controls how many workers can be
started.

Config:

```yaml
callback:
  enabled: true
  listen: 127.0.0.1:8791
  path: /callbacks/cnb
  secret: change-me
  secretHeader: X-Issue-Agent-Token
```

Manual trigger:

```bash
curl -X POST \
  -H 'X-Issue-Agent-Token: change-me' \
  http://127.0.0.1:8791/callbacks/cnb
```

CNB should be configured to POST to this endpoint. If CNB cannot reach localhost
directly, expose it through an internal reverse proxy or tunnel and keep the
secret header enabled.

## Integration Hooks

The runner supports command hooks and HTTP webhook hooks. Both are best-effort:
failures are logged but do not roll back runner state.

```yaml
hooks:
  onIssueClaim:
    command: ""
    timeoutSeconds: 30
  onIssuePaused:
    webhook:
      url: ""
      method: POST
  onProgressUpdate:
    command: ""
    timeoutSeconds: 30
  onIssueIntegrated:
    command: "rtk ./scripts/restart-validation-env.sh"
    timeoutSeconds: 180
  onIssueRejected:
    command: ""
    timeoutSeconds: 180
  onIssueClosedPushed:
    command: ""
    timeoutSeconds: 60
```

Hook commands run from `project.workspace`. Command failures are logged as
`hook_failed`; HTTP webhook failures are logged as `webhook_failed`. Available
command environment variables and webhook JSON fields:

```text
ISSUE_AGENT_EVENT
ISSUE_AGENT_ISSUE_NUMBER
ISSUE_AGENT_ISSUE_TITLE
ISSUE_AGENT_STATE_DIR
ISSUE_AGENT_WORKSPACE
ISSUE_AGENT_MODEL
ISSUE_AGENT_REASONING_EFFORT
ISSUE_AGENT_WORKFLOW_STATUS
```

## Codex Model Selection

Current behavior:

- The runner can now pass both `executor.model` and `executor.reasoningEffort` to `codex exec`.
- `executor.model` is passed as `--model <name>`.
- `executor.reasoningEffort` is passed as `-c model_reasoning_effort="<level>"`.

The current argument builder is effectively:

```bash
codex exec \
  --cd <workspace> \
  --sandbox <mode> \
  --ask-for-approval <mode> \
  [--model <model>] \
  [-c model_reasoning_effort="<level>"] \
  [--skip-git-repo-check] \
  -
```

Notes:

- Example:

```yaml
executor:
  type: codex
  bin: /Users/m4/.local/bin/codex
  model: gpt-5.5
  sandbox: danger-full-access
  approval: never
  reasoningEffort: high
```

- If `executor.model` is empty, Codex falls back to the local CLI default model.
- `codex exec --help` on this machine confirms direct `--model` support.
- The installed Codex CLI also exposes the config key `model_reasoning_effort`, which is what the runner now uses for reasoning level control.

## Documents

- [Usage Guide](docs/usage.zh-CN.md)
- [Architecture Plan](docs/architecture-plan.zh-CN.md)
- [MVP Implementation Plan](docs/superpowers/plans/2026-05-08-issue-agent-runner-mvp.md)
- [Integration Closure Plan](docs/superpowers/plans/2026-05-15-issue-integration-closure.md)
- [Example Config](configs/runner.example.yaml)
- [Worker Prompt Template](templates/worker_prompt.md)
