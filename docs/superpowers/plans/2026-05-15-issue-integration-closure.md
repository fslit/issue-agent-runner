# Issue Integration Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend `issue-agent-runner` so AI workers fix issues in isolated worktrees, merge successful fixes into local baseline branches one issue at a time for human verification, then push baseline branches and clean up temporary branches/worktrees after the CNB issue is closed.

**Architecture:** Keep concurrent issue analysis and coding on isolated per-issue branches, but serialize baseline integration so the shared verification environment always contains at most one unverified issue. The runner remains the single authority for git integration, CNB comments/labels, retry state, verification gate state, push-after-close, merge conflict handling, and cleanup.

**Tech Stack:** Go 1.22+, existing Cobra CLI, CNB provider, Codex executor, local git via `os/exec`, JSON state store, markdown summaries.

---

## Scope

This plan covers:

- Multi-repo config for `ml-svc-zcode` and `ml-web-zcode`.
- Per-issue local worktree and branch lifecycle.
- Worker prompt/result contract updates so workers commit on issue branches.
- Serial merge queue into local baseline branches:
  - `ml-svc-zcode -> ai-v2.0`
  - `ml-web-zcode -> ai-v2.0`
- Single pending-human-verification slot on baseline branches.
- CNB issue comments for merge success, merge conflict, reject/retry, push-on-close, and cleanup.
- `close -> push baseline branch -> comment -> cleanup local issue branches/worktrees` flow.
- Explicit reject command to revert the single pending verification issue from local baseline branches.

Out of scope for this phase:

- Remote issue-branch pushes.
- Auto-opening GitHub PRs or CNB merge requests.
- Browser-based verification automation beyond the existing optional verifier.
- Running multiple verification environments.

## Working Model

1. Worker fixes issue in isolated worktree branches and commits locally.
2. Runner reads worker result and queues the issue for integration.
3. Runner integrates at most one queued issue into the local baseline branches.
4. Human verifies against the normal local environment built from baseline branches.
5. If verification fails, operator runs `reject ISSUE`, runner reverts the baseline merge commit and requeues the issue.
6. If verification passes, user closes the CNB issue.
7. Runner detects closure, pushes touched baseline branches, comments the final fix summary, and deletes local issue branch/worktree state.

This preserves one shared verification environment without requiring users to know branch names, while still keeping issue edits isolated during coding.

## File Structure

Modify:

- `cmd/issue-agent-runner/main.go`
  - Add verification/integration management commands.
- `configs/runner.example.yaml`
  - Replace single-repo config with managed repo list and integration settings.
- `templates/worker_prompt.md`
  - Instruct workers to operate in issue worktrees, commit on issue branches, and write structured git metadata to result files.
- `internal/config/config.go`
  - Add managed repositories, worktree root, baseline integration policy, and cleanup settings.
- `internal/config/config_test.go`
  - Cover new config shape and validation.
- `internal/domain/types.go`
  - Add repo/worktree/integration/result/state types.
- `internal/state/store.go`
  - Persist new integration state.
- `internal/state/store_test.go`
  - Cover migration/additive compatibility.
- `internal/runner/runner.go`
  - Add worktree provisioning, integration queue, verification gate, close handling, push, and cleanup orchestration.
- `internal/runner/policy.go`
  - Add candidate filters for ready-to-integrate, awaiting-verify, rejected, and cleanup states.
- `internal/runner/policy_test.go`
  - Cover new state transitions.
- `internal/runner/runner_test.go`
  - Cover integration queue, conflict comment flow, reject flow, close push flow, and cleanup flow.
- `internal/provider/provider.go`
  - Keep interface stable or minimally extend if needed for closed-issue refresh.
- `internal/provider/cnb/client.go`
  - Ensure `Get` is used for pending verification issues that may become closed.
- `README.md`
  - Document the new workflow and operator commands.

Create:

- `internal/gitops/manager.go`
  - Local git operations for worktree creation, branch creation, merge, revert, push, and cleanup.
- `internal/gitops/manager_test.go`
  - Git behavior tests with temp repositories.
- `internal/integration/comments.go`
  - CNB comment builders for conflict, merged-awaiting-verify, reject, pushed, and cleanup events.
- `internal/integration/comments_test.go`
  - Comment rendering tests.

## Architecture Decisions

- **Decision 1: Keep issue coding isolated in local worktrees.**
  Rationale: this prevents concurrent issue edits from contaminating each other before integration.

- **Decision 2: Only one issue may be merged into local baseline branches and wait for human verification at a time.**
  Rationale: the user validates the normal baseline environment and must be able to attribute changes to a single issue.

- **Decision 3: Push only the touched baseline branches after CNB issue closure, not before.**
  Rationale: human validation happens on the local baseline branches first; remote branches are updated only after acceptance.

