#!/usr/bin/env bash
set -euo pipefail

LABEL="com.zcode.cnb-issue-runner"
WORKSPACE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNNER="${WORKSPACE}/automation/cnb_issue_runner.py"
STATE_DIR="${WORKSPACE}/.local_cache/cnb-issue-runner"
PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
PYTHON_BIN="${PYTHON_BIN:-$(command -v python3)}"
CODEX_BIN="${CODEX_BIN:-$(command -v codex)}"
MAX_CONCURRENCY="${MAX_CONCURRENCY:-5}"
INTERVAL_SECONDS="${INTERVAL_SECONDS:-60}"
GUI_TARGET="gui/$(id -u)"

if [[ "${1:-}" == "uninstall" ]]; then
  launchctl bootout "${GUI_TARGET}" "${PLIST}" 2>/dev/null || true
  rm -f "${PLIST}"
  echo "uninstalled ${LABEL}"
  exit 0
fi

mkdir -p "${STATE_DIR}" "${HOME}/Library/LaunchAgents"
chmod +x "${RUNNER}"

cat > "${PLIST}" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${PYTHON_BIN}</string>
    <string>${RUNNER}</string>
    <string>--once</string>
    <string>--workspace</string>
    <string>${WORKSPACE}</string>
    <string>--max-concurrency</string>
    <string>${MAX_CONCURRENCY}</string>
    <string>--codex-bin</string>
    <string>${CODEX_BIN}</string>
  </array>
  <key>StartInterval</key>
  <integer>${INTERVAL_SECONDS}</integer>
  <key>WorkingDirectory</key>
  <string>${WORKSPACE}</string>
  <key>StandardOutPath</key>
  <string>${STATE_DIR}/launchd.out.log</string>
  <key>StandardErrorPath</key>
  <string>${STATE_DIR}/launchd.err.log</string>
  <key>RunAtLoad</key>
  <true/>
</dict>
</plist>
EOF

launchctl bootout "${GUI_TARGET}" "${PLIST}" 2>/dev/null || true
launchctl bootstrap "${GUI_TARGET}" "${PLIST}"
launchctl kickstart -k "${GUI_TARGET}/${LABEL}" 2>/dev/null || true

echo "installed ${LABEL}"
echo "plist: ${PLIST}"
echo "logs: ${STATE_DIR}"
