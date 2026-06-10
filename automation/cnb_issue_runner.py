#!/usr/bin/env python3
"""Poll CNB issues and dispatch Codex workers with a concurrency cap."""

from __future__ import annotations

import argparse
import contextlib
from dataclasses import dataclass
import fcntl
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
from typing import Any, Callable
import urllib.error
import urllib.parse
import urllib.request


DEFAULT_INTERVAL_SECONDS = 60
DEFAULT_MAX_CONCURRENCY = 2
RUNNING_LABEL = "处理中"
RUNNING_LABEL_ALIASES = {RUNNING_LABEL, "ai:in-progress"}
FAILED_LABEL = "处理失败"
NEEDS_HUMAN_LABEL = "需人工确认"
DONE_LABEL = "已处理"
LEGACY_WORKFLOW_LABELS = {"待集成", "待验证"}
LEGACY_COMPLETED_WORKFLOW_STATUSES = {"READY_TO_INTEGRATE", "AWAITING_VERIFY", "RUNNING"}
DEFAULT_SKIP_LABELS = RUNNING_LABEL_ALIASES | {DONE_LABEL, FAILED_LABEL, NEEDS_HUMAN_LABEL}
DEFAULT_MAX_ATTEMPTS = 3
DEFAULT_RETRY_BACKOFF_SECONDS = 600
DEFAULT_CAPACITY_BACKOFF_SECONDS = 1800
DEFAULT_PUSH_RETRY_BACKOFF_SECONDS = 600
DEFAULT_STALE_WORKER_SECONDS = 7200
DEFAULT_CODEX_MODEL = "gpt-5.5"
DEFAULT_CODEX_REASONING_EFFORT = "xhigh"
MALLOC_DEBUG_ENV_KEYS = {
    "MallocStackLogging",
    "MallocStackLoggingNoCompact",
    "MallocScribble",
    "MallocGuardEdges",
    "MallocNanoZone",
    "MallocCheckHeapStart",
    "MallocCheckHeapEach",
}
REQUIRED_PUSH_BRANCHES = {
    "ml-svc-zcode": "ai-v2.0",
    "ml-web-zcode": "poc/api-adapter-validation",
}


@dataclass(frozen=True)
class WorkerResult:
    exit_code: int | None
    error: str = ""


class RunnerError(RuntimeError):
    pass


@dataclass(frozen=True)
class PushedRepo:
    name: str
    branch: str
    head_sha: str
    commits: list[str]


def sanitized_subprocess_env(env: dict[str, str] | None = None) -> dict[str, str]:
    cleaned = dict(os.environ if env is None else env)
    for key in MALLOC_DEBUG_ENV_KEYS:
        cleaned.pop(key, None)
    return cleaned


def parse_cnb_remote(remote_url: str) -> tuple[str, str]:
    prefix = "https://cnb:"
    host_marker = "@cnb.cool/"
    if not remote_url.startswith(prefix) or host_marker not in remote_url:
        raise RunnerError("remote.origin.url must be https://cnb:<token>@cnb.cool/<owner>/<repo>.git")

    token_part, repo_part = remote_url[len(prefix) :].split(host_marker, 1)
    repo_path = repo_part.removesuffix(".git").strip("/")
    if not token_part or not repo_path:
        raise RunnerError("CNB token or repository path is empty")
    return token_part, repo_path


def issue_label_names(issue: dict[str, Any]) -> set[str]:
    return {str(label.get("name", "")) for label in issue.get("labels", []) if isinstance(label, dict)}


def labels_for_inactive_running_issue(issue: dict[str, Any], meta: dict[str, Any]) -> list[str] | None:
    labels = issue_label_names(issue)
    should_clean_legacy = bool(meta.get("pushCompletedAt")) or DONE_LABEL in labels
    has_running_label = not labels.isdisjoint(RUNNING_LABEL_ALIASES)
    has_legacy_label = not labels.isdisjoint(LEGACY_WORKFLOW_LABELS)
    if not has_running_label and not (should_clean_legacy and has_legacy_label):
        return None
    if has_running_label and not meta.get("paused") and not meta.get("lastFinishedAt") and not should_clean_legacy:
        return None

    labels.difference_update(RUNNING_LABEL_ALIASES)
    if should_clean_legacy:
        labels.difference_update(LEGACY_WORKFLOW_LABELS)
    if meta.get("paused"):
        error = str(meta.get("lastError", ""))
        labels.add(NEEDS_HUMAN_LABEL if is_resource_backoff_error(error) else FAILED_LABEL)
    return sorted(labels)


def cleanup_legacy_workflow_status(meta: dict[str, Any]) -> bool:
    if not meta.get("pushCompletedAt"):
        return False
    if meta.get("workflowStatus") not in LEGACY_COMPLETED_WORKFLOW_STATUSES:
        return False
    meta.pop("workflowStatus", None)
    return True


def cleanup_legacy_workflow_statuses(state: dict[str, Any]) -> list[str]:
    cleaned: list[str] = []
    issue_meta = state.get("issues", {})
    if not isinstance(issue_meta, dict):
        return cleaned
    for number, meta in issue_meta.items():
        if not isinstance(meta, dict):
            continue
        if cleanup_legacy_workflow_status(meta):
            cleaned.append(str(number))
    return cleaned