- **Decision 4: Verification failure is handled by explicit operator action (`reject ISSUE`) that reverts the single pending baseline merge commit.**
  Rationale: the runner cannot infer human rejection from CNB state alone.

- **Decision 5: Merge conflicts are surfaced directly on the CNB issue and pause further automation for that issue.**
  Rationale: conflict visibility must be user-facing and traceable in the same tool where the issue is managed.

## Config Model

The current single `project.serviceRepo` model is not sufficient. Replace it with managed repositories and integration settings.

Planned config shape:

```yaml
project:
  name: zcode
  workspace: /Users/m4/Documents/code/zcode

repositories:
  - name: ml-svc-zcode
    path: ml-svc-zcode
    issueBranchPrefix: cnb
    baselineBranch: ai-v2.0
  - name: ml-web-zcode
    path: ml-web-zcode
    issueBranchPrefix: cnb
    baselineBranch: ai-v2.0

integration:
  worktreeRoot: .local_worktrees/issue-agent-runner
  mergeMode: no-ff
  pushOnClose: true
  cleanupOnClose: true
  allowSinglePendingVerify: true
```

## State Model Additions

Add additive state for:

- `issue workflow status`
  - `RUNNING`
  - `READY_TO_INTEGRATE`
  - `AWAITING_VERIFY`
  - `MERGE_CONFLICT`
  - `VERIFY_REJECTED`
  - `CLOSED_PUSHED`
  - `CLEANED`
- per-repo issue branch metadata:
  - repo name
  - worktree path
  - issue branch
  - baseline branch
  - issue branch head SHA
  - baseline merge commit SHA
  - pushed baseline SHA
- queue ordering:
  - `readyAt`
  - `integratedAt`
  - `rejectedAt`
- audit metadata:
  - conflict file list
  - pushed branch list
  - cleanup timestamp

## Task List

### Phase 1: Foundation

### Task 1: Expand config and domain types for integration workflow

**Description:** Replace the single-service-repo model with a managed multi-repo integration model and add the domain/state types needed for worktrees, issue branches, baseline merges, verification gate state, and cleanup.

**Acceptance criteria:**

- [ ] Config supports `repositories` and `integration` blocks.
- [ ] Existing config validation still fails fast on missing workspace and unsupported provider/executor.
- [ ] State types can represent issue branch metadata, merge metadata, and pending verification status without breaking old state loading.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/config ./internal/domain ./internal/state -count=1`
- [ ] Manual check: `cd issue-agent-runner && rtk go run ./cmd/issue-agent-runner once -c configs/runner.example.yaml --dry-run`

**Dependencies:** None

**Files likely touched:**

- `issue-agent-runner/internal/config/config.go`
- `issue-agent-runner/internal/config/config_test.go`
- `issue-agent-runner/internal/domain/types.go`
- `issue-agent-runner/internal/state/store.go`
- `issue-agent-runner/internal/state/store_test.go`
- `issue-agent-runner/configs/runner.example.yaml`

**Estimated scope:** Medium

### Task 2: Add git worktree and baseline integration manager

**Description:** Introduce a focused `gitops` package that owns all local git mutations: creating issue branches/worktrees, checking branch cleanliness, merging an issue branch into the local baseline branch with `--no-ff`, reverting the pending verification merge, pushing baseline branches, and deleting temporary branches/worktrees.

**Acceptance criteria:**

- [ ] Can create an issue worktree from a configured baseline branch.
- [ ] Can merge issue branch into baseline branch and return merge commit SHA.
- [ ] Can detect and report merge conflicts with conflicted file paths.
- [ ] Can revert a merge commit from the local baseline branch.
- [ ] Can push configured baseline branches.
- [ ] Can delete local issue branch and worktree after completion.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/gitops -count=1`
- [ ] Manual check: temp git repo integration tests create, merge, revert, and clean without touching the real workspace.

**Dependencies:** Task 1

**Files likely touched:**

- `issue-agent-runner/internal/gitops/manager.go`
- `issue-agent-runner/internal/gitops/manager_test.go`
- `issue-agent-runner/go.mod`

**Estimated scope:** Medium

### Task 3: Update worker prompt/result contract for issue worktrees and commits

**Description:** Change the worker contract so workers no longer edit the shared workspace directly. They must use the issue worktree paths, commit on issue branches, and write repo-level git metadata into `result.json` so the runner can integrate and audit changes safely.

**Acceptance criteria:**

