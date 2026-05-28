#!/usr/bin/env bash
set -euo pipefail

WORKSPACE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNNER="${WORKSPACE}/automation/cnb_issue_runner.py"
STATE_DIR="${WORKSPACE}/.local_cache/cnb-issue-runner"
PID_FILE="${STATE_DIR}/daemon.pid"
LOG_FILE="${STATE_DIR}/daemon.log"
TMUX_SESSION="${TMUX_SESSION:-zcode-cnb-issue-runner}"
PYTHON_BIN="${PYTHON_BIN:-$(command -v python3)}"
CODEX_BIN="${CODEX_BIN:-$(command -v codex)}"
CODEX_MODEL="${CODEX_MODEL:-gpt-5.5}"
CODEX_REASONING_EFFORT="${CODEX_REASONING_EFFORT:-xhigh}"
MAX_CONCURRENCY="${MAX_CONCURRENCY:-10}"
INTERVAL_SECONDS="${INTERVAL_SECONDS:-60}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-3}"
RETRY_BACKOFF_SECONDS="${RETRY_BACKOFF_SECONDS:-600}"
CAPACITY_BACKOFF_SECONDS="${CAPACITY_BACKOFF_SECONDS:-1800}"
STALE_WORKER_SECONDS="${STALE_WORKER_SECONDS:-7200}"
MALLOC_DEBUG_ENV_KEYS=(
  MallocStackLogging
  MallocStackLoggingNoCompact
  MallocScribble
  MallocGuardEdges
  MallocNanoZone
  MallocCheckHeapStart
  MallocCheckHeapEach
)

unset_malloc_debug_env() {
  local key
  for key in "${MALLOC_DEBUG_ENV_KEYS[@]}"; do
    unset "${key}" || true
  done
}

unset_malloc_debug_env

mkdir -p "${STATE_DIR}"
chmod +x "${RUNNER}"

is_running() {
  if command -v tmux >/dev/null 2>&1 && tmux has-session -t "${TMUX_SESSION}" 2>/dev/null; then
    return 0
  fi
  [[ -f "${PID_FILE}" ]] && kill -0 "$(cat "${PID_FILE}")" 2>/dev/null
}

runner_command() {
  printf 'unset %s; cd %q && exec %q -u %q --daemon --workspace %q --interval %q --max-concurrency %q --max-attempts %q --retry-backoff-seconds %q --capacity-backoff-seconds %q --stale-worker-seconds %q --codex-bin %q --codex-model %q --codex-reasoning-effort %q >> %q 2>&1' \
    "${MALLOC_DEBUG_ENV_KEYS[*]}" \
    "${WORKSPACE}" \
    "${PYTHON_BIN}" \
    "${RUNNER}" \
    "${WORKSPACE}" \
    "${INTERVAL_SECONDS}" \
    "${MAX_CONCURRENCY}" \
    "${MAX_ATTEMPTS}" \
    "${RETRY_BACKOFF_SECONDS}" \
    "${CAPACITY_BACKOFF_SECONDS}" \
    "${STALE_WORKER_SECONDS}" \
    "${CODEX_BIN}" \
    "${CODEX_MODEL}" \
    "${CODEX_REASONING_EFFORT}" \
    "${LOG_FILE}"
}

case "${1:-start}" in
  start)
    if is_running; then
      echo "already running"
      exit 0
    fi
    if command -v tmux >/dev/null 2>&1; then
      tmux new-session -d -s "${TMUX_SESSION}" "$(runner_command)"
      rm -f "${PID_FILE}"
      echo "started tmux session=${TMUX_SESSION}"
    else
      nohup "${PYTHON_BIN}" "${RUNNER}" \
        --daemon \
        --workspace "${WORKSPACE}" \
        --interval "${INTERVAL_SECONDS}" \
        --max-concurrency "${MAX_CONCURRENCY}" \
        --max-attempts "${MAX_ATTEMPTS}" \
        --retry-backoff-seconds "${RETRY_BACKOFF_SECONDS}" \
        --capacity-backoff-seconds "${CAPACITY_BACKOFF_SECONDS}" \
        --stale-worker-seconds "${STALE_WORKER_SECONDS}" \
        --codex-bin "${CODEX_BIN}" \
        --codex-model "${CODEX_MODEL}" \
        --codex-reasoning-effort "${CODEX_REASONING_EFFORT}" \
        >> "${LOG_FILE}" 2>&1 &
      echo "$!" > "${PID_FILE}"
      echo "started pid=$(cat "${PID_FILE}")"
    fi
    echo "log: ${LOG_FILE}"
    ;;
  stop)
    if command -v tmux >/dev/null 2>&1 && tmux has-session -t "${TMUX_SESSION}" 2>/dev/null; then
      tmux kill-session -t "${TMUX_SESSION}"
      echo "stopped tmux session=${TMUX_SESSION}"
    elif is_running; then
      kill "$(cat "${PID_FILE}")"
      echo "stopped pid=$(cat "${PID_FILE}")"
    else
      echo "not running"
    fi
    rm -f "${PID_FILE}"
    ;;
  status)
    if command -v tmux >/dev/null 2>&1 && tmux has-session -t "${TMUX_SESSION}" 2>/dev/null; then
      echo "running tmux session=${TMUX_SESSION}"
    elif is_running; then
      echo "running pid=$(cat "${PID_FILE}")"
    else
      echo "not running"
    fi
    ;;
  *)
    echo "usage: $0 [start|stop|status]" >&2
    exit 2
    ;;
esac