def filter_processable_issues(issues: list[dict[str, Any]], skip_labels: set[str]) -> list[dict[str, Any]]:
    return [issue for issue in issues if issue_label_names(issue).isdisjoint(skip_labels)]


def process_alive(pid: int) -> bool:
    if pid <= 0:
        return False
    try:
        stat = subprocess.run(
            ["ps", "-p", str(pid), "-o", "stat="],
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            check=False,
            env=sanitized_subprocess_env(),
        ).stdout.strip()
        if stat.startswith("Z"):
            return False
    except Exception:
        pass
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def read_text_tail(path: Path, max_bytes: int = 65536) -> str:
    if not path.exists():
        return ""
    with path.open("rb") as handle:
        handle.seek(0, os.SEEK_END)
        size = handle.tell()
        handle.seek(max(0, size - max_bytes))
        return handle.read().decode("utf-8", errors="replace")


def read_worker_result(worker: dict[str, Any]) -> WorkerResult:
    log_value = worker.get("log") if isinstance(worker, dict) else None
    if not isinstance(log_value, str) or not log_value:
        return WorkerResult(exit_code=None, error="worker log path missing")

    text = read_text_tail(Path(log_value))
    match = re.search(r"=== worker exit code (\d+) ", text)
    exit_code = int(match.group(1)) if match else None
    error_lines = [
        line.strip()
        for line in text.splitlines()
        if "ERROR:" in line or "Traceback" in line or "Exception" in line
    ]
    error = error_lines[-1] if error_lines else ""
    return WorkerResult(exit_code=exit_code, error=error)


def is_capacity_error(message: str) -> bool:
    lowered = message.lower()
    return "model is at capacity" in lowered or "selected model is at capacity" in lowered


def is_resource_backoff_error(message: str) -> bool:
    lowered = message.lower()
    return (
        is_capacity_error(message)
        or "too many requests" in lowered
        or "429" in lowered
        or "rate limit" in lowered
        or "quota" in lowered
        or "额度不足" in message
        or "订阅" in message
        or "no active subscription" in lowered
        or "no forced gpt backup channel" in lowered
        or "403 forbidden" in lowered
        or "502 bad gateway" in lowered
        or "503 service unavailable" in lowered
        or "service temporarily unavailable" in lowered
        or "upstream request failed" in lowered
        or "请求转发失败" in message
        or "上游请求失败" in message
    )


def log_timestamp(now: int | None = None) -> str:
    value = int(time.time()) if now is None else now
    return time.strftime("%Y-%m-%d %H:%M:%S %Z", time.localtime(value))


def format_log_line(message: str, now: int | None = None) -> str:
    return f"[{log_timestamp(now)}] {message}"


def log_line(message: str, *, file: Any = sys.stdout) -> None:
    print(format_log_line(message), file=file, flush=True)


def record_worker_completion(
    state: dict[str, Any],
    issue_number_value: str,
    worker: dict[str, Any],
    result: WorkerResult,
    now: int,
    max_attempts: int,
    retry_backoff_seconds: int,
    capacity_backoff_seconds: int,
) -> None:
    issues_state = state.setdefault("issues", {})
    meta = issues_state.setdefault(issue_number_value, {})
    if result.exit_code == 0:
        meta.update(
            {
                "lastExitCode": 0,
                "lastFinishedAt": now,
                "lastError": "",
                "nextRetryAt": now + retry_backoff_seconds,
                "title": worker.get("title", ""),
            }
        )
        return

    attempts = int(meta.get("attempts", 0)) + 1
    error = result.error or f"worker exited without success code: {result.exit_code}"
    backoff = capacity_backoff_seconds if is_resource_backoff_error(error) else retry_backoff_seconds
    meta.update(
        {
            "attempts": attempts,
            "lastExitCode": result.exit_code,
            "lastError": error,
            "lastFinishedAt": now,
            "nextRetryAt": now + backoff,
            "title": worker.get("title", ""),
        }
    )
    if attempts >= max_attempts:
        meta["paused"] = True
        meta["pauseReason"] = f"failed {attempts} attempts"


def worker_log_is_stale(worker: dict[str, Any], now: int, stale_worker_seconds: int) -> bool:
    if stale_worker_seconds <= 0:
        return False
    log_value = worker.get("log") if isinstance(worker, dict) else None
    if not isinstance(log_value, str) or not log_value:
        started_at = int(worker.get("startedAt", now)) if isinstance(worker, dict) else now
        return now - started_at >= stale_worker_seconds
    path = Path(log_value)
    if not path.exists():
        return False
    return now - int(path.stat().st_mtime) >= stale_worker_seconds