- [ ] Prompt renders issue worktree locations and baseline branches.
- [ ] Worker result format includes touched repos, issue branch names, head SHAs, commit SHAs, verification commands, and changed file details.
- [ ] Existing executor still starts Codex the same way, but task payload now includes worktree metadata.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/prompt ./internal/executor/codex -count=1`
- [ ] Manual check: rendered prompt no longer says “不要开分支，不要创建 worktree，直接在当前 workspace 处理。”

**Dependencies:** Task 1, Task 2

**Files likely touched:**

- `issue-agent-runner/templates/worker_prompt.md`
- `issue-agent-runner/internal/domain/types.go`
- `issue-agent-runner/internal/prompt/render.go`
- `issue-agent-runner/internal/prompt/render_test.go`
- `issue-agent-runner/internal/executor/codex/executor.go`
- `issue-agent-runner/internal/executor/codex/executor_test.go`

**Estimated scope:** Medium

### Checkpoint: Foundation

- [ ] `cd issue-agent-runner && rtk go test ./...`
- [ ] Config can describe both `ml-svc-zcode` and `ml-web-zcode`.
- [ ] Prompt/result contract reflects issue worktree-based execution.

### Phase 2: Core Workflow

### Task 4: Provision issue worktrees during claim/start and record worker outputs

**Description:** When a candidate issue is claimed, provision or refresh per-repo issue worktrees before starting the worker. After worker success, parse the structured git result into state and transition the issue into `READY_TO_INTEGRATE` instead of immediately tagging it done.

**Acceptance criteria:**

- [ ] Claiming an issue creates issue worktree metadata for all configured repos.
- [ ] Worker success stores repo-level branch and commit data in state.
- [ ] Success no longer auto-adds `已处理`; instead it prepares the issue for baseline integration.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/runner -count=1`
- [ ] Manual check: dry-run and fake-runner tests show state moving to `READY_TO_INTEGRATE`.

**Dependencies:** Task 1, Task 2, Task 3

**Files likely touched:**

- `issue-agent-runner/internal/runner/runner.go`
- `issue-agent-runner/internal/runner/policy.go`
- `issue-agent-runner/internal/runner/policy_test.go`
- `issue-agent-runner/internal/runner/runner_test.go`

**Estimated scope:** Medium

### Task 5: Add serial baseline integration queue and conflict reporting

**Description:** Teach the runner to integrate exactly one ready issue into the local baseline branches at a time. If the merge is clean, mark the issue `AWAITING_VERIFY` and comment on CNB that it is ready for manual verification on the shared baseline environment. If there is a merge conflict, pause the issue and add a detailed CNB comment with conflict files.

**Acceptance criteria:**

- [ ] At most one issue can hold `AWAITING_VERIFY` state.
- [ ] Merge success records baseline merge commit SHAs and timestamps.
- [ ] Merge conflict records conflict files and comments directly on CNB issue.
- [ ] Conflict issues receive `需人工确认` and leave the normal integration queue.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/runner ./internal/integration ./internal/provider/cnb -count=1`
- [ ] Manual check: fake provider captures merge-success and merge-conflict comments.

**Dependencies:** Task 4

**Files likely touched:**

- `issue-agent-runner/internal/runner/runner.go`
- `issue-agent-runner/internal/runner/policy.go`
- `issue-agent-runner/internal/runner/runner_test.go`
- `issue-agent-runner/internal/integration/comments.go`
- `issue-agent-runner/internal/integration/comments_test.go`
- `issue-agent-runner/internal/provider/cnb/client.go`
- `issue-agent-runner/internal/provider/cnb/client_test.go`

**Estimated scope:** Medium

### Task 6: Add reject flow for failed human verification

**Description:** Add an explicit operator command that reverts the single pending verification merge commit from each touched baseline branch, comments on the CNB issue, updates state to `VERIFY_REJECTED`, and requeues the issue for another worker pass.

**Acceptance criteria:**

- [ ] New CLI command exists: `issue-agent-runner reject ISSUE`.
- [ ] Reject reverts local baseline merge commits and clears `AWAITING_VERIFY`.
- [ ] Reject comments are written to CNB issue.
- [ ] Rejected issue returns to retryable queue state without losing previous branch metadata.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/runner ./cmd/issue-agent-runner -count=1`
- [ ] Manual check: fake git manager and fake provider show revert + comment + requeue behavior.

**Dependencies:** Task 5

**Files likely touched:**

- `issue-agent-runner/cmd/issue-agent-runner/main.go`
- `issue-agent-runner/internal/runner/runner.go`
- `issue-agent-runner/internal/runner/runner_test.go`
- `issue-agent-runner/internal/integration/comments.go`

**Estimated scope:** Small

### Checkpoint: Core Workflow

- [ ] `cd issue-agent-runner && rtk go test ./...`
- [ ] One ready issue can be merged into local baseline branches and await human verification.
- [ ] Merge conflicts are visible in CNB issue comments and labels.
- [ ] Reject restores the baseline branches and unblocks the verification slot.

### Phase 3: Close, Push, Cleanup

### Task 7: Detect CNB closure for pending verification issues and push baseline branches

**Description:** Extend the polling loop so it explicitly refreshes `AWAITING_VERIFY` issues via `provider.Get`. When an issue is found closed, push only the touched baseline branches, add a final CNB comment with fix summary and pushed branches/SHAs, and mark the issue `CLOSED_PUSHED`.