def prune_active_workers(
    state: dict[str, Any],
    pid_exists: Callable[[int], bool] = process_alive,
    now: int | None = None,
    read_worker_result: Callable[[dict[str, Any]], WorkerResult] = read_worker_result,
    max_attempts: int = DEFAULT_MAX_ATTEMPTS,
    retry_backoff_seconds: int = DEFAULT_RETRY_BACKOFF_SECONDS,
    capacity_backoff_seconds: int = DEFAULT_CAPACITY_BACKOFF_SECONDS,
    stale_worker_seconds: int = DEFAULT_STALE_WORKER_SECONDS,
    terminate_worker_func: Callable[[int], None] | None = None,
) -> dict[str, Any]:
    now = int(time.time()) if now is None else now
    workers = state.setdefault("workers", {})
    active = {}
    for issue_number_value, worker in list(workers.items()):
        if not isinstance(worker, dict) or not isinstance(worker.get("pid"), int):
            continue
        pid = worker["pid"]
        alive = pid_exists(pid)
        stale = alive and worker_log_is_stale(worker, now, stale_worker_seconds)
        if alive and not stale:
            active[issue_number_value] = worker
            continue
        if stale:
            (terminate_worker_func or terminate_worker)(pid)
            result = WorkerResult(exit_code=None, error="worker log stale; terminated for retry")
        else:
            result = read_worker_result(worker)
        record_worker_completion(
            state,
            issue_number_value,
            worker,
            result,
            now,
            max_attempts=max_attempts,
            retry_backoff_seconds=retry_backoff_seconds,
            capacity_backoff_seconds=capacity_backoff_seconds,
        )
    state["workers"] = active
    return active


def available_capacity(
    state: dict[str, Any],
    max_concurrency: int,
    pid_exists: Callable[[int], bool] = process_alive,
    **prune_kwargs: Any,
) -> int:
    active = prune_active_workers(state, pid_exists, **prune_kwargs)
    return max(0, max_concurrency - len(active))


def worker_slot_counts(state: dict[str, Any], max_concurrency: int) -> tuple[int, int]:
    workers = state.get("workers", {})
    processing = len(workers) if isinstance(workers, dict) else 0
    idle = max(0, max_concurrency - processing)
    return processing, idle


def format_poll_complete_message(
    open_count: int,
    candidate_count: int,
    state: dict[str, Any],
    max_concurrency: int,
) -> str:
    processing, idle = worker_slot_counts(state, max_concurrency)
    return (
        f"poll complete: open={open_count} candidates={candidate_count} "
        f"capacity={idle} processing={processing} idle={idle}"
    )


def terminate_worker(pid: int) -> None:
    if pid <= 0:
        return
    with contextlib.suppress(ProcessLookupError, PermissionError):
        os.killpg(pid, signal.SIGTERM)
    with contextlib.suppress(ProcessLookupError, PermissionError):
        os.kill(pid, signal.SIGTERM)


def remove_terminal_issue_workers(
    state: dict[str, Any],
    issues: list[dict[str, Any]],
    terminal_labels: set[str],
    terminate_worker: Callable[[int], None] = terminate_worker,
) -> None:
    labels_by_number = {issue_number(issue): issue_label_names(issue) for issue in issues}
    workers = state.setdefault("workers", {})
    for number, worker in list(workers.items()):
        if labels_by_number.get(number, set()).isdisjoint(terminal_labels):
            continue
        pid = worker.get("pid") if isinstance(worker, dict) else None
        if isinstance(pid, int):
            terminate_worker(pid)
        workers.pop(number, None)


def sync_inactive_running_issue_labels(
    token: str,
    repo_path: str,
    issues: list[dict[str, Any]],
    state: dict[str, Any],
) -> None:
    workers = state.get("workers", {})
    issue_meta = state.get("issues", {})
    for issue in issues:
        number = issue_number(issue)
        if number in workers:
            continue
        meta = issue_meta.get(number, {}) if isinstance(issue_meta, dict) else {}
        if not isinstance(meta, dict):
            continue
        if cleanup_legacy_workflow_status(meta):
            log_line(f"cleaned legacy workflow status for issue #{number}")
        labels = labels_for_inactive_running_issue(issue, meta)
        if labels is None or set(labels) == issue_label_names(issue):
            continue
        try:
            current_labels = issue_label_names(issue)
            set_issue_labels(token, repo_path, number, labels)
            for removed_label in sorted(current_labels - set(labels)):
                with contextlib.suppress(Exception):
                    delete_issue_label(token, repo_path, number, removed_label)
            log_line(f"synced inactive issue #{number} labels={','.join(labels) or '<none>'}")
        except Exception as error:  # noqa: BLE001 - do not block other issues when label sync fails.
            meta["labelSyncError"] = str(error)
            log_line(f"label sync failed for issue #{number}: {error}", file=sys.stderr)


def default_workspace() -> Path:
    return Path(__file__).resolve().parents[1]


def state_dir(workspace: Path) -> Path:
    return workspace / ".local_cache" / "cnb-issue-runner"


def state_file(workspace: Path) -> Path:
    return state_dir(workspace) / "state.json"


def lock_file(workspace: Path) -> Path:
    return state_dir(workspace) / "runner.lock"


@contextlib.contextmanager
def exclusive_lock(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a+") as handle:
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def load_state(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {"workers": {}}
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def save_state(path: Path, state: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = path.with_suffix(".tmp")
    with tmp_path.open("w", encoding="utf-8") as handle:
        json.dump(state, handle, ensure_ascii=False, indent=2, sort_keys=True)
        handle.write("\n")
    tmp_path.replace(path)


def progress_summary_file(workspace: Path) -> Path:
    return state_dir(workspace) / "progress-summary.md"


def issue_title(issue: dict[str, Any]) -> str:
    return str(issue.get("title", "")).strip()


def is_issue_retry_ready(issue: dict[str, Any], state: dict[str, Any], now: int) -> bool:
    meta = state.get("issues", {}).get(issue_number(issue), {})
    if not isinstance(meta, dict) or meta.get("paused"):
        return False
    return int(meta.get("nextRetryAt", 0) or 0) <= now


def filter_retryable_candidates(issues: list[dict[str, Any]], state: dict[str, Any], now: int) -> list[dict[str, Any]]:
    return [issue for issue in issues if is_issue_retry_ready(issue, state, now)]


def issue_is_processable(
    issue: dict[str, Any],
    state: dict[str, Any],
    skip_labels: set[str],
    now: int,
    min_restart_seconds: int = DEFAULT_RETRY_BACKOFF_SECONDS,
) -> bool:
    labels = issue_label_names(issue)
    number = issue_number(issue)
    if number in state.get("workers", {}):
        return False
    meta = state.get("issues", {}).get(number, {})
    if isinstance(meta, dict):
        last_started_at = int(meta.get("lastStartedAt", 0) or 0)
        if min_restart_seconds > 0 and last_started_at > 0 and now - last_started_at < min_restart_seconds:
            return False
    if labels.isdisjoint(skip_labels):
        return is_issue_retry_ready(issue, state, now)
    if not labels.isdisjoint(RUNNING_LABEL_ALIASES) and isinstance(meta, dict) and int(meta.get("attempts", 0) or 0) > 0:
        return is_issue_retry_ready(issue, state, now)
    return False


def render_progress_summary(issues: list[dict[str, Any]], state: dict[str, Any], now: int) -> str:
    issue_by_number = {issue_number(issue): issue for issue in issues}
    workers = state.get("workers", {})
    issue_meta = state.get("issues", {})
    active_lines = []
    for number, worker in sorted(workers.items(), key=lambda item: item[0]):
        if not isinstance(worker, dict):
            continue
        title = worker.get("title") or issue_title(issue_by_number.get(number, {}))
        started_at = int(worker.get("startedAt", now) or now)
        active_lines.append(f"- #{number} {title} running_for={max(0, now - started_at)}s")

    paused_lines = []
    waiting_lines = []
    for number, meta in sorted(issue_meta.items(), key=lambda item: item[0]):
        if not isinstance(meta, dict):
            continue
        if number not in issue_by_number and number not in workers:
            continue
        issue = issue_by_number.get(number, {})
        if issue and issue_is_terminal(issue):
            continue
        title = meta.get("title") or issue_title(issue_by_number.get(number, {}))
        if meta.get("paused"):
            paused_lines.append(f"- #{number} attempts={meta.get('attempts', 0)} {title} error={meta.get('lastError', '')}")
            continue
        if number in workers:
            continue
        next_retry_at = int(meta.get("nextRetryAt", 0) or 0)
        if next_retry_at > now:
            waiting_lines.append(f"- #{number} retry_in={next_retry_at - now}s attempts={meta.get('attempts', 0)} {title}")

    lines = [
        f"# CNB Issue Runner Progress",
        "",
        f"Updated: {time.strftime('%Y-%m-%d %H:%M:%S %Z', time.localtime(now))}",
        f"Open issues: {len(issues)}",
        f"Active workers: {len(active_lines)}",
        f"Retry backoff issues: {len(waiting_lines)}",
        f"Paused issues: {len(paused_lines)}",
        "",
        "## Active",
        *(active_lines or ["- none"]),
        "",
        "## Waiting For Retry",
        *(waiting_lines or ["- none"]),
        "",
        "## Paused",
        *(paused_lines or ["- none"]),
        "",
    ]
    return "\n".join(lines)


def write_progress_summary(workspace: Path, issues: list[dict[str, Any]], state: dict[str, Any], now: int) -> None:
    path = progress_summary_file(workspace)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(render_progress_summary(issues, state, now), encoding="utf-8")


def run_git_remote(workspace: Path, service_repo: str) -> str:
    result = subprocess.run(
        ["git", "-C", str(workspace / service_repo), "config", "--get", "remote.origin.url"],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=sanitized_subprocess_env(),
    )
    return result.stdout.strip()


def cnb_request(token: str, method: str, url: str, body: dict[str, Any] | None = None) -> Any:
    data = None
    headers = {
        "accept": "application/json",
        "Authorization": f"Bearer {token}",
    }
    if body is not None:
        data = json.dumps(body, ensure_ascii=False).encode("utf-8")
        headers["content-type"] = "application/json"

    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            raw = response.read().decode("utf-8")
    except urllib.error.HTTPError as error:
        detail = error.read().decode("utf-8", errors="replace")
        raise RunnerError(f"CNB request failed: {error.code} {detail}") from error

    if not raw:
        return None
    return json.loads(raw)


def list_open_issues(token: str, repo_path: str) -> list[dict[str, Any]]:
    all_issues: list[dict[str, Any]] = []
    page_size = 50
    for page in range(1, 101):
        url = f"https://api.cnb.cool/{repo_path}/-/issues?page={page}&page_size={page_size}&state=open"
        issues = cnb_request(token, "GET", url)
        if not isinstance(issues, list) or not issues:
            break
        all_issues.extend(issue for issue in issues if isinstance(issue, dict))
        if len(issues) < page_size:
            break
    return all_issues


def get_issue(token: str, repo_path: str, issue_number_value: str) -> dict[str, Any]:
    url = f"https://api.cnb.cool/{repo_path}/-/issues/{issue_number_value}"
    issue = cnb_request(token, "GET", url)
    return issue if isinstance(issue, dict) else {}


def set_issue_labels(token: str, repo_path: str, issue_number: str, labels: list[str]) -> None:
    url = f"https://api.cnb.cool/{repo_path}/-/issues/{issue_number}/labels"
    cnb_request(token, "PUT", url, {"labels": labels})


def delete_issue_label(token: str, repo_path: str, issue_number: str, label: str) -> None:
    encoded_label = urllib.parse.quote(label, safe="")
    url = f"https://api.cnb.cool/{repo_path}/-/issues/{issue_number}/labels/{encoded_label}"
    cnb_request(token, "DELETE", url)


def add_issue_comment(token: str, repo_path: str, issue_number_value: str, body: str) -> None:
    url = f"https://api.cnb.cool/{repo_path}/-/issues/{issue_number_value}/comments"
    cnb_request(token, "POST", url, {"body": body})


def issue_number(issue: dict[str, Any]) -> str:
    value = issue.get("number") or issue.get("id")
    return str(value)


def issue_is_terminal(issue: dict[str, Any]) -> bool:
    return str(issue.get("state", "")).lower() == "closed" or DONE_LABEL in issue_label_names(issue)


def git_repo_name(repo_path: Path) -> str:
    return repo_path.name


def discover_git_repositories(workspace: Path) -> list[Path]:
    repos: list[Path] = []
    for child in sorted(workspace.iterdir(), key=lambda item: item.name):
        if child.name.startswith(".") or not child.is_dir():
            continue
        if (child / ".git").exists():
            repos.append(child)
    return repos


def run_git(repo_path: Path, args: list[str], check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", "-C", str(repo_path), *args],
        check=check,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=sanitized_subprocess_env(),
    )


def current_git_branch(repo_path: Path) -> str:
    result = run_git(repo_path, ["rev-parse", "--abbrev-ref", "HEAD"])
    branch = result.stdout.strip()
    if not branch or branch == "HEAD":
        raise RunnerError(f"{repo_path} is not on a branch")
    return branch


def current_git_head(repo_path: Path) -> str:
    return run_git(repo_path, ["rev-parse", "HEAD"]).stdout.strip()


def validate_push_branch(repo_path: Path, branch: str) -> None:
    repo_name = git_repo_name(repo_path)
    expected_branch = REQUIRED_PUSH_BRANCHES.get(repo_name)
    if expected_branch and branch != expected_branch:
        raise RunnerError(f"refusing to push {repo_name} from branch {branch}; expected {expected_branch}")


def unpushed_commits(repo_path: Path) -> list[str]:
    result = run_git(
        repo_path,
        [
            "log",
            "--cherry-pick",
            "--right-only",
            "--format=%H",
            "@{upstream}...HEAD",
        ],
        check=False,
    )
    if result.returncode not in {0, 1}:
        raise RunnerError(f"failed to inspect unpushed commits in {repo_path}: {result.stderr.strip()}")
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def validate_issue_push_scope(repo_path: Path, branch: str, issue_number_value: str, issue_commits: list[str]) -> None:
    issue_commit_set = set(issue_commits)
    unrelated_commits = [commit for commit in unpushed_commits(repo_path) if commit not in issue_commit_set]
    if unrelated_commits:
        repo_name = git_repo_name(repo_path)
        raise RunnerError(
            f"refusing to push {repo_name}:{branch} for CNB issue #{issue_number_value}; "
            f"found {len(unrelated_commits)} unrelated unpushed commits"
        )


def unpushed_issue_commits(repo_path: Path, issue_number_value: str) -> list[str]:
    pattern = f"CNB issue #{issue_number_value}"
    result = run_git(
        repo_path,
        [
            "log",
            "--cherry-pick",
            "--right-only",
            "--regexp-ignore-case",
            "--format=%H",
            "--grep",
            pattern,
            "@{upstream}...HEAD",
        ],
        check=False,
    )
    if result.returncode not in {0, 1}:
        raise RunnerError(f"failed to inspect unpushed commits in {repo_path}: {result.stderr.strip()}")
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def push_issue_commits(workspace: Path, issue_number_value: str) -> list[PushedRepo]:
    pushed: list[PushedRepo] = []
    for repo_path in discover_git_repositories(workspace):
        commits = unpushed_issue_commits(repo_path, issue_number_value)
        if not commits:
            continue
        branch = current_git_branch(repo_path)
        validate_push_branch(repo_path, branch)
        validate_issue_push_scope(repo_path, branch, issue_number_value, commits)
        run_git(repo_path, ["push", "origin", branch])
        pushed.append(
            PushedRepo(
                name=git_repo_name(repo_path),
                branch=branch,
                head_sha=current_git_head(repo_path),
                commits=commits,
            )
        )
    return pushed


def has_unpushed_issue_commits(workspace: Path, issue_number_value: str) -> bool:
    return any(unpushed_issue_commits(repo_path, issue_number_value) for repo_path in discover_git_repositories(workspace))


def render_pushed_comment(issue_number_value: str, pushed: list[PushedRepo]) -> str:
    lines = [
        f"Issue #{issue_number_value} 已进入终态，runner 已自动推送相关提交。",
        "",
        "推送结果:",
    ]
    for repo in pushed:
        lines.append(f"- `{repo.name}` -> `{repo.branch}` ({repo.head_sha})")
        for commit in repo.commits:
            lines.append(f"  - commit `{commit}`")
    return "\n".join(lines)


def pushed_repo_records(pushed: list[PushedRepo]) -> list[dict[str, Any]]:
    return [
        {
            "name": repo.name,
            "branch": repo.branch,
            "headSha": repo.head_sha,
            "commits": repo.commits,
        }
        for repo in pushed
    ]


def append_pushed_repo_records(meta: dict[str, Any], pushed: list[PushedRepo]) -> None:
    existing = meta.get("pushedRepos")
    if not isinstance(existing, list):
        existing = []
    existing.extend(pushed_repo_records(pushed))
    meta["pushedRepos"] = existing


def clear_push_state(meta: dict[str, Any]) -> None:
    for key in [
        "pushCompletedAt",
        "pushLastAttemptAt",
        "pushNoopAt",
        "nextPushRetryAt",
        "pushError",
        "pushedRepos",
    ]:
        meta.pop(key, None)


def issue_last_exit_code(meta: dict[str, Any]) -> int:
    value = meta.get("lastExitCode", -1)
    try:
        return int(value)
    except (TypeError, ValueError):
        return -1


def terminal_issue_candidates(
    issues: list[dict[str, Any]],
    state: dict[str, Any],
    token: str,
    repo_path: str,
    now: int,
    has_unpushed_commits: Callable[[str], bool] | None = None,
) -> dict[str, dict[str, Any]]:
    candidates: dict[str, dict[str, Any]] = {}
    for issue in issues:
        number = issue_number(issue)
        if number not in state.get("issues", {}) or number in state.get("workers", {}):
            continue
        meta = state.get("issues", {}).get(number, {})
        if isinstance(meta, dict) and int(meta.get("nextPushRetryAt", 0) or 0) > now:
            continue
        if isinstance(meta, dict) and meta.get("pushCompletedAt"):
            checker = has_unpushed_commits or (lambda _number: False)
            if not checker(number):
                continue
        if issue_is_terminal(issue):
            candidates[number] = issue

    for number, meta in list(state.get("issues", {}).items()):
        if number in state.get("workers", {}):
            continue
        if not isinstance(meta, dict):
            continue
        if int(meta.get("nextPushRetryAt", 0) or 0) > now:
            continue
        local_unpushed: bool | None = None

        def issue_has_unpushed_commits() -> bool:
            nonlocal local_unpushed
            if local_unpushed is None:
                local_unpushed = bool(has_unpushed_commits and has_unpushed_commits(number))
            return local_unpushed

        if meta.get("pushCompletedAt"):
            if not issue_has_unpushed_commits():
                continue
        if issue_last_exit_code(meta) != 0 and not meta.get("pushError") and not issue_has_unpushed_commits():
            continue
        if number in candidates:
            continue
        try:
            issue = get_issue(token, repo_path, number)
        except Exception as error:  # noqa: BLE001 - keep polling other issues when one lookup fails.
            log_line(f"terminal issue lookup failed for #{number}: {error}", file=sys.stderr)
            continue
        if issue and issue_is_terminal(issue):
            candidates[number] = issue
    return candidates


def push_terminal_issues(
    workspace: Path,
    token: str,
    repo_path: str,
    issues: list[dict[str, Any]],
    state: dict[str, Any],
    now: int,
) -> None:
    for number, issue in sorted(
        terminal_issue_candidates(
            issues,
            state,
            token,
            repo_path,
            now,
            has_unpushed_commits=lambda issue_number_value: has_unpushed_issue_commits(workspace, issue_number_value),
        ).items()
    ):
        meta = state.setdefault("issues", {}).setdefault(number, {})
        try:
            pushed = push_issue_commits(workspace, number)
        except Exception as error:  # noqa: BLE001 - persist failure so the next poll can retry.
            meta["pushError"] = str(error)
            meta["pushLastAttemptAt"] = now
            meta["nextPushRetryAt"] = now + DEFAULT_PUSH_RETRY_BACKOFF_SECONDS
            log_line(f"push failed for issue #{number}: {error}", file=sys.stderr)
            continue
        meta["pushLastAttemptAt"] = now
        meta.pop("nextPushRetryAt", None)
        meta.pop("pushError", None)
        if not pushed:
            if not meta.get("pushNoopAt"):
                log_line(f"push skipped for issue #{number}: no unpushed CNB issue commits found")
            meta["pushNoopAt"] = now
            continue
        meta["pushCompletedAt"] = now
        meta.pop("pushNoopAt", None)
        append_pushed_repo_records(meta, pushed)
        add_issue_comment(token, repo_path, number, render_pushed_comment(number, pushed))
        log_line(
            "pushed issue #"
            + number
            + " repos="
            + ",".join(f"{repo.name}:{repo.branch}@{repo.head_sha[:12]}" for repo in pushed)
        )


def build_worker_prompt(
    issue: dict[str, Any],
    repo_path: str,
    codex_model: str = DEFAULT_CODEX_MODEL,
    codex_reasoning_effort: str = DEFAULT_CODEX_REASONING_EFFORT,
) -> str:
    number = issue_number(issue)
    title = str(issue.get("title", "")).strip()
    body = str(issue.get("body", "")).strip()
    return f"""继续处理 CNB issue，不要开分支，不要创建 worktree，直接在当前 workspace 处理。

CNB repo: https://cnb.cool/{repo_path}
Issue: #{number}
Title: {title}
Codex model: {codex_model}
Codex reasoning effort: {codex_reasoning_effort}
Body:
{body}

执行要求：
- 只处理这个 issue，不要顺手处理其他 issue。
- 所有 CNB 评论、任务回复、阻塞说明和完成说明都必须使用中文；不要使用英文作为主体回复。
- 先重新拉取 issue 详情确认状态；如果已经有 `已处理` 标签，直接结束。
- 修改前检查相关仓库 dirty 状态；不要还原用户或其他任务已有改动。
- 前端源码必须修改 `/Users/m4/Documents/code/zcode/ml-web-zcode`；不要通过修改 `ml-svc-zcode/addons/ml.svc.zcode/public/zcode/web` 或 `public/zcode/web/assets` 下的编译后文件来修前端问题。
- 后端仓库 `ml-svc-zcode` 只用于 Go/API/数据库等后端修复；涉及 React、页面、组件、样式、运行态 UI、编辑器 UI 的问题应优先在 `ml-web-zcode` 源码中修改。
- shell 命令使用 `rtk` 前缀。
- 找到根因后实现最小修复，并运行相关测试/检查。
- 只有测试通过并且相关代码已经提交后，才把这个 issue 标记为 `已处理` 并评论说明改动和验证命令。
- 标记 `已处理` 前必须完成 git 提交：
  - 对每个有本任务改动的仓库执行 `git status --short`，只 stage 和本 issue 直接相关的文件。
  - 不要 stage `.local_cache`、runner 日志、提示词、其他 issue 或用户已有改动。
  - 如果存在无法区分归属的 dirty 改动、合并冲突或提交失败，必须在 CNB 评论说明阻塞原因，不要打 `已处理`。
  - commit 标题格式使用 `fix: resolve CNB issue #{number} <中文简短问题描述>`，其中问题描述必须使用中文。
  - commit body 必须使用中文描述，且包含 `修复内容:` 和 `验证:`，写清修了什么、验证命令和结果。
  - 提交完成后，在 CNB 评论里写明每个仓库的 commit SHA。
  - 不要手动 `git push`；runner 会在任务进入终态后自动 push 包含 `CNB issue #{number}` 的相关提交，并追加 push 结果评论。
- 处理完成后的 CNB 评论必须包含“修改文件清单”，逐项写明：
  - 文件路径。
  - 起始行号或关键行号。
  - 修改的方法名、函数名、组件名；如果是新增逻辑，写新增方法名/新增组件名。
  - 简要说明该文件改了什么。
- 处理完成后的 CNB 评论必须包含“处理模型：{codex_model} / {codex_reasoning_effort}”。
- 如果无法处理或验证失败，评论说明阻塞原因，不要打 `已处理`。
"""


def spawn_worker(args: argparse.Namespace, issue: dict[str, Any], token: str, repo_path: str, state: dict[str, Any]) -> None:
    number = issue_number(issue)
    prompt = build_worker_prompt(issue, repo_path, args.codex_model, args.codex_reasoning_effort)
    issue_path = state_dir(args.workspace) / f"issue-{number}.json"
    prompt_path = state_dir(args.workspace) / f"issue-{number}.prompt.md"
    log_path = state_dir(args.workspace) / f"issue-{number}.log"
    issue_path.write_text(json.dumps(issue, ensure_ascii=False, indent=2), encoding="utf-8")
    prompt_path.write_text(prompt, encoding="utf-8")

    if args.dry_run:
        log_line(f"[dry-run] would start worker for issue #{number}: {issue.get('title', '')}")
        return

    labels = sorted(issue_label_names(issue) | {RUNNING_LABEL})
    set_issue_labels(token, repo_path, number, labels)

    command = [
        sys.executable,
        str(Path(__file__).resolve()),
        "--worker",
        "--workspace",
        str(args.workspace),
        "--codex-bin",
        args.codex_bin,
        "--codex-model",
        args.codex_model,
        "--codex-reasoning-effort",
        args.codex_reasoning_effort,
        "--prompt-file",
        str(prompt_path),
        "--log-file",
        str(log_path),
    ]
    process = subprocess.Popen(
        command,
        cwd=str(args.workspace),
        start_new_session=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=sanitized_subprocess_env(),
    )
    state.setdefault("workers", {})[number] = {
        "pid": process.pid,
        "title": issue.get("title", ""),
        "startedAt": int(time.time()),
        "log": str(log_path),
        "codexModel": args.codex_model,
        "codexReasoningEffort": args.codex_reasoning_effort,
    }
    meta = state.setdefault("issues", {}).setdefault(number, {})
    clear_push_state(meta)
    started_at = int(time.time())
    meta["title"] = issue.get("title", "")
    meta["lastStartedAt"] = started_at
    meta["nextRetryAt"] = started_at + args.retry_backoff_seconds
    meta.pop("paused", None)
    meta.pop("pauseReason", None)
    log_line(f"started issue #{number} worker pid={process.pid}")


def build_codex_command(args: argparse.Namespace) -> list[str]:
    command = [
        args.codex_bin,
        "--ask-for-approval",
        "never",
        "exec",
        "--cd",
        str(args.workspace),
        "--sandbox",
        "danger-full-access",
    ]
    if args.codex_model:
        command.extend(["--model", args.codex_model])
    if args.codex_reasoning_effort:
        command.extend(["-c", f'model_reasoning_effort="{args.codex_reasoning_effort}"'])
    command.extend(
        [
            "--skip-git-repo-check",
            "-",
        ]
    )
    return command


def worker_main(args: argparse.Namespace) -> int:
    prompt = Path(args.prompt_file).read_text(encoding="utf-8")
    log_path = Path(args.log_file)
    log_path.parent.mkdir(parents=True, exist_ok=True)
    command = build_codex_command(args)
    with log_path.open("a", encoding="utf-8") as log:
        log.write(f"\n=== worker start {log_timestamp()} ===\n")
        log.write(f"codex model: {args.codex_model}\n")
        log.write(f"codex reasoning effort: {args.codex_reasoning_effort}\n")
        log.flush()
        process = subprocess.run(
            command,
            input=prompt,
            text=True,
            stdout=log,
            stderr=subprocess.STDOUT,
            env=sanitized_subprocess_env(),
        )
        log.write(f"\n=== worker exit code {process.returncode} {log_timestamp()} ===\n")
    return process.returncode


def poll_once(args: argparse.Namespace) -> None:
    remote_url = run_git_remote(args.workspace, args.service_repo)
    token, repo_path = parse_cnb_remote(remote_url)

    with exclusive_lock(lock_file(args.workspace)):
        now = int(time.time())
        path = state_file(args.workspace)
        state = load_state(path)
        for number in cleanup_legacy_workflow_statuses(state):
            log_line(f"cleaned legacy workflow status for issue #{number}")
        issues = list_open_issues(token, repo_path)
        remove_terminal_issue_workers(state, issues, terminal_labels={"已处理"})
        push_terminal_issues(args.workspace, token, repo_path, issues, state, now)
        capacity = available_capacity(
            state,
            args.max_concurrency,
            now=now,
            max_attempts=args.max_attempts,
            retry_backoff_seconds=args.retry_backoff_seconds,
            capacity_backoff_seconds=args.capacity_backoff_seconds,
            stale_worker_seconds=args.stale_worker_seconds,
        )
        sync_inactive_running_issue_labels(token, repo_path, issues, state)
        if capacity <= 0:
            write_progress_summary(args.workspace, issues, state, now)
            save_state(path, state)
            processing, idle = worker_slot_counts(state, args.max_concurrency)
            log_line(
                f"ai concurrency reached {args.max_concurrency}; "
                f"processing={processing} idle={idle}; wait for workers to finish"
            )
            return

        candidates = [
            issue
            for issue in issues
            if issue_is_processable(issue, state, set(args.skip_labels), now, args.retry_backoff_seconds)
        ]
        for issue in candidates[:capacity]:
            spawn_worker(args, issue, token, repo_path, state)

        write_progress_summary(args.workspace, issues, state, now)
        save_state(path, state)
        log_line(
            format_poll_complete_message(
                open_count=len(issues),
                candidate_count=len(candidates),
                state=state,
                max_concurrency=args.max_concurrency,
            )
        )


def daemon_loop(args: argparse.Namespace) -> None:
    while True:
        try:
            poll_once(args)
        except Exception as error:  # noqa: BLE001 - daemon should keep polling after transient failures.
            log_line(f"poll failed: {error}", file=sys.stderr)
        time.sleep(args.interval)


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workspace", type=Path, default=default_workspace())
    parser.add_argument("--service-repo", default="ml-svc-zcode")
    parser.add_argument("--interval", type=int, default=DEFAULT_INTERVAL_SECONDS)
    parser.add_argument("--max-concurrency", type=int, default=DEFAULT_MAX_CONCURRENCY)
    parser.add_argument("--max-attempts", type=int, default=DEFAULT_MAX_ATTEMPTS)
    parser.add_argument("--retry-backoff-seconds", type=int, default=DEFAULT_RETRY_BACKOFF_SECONDS)
    parser.add_argument("--capacity-backoff-seconds", type=int, default=DEFAULT_CAPACITY_BACKOFF_SECONDS)
    parser.add_argument("--stale-worker-seconds", type=int, default=DEFAULT_STALE_WORKER_SECONDS)
    parser.add_argument("--codex-bin", default="codex")
    parser.add_argument("--codex-model", default=DEFAULT_CODEX_MODEL)
    parser.add_argument("--codex-reasoning-effort", default=DEFAULT_CODEX_REASONING_EFFORT)
    parser.add_argument("--skip-label", dest="skip_labels", action="append", default=[])
    parser.add_argument("--once", action="store_true")
    parser.add_argument("--daemon", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--worker", action="store_true")
    parser.add_argument("--prompt-file")
    parser.add_argument("--log-file")
    args = parser.parse_args(argv)
    args.workspace = args.workspace.resolve()
    if not args.skip_labels:
        args.skip_labels = sorted(DEFAULT_SKIP_LABELS)
    if args.max_concurrency < 1:
        raise RunnerError("--max-concurrency must be at least 1")
    if args.max_attempts < 1:
        raise RunnerError("--max-attempts must be at least 1")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    if args.worker:
        if not args.prompt_file or not args.log_file:
            raise RunnerError("--worker requires --prompt-file and --log-file")
        return worker_main(args)
    if args.daemon:
        daemon_loop(args)
        return signal.SIGTERM
    poll_once(args)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