**Acceptance criteria:**

- [ ] Closed pending-verification issues are detected without requiring `ListOpen`.
- [ ] Only touched baseline branches are pushed.
- [ ] Final CNB comment includes branch names, merge SHAs, worker summary, changed files, and verification commands.
- [ ] Push failures pause cleanup and leave enough state to retry push safely.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/runner ./internal/provider/cnb -count=1`
- [ ] Manual check: fake provider returns closed issue and fake git manager captures push calls.

**Dependencies:** Task 5

**Files likely touched:**

- `issue-agent-runner/internal/runner/runner.go`
- `issue-agent-runner/internal/runner/runner_test.go`
- `issue-agent-runner/internal/integration/comments.go`
- `issue-agent-runner/internal/provider/cnb/client.go`

**Estimated scope:** Medium

### Task 8: Cleanup local issue branches/worktrees after successful push

**Description:** After push succeeds for a closed issue, delete all local issue worktrees and issue branches, keep audit fields in state, and write updated progress summaries so temporary branches do not accumulate.

**Acceptance criteria:**

- [ ] Cleanup deletes local worktrees and issue branches for touched repos.
- [ ] Cleanup keeps minimal audit fields: issue number, touched repos, pushed SHAs, summary, verification, cleanup timestamp.
- [ ] Cleanup does not run if push failed.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/gitops ./internal/runner ./internal/state -count=1`
- [ ] Manual check: cleaned issue no longer appears as active/pending and temporary worktree paths are gone.

**Dependencies:** Task 7

**Files likely touched:**

- `issue-agent-runner/internal/gitops/manager.go`
- `issue-agent-runner/internal/gitops/manager_test.go`
- `issue-agent-runner/internal/runner/runner.go`
- `issue-agent-runner/internal/state/store.go`
- `issue-agent-runner/internal/state/store_test.go`

**Estimated scope:** Small

### Task 9: Update progress summary, README, and operator runbook

**Description:** Reflect the new lifecycle in summaries and documentation so operators can see which issue is running, ready to integrate, awaiting verification, conflicted, rejected, closed-pending-push, or cleaned.

**Acceptance criteria:**

- [ ] Summary output distinguishes active worker, integration queue, awaiting verify, merge conflict, and cleanup states.
- [ ] README documents the new operator flow:
  - worker runs in issue worktree
  - runner merges one issue at a time into local baseline
  - human validates baseline environment
  - close issue to push
  - run `reject ISSUE` to revert failed verification
- [ ] Example config matches the final config model.

**Verification:**

- [ ] Tests pass: `cd issue-agent-runner && rtk go test ./internal/report ./internal/runner -count=1`
- [ ] Manual check: `cd issue-agent-runner && rtk go run ./cmd/issue-agent-runner summary -c configs/runner.example.yaml`

**Dependencies:** Task 8

**Files likely touched:**

- `issue-agent-runner/internal/report/report.go`
- `issue-agent-runner/internal/report/report_test.go`
- `issue-agent-runner/README.md`
- `issue-agent-runner/configs/runner.example.yaml`

**Estimated scope:** Small

### Checkpoint: Complete

- [ ] `cd issue-agent-runner && rtk go test ./...`
- [ ] Issue workers run in isolated worktrees and commit locally.
- [ ] At most one issue is merged into local baseline and waiting for human verification.
- [ ] Merge conflicts are written into the CNB issue.
- [ ] `reject ISSUE` reverts the pending verification merge.
- [ ] Closing the CNB issue pushes touched baseline branches and triggers cleanup.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Worker edits shared workspace instead of issue worktree | High | Prompt must render issue worktree paths only, and runner should fail result ingestion if expected issue branch metadata is missing. |
| Multiple issues accumulate on baseline before human verifies | High | Enforce exactly one `AWAITING_VERIFY` issue at a time in runner policy. |
| Merge conflict visibility is poor | High | Emit conflict files and branch names directly into CNB issue comments and add `需人工确认`. |
| Push fails after issue is closed | Medium | Persist pushed/not-pushed branch status in state and retry on subsequent poll loops without deleting worktrees first. |
| State migration breaks old Python-compatible data | Medium | Keep additive fields only, preserve unknown JSON fields, and test legacy fixture loading. |

## Open Questions Resolved By This Plan

- Verification environment stays on the normal local baseline branches, not per-issue branches.
- Users do not need to know branch names during validation.
- Conflict updates are written directly into CNB issue comments and labels.
- Temporary issue branches are deleted only after close + push succeeds.

## Recommended Execution Order

1. Task 1
2. Task 2
3. Task 3
4. Task 4
5. Task 5
6. Task 6
7. Task 7
8. Task 8
9. Task 9
