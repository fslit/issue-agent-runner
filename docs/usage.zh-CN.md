# IssueAgent Runner 使用说明

## 当前运行方式

当前 zcode workspace 使用本地私有配置运行：

```bash
/Users/m4/Documents/code/zcode/issue-agent-runner/.local_cache/cnb-issue-runner/runner.local.yaml
```

`configs/runner.example.yaml` 是示例配置，不是当前 daemon 的生效配置。真实 CNB token 只应该放在本地私有配置里，不要写进示例配置。

当前 daemon 启动命令：

```bash
cd /Users/m4/Documents/code/zcode/issue-agent-runner
rtk go run ./cmd/issue-agent-runner run -c .local_cache/cnb-issue-runner/runner.local.yaml
```

当前常驻方式是 `tmux`：

```bash
tmux ls
tmux attach -t issue-agent-runner
```

## 启动和停止

启动：

```bash
cd /Users/m4/Documents/code/zcode/issue-agent-runner
tmux new-session -d -s issue-agent-runner \
  "rtk go run ./cmd/issue-agent-runner run -c .local_cache/cnb-issue-runner/runner.local.yaml >> .local_cache/cnb-issue-runner/go-daemon.log 2>&1"
```

停止：

```bash
tmux kill-session -t issue-agent-runner
```

如果曾经用 `go run` 启动后残留了孤儿进程，可以检查：

```bash
ps -ef | rg -i "issue-agent-runner run|codex exec"
```

同一个 `stateDir` 现在有 daemon 锁保护，重复启动会直接失败：

```text
runner daemon already running for state dir ...
```

## 状态查看

查看当前状态：

```bash
cd /Users/m4/Documents/code/zcode/issue-agent-runner
rtk go run ./cmd/issue-agent-runner status -c .local_cache/cnb-issue-runner/runner.local.yaml
```

查看摘要：

```bash
rtk go run ./cmd/issue-agent-runner summary -c .local_cache/cnb-issue-runner/runner.local.yaml
```

查看当前人工验证列表：

```bash
rtk go run ./cmd/issue-agent-runner current -c .local_cache/cnb-issue-runner/runner.local.yaml
```

也可以直接读生成文件：

```bash
/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/progress-summary.md
/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/progress-summary.json
```

如果当前有一个或多个 issue 已经合入本地基准分支并等待人工验证，还会生成：

```bash
/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/current-validation.md
/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/current-validation.json
```

`current-validation.*` 表示当前待人工验证的 issue 列表，不表示所有任务。它包含每个待验证 issue 的 issue 号、标题、基准分支、合并提交、worker 修复摘要和 worker 自测命令。执行 `reject` 或 issue 关闭后完成 push/cleanup，且没有剩余待验证 issue 时，该文件会自动删除。

## 日志查看

调度事件日志：

```bash
tail -f /Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/runner.log
```

`runner.log` 是 JSONL，每一行是一个结构化事件，并带有 `timestamp` 字段。常见事件：

```text
poll_start
poll_complete
worker_started
worker_succeeded
merged
merge_conflict
awaiting_verify
pushed
issue_cleaned
```

单个 issue 的 Codex worker 日志：

```bash
tail -f /Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/issue-77.log
```

Go daemon 启动和回调输出：

```bash
tail -f /Users/m4/Documents/code/zcode/issue-agent-runner/.local_cache/cnb-issue-runner/go-daemon.log
```

## 配置说明

当前关键配置在 `runner.local.yaml`：

```yaml
runner:
  intervalSeconds: 60
  maxConcurrency: 5
  hardWorkerSeconds: 14400

executor:
  type: codex
  bin: /Users/m4/.local/bin/codex
  model: gpt-5.5
  reasoningEffort: xhigh

callback:
  enabled: true
  listen: 127.0.0.1:8791
  path: /callbacks/cnb
  secret: change-me
  secretHeader: X-Issue-Agent-Token
```

并发控制：

- `runner.maxConcurrency: 5` 控制最多同时启动 5 个 Codex worker。
- `tmux -> rtk/go run -> issue-agent-runner` 这条父子进程链不等于多个任务。
- 真正执行任务时能看到 `codex exec` 进程和 `worker_started` 日志事件。
- `runner.hardWorkerSeconds: 14400` 是 worker 总运行时长硬上限，默认 4 小时；超过后 runner 会终止 worker 并按可重试失败处理。
- `runner.staleWorkerSeconds` 和 `hardWorkerSeconds` 不同：前者看 worker 日志是否长期没有更新，后者只看启动到现在的总耗时。

修改配置后需要重启 daemon：

```bash
tmux kill-session -t issue-agent-runner
cd /Users/m4/Documents/code/zcode/issue-agent-runner
tmux new-session -d -s issue-agent-runner \
  "rtk go run ./cmd/issue-agent-runner run -c .local_cache/cnb-issue-runner/runner.local.yaml >> .local_cache/cnb-issue-runner/go-daemon.log 2>&1"
```

## CNB Callback

callback 用来让 CNB webhook 或其他系统在 issue 状态变化时立即唤醒 runner。它只触发一轮轮询，不绕过状态机和并发限制。

本地验证：

```bash
curl -X POST \
  -H 'X-Issue-Agent-Token: change-me' \
  http://127.0.0.1:8791/callbacks/cnb
```

正常返回：

```json
{"status":"queued"}
```

不带 secret header 会返回 `401 Unauthorized`。

如果 CNB 不能访问本机 `127.0.0.1`，需要通过内网反代或 tunnel 暴露地址，并保留 `X-Issue-Agent-Token` header。

## 集成 Hook

Go runner 支持 command hook 和 HTTP webhook hook。两类 hook 都是旁路能力：失败只写日志，不回滚 runner 状态。

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

- `onIssueClaim`：runner 认领 issue 并成功启动 worker 后触发。
- `onIssuePaused`：issue 因人工 pause、quota/auth、达到最大失败次数或合并冲突暂停时触发。
- `onProgressUpdate`：每轮正常轮询写完 `progress-summary.*` 后触发。
- `onIssueIntegrated`：issue 分支成功合入本地基准分支，进入待验证状态后触发，适合重启本地后端/前端验证环境。
- `onIssueRejected`：人工验证失败并执行 `reject` 回滚后触发。
- `onIssueClosedPushed`：CNB issue 关闭，基准分支 push 成功并清理 issue worktree/branch 后触发。

hook 命令的工作目录是 `project.workspace`。命令失败会在 `runner.log` 里记录 `hook_failed`。HTTP webhook 返回非 2xx 或请求失败会记录 `webhook_failed`。

command hook 环境变量和 webhook JSON 字段语义一致：

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

webhook payload 使用 camelCase 字段：

```json
{
  "event": "onIssueIntegrated",
  "issueNumber": "77",
  "issueTitle": "example",
  "stateDir": "/Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner",
  "workspace": "/Users/m4/Documents/code/zcode",
  "model": "gpt-5.5",
  "reasoningEffort": "xhigh",
  "workflowStatus": "AWAITING_VERIFY"
}
```

重启验证环境建议放到脚本里，再由 hook 调用，避免把复杂 shell 写进 YAML：

```yaml
hooks:
  onIssueIntegrated:
    command: "rtk ./scripts/restart-validation-env.sh"
    timeoutSeconds: 180
```

脚本示例：

```bash
#!/usr/bin/env bash
set -euo pipefail

cd /Users/m4/Documents/code/zcode/ml-svc-zcode
rtk go build -o main main.go

pids="$(lsof -tiTCP:8022 -sTCP:LISTEN || true)"
if [ -n "$pids" ]; then
  kill $pids
fi

nohup ./main > /tmp/ml-svc-zcode.log 2>&1 &
```

## 人工验证闭环

worker 完成后，runner 会把 issue 分支合并到本地基准分支等待人工验证：

```text
ml-svc-zcode -> ai-v2.0
ml-web-zcode -> poc/api-adapter-validation
```

当前有哪些 issue 等待验证以 `current-validation.md` 为准。这样用户只需要验证正常基准分支对应的本地环境，不需要知道或切换 `cnb/issue-*` 分支。

`cnb/issue-*` 分支默认只作为本地临时分支使用，不能 push 到远端；runner 只会 push `ai-v2.0`、`poc/api-adapter-validation` 等目标验证分支。

也可以直接执行：

```bash
rtk go run ./cmd/issue-agent-runner current -c .local_cache/cnb-issue-runner/runner.local.yaml
```

验证失败：

```bash
cd /Users/m4/Documents/code/zcode/issue-agent-runner
rtk go run ./cmd/issue-agent-runner reject -c .local_cache/cnb-issue-runner/runner.local.yaml <ISSUE_NUMBER>
```

验证通过：

```text
关闭 CNB issue
```

runner 检测到 issue closed 后，会 push 相关基准分支，并删除本地 issue worktree/branch。

## 常见排查

确认 runner 是否在跑：

```bash
tmux ls | rg issue-agent-runner
ps -ef | rg -i "issue-agent-runner run|codex exec"
```

确认 callback 是否监听：

```bash
lsof -nP -iTCP:8791 -sTCP:LISTEN
```

确认当前有没有任务执行：

```bash
cd /Users/m4/Documents/code/zcode/issue-agent-runner
rtk go run ./cmd/issue-agent-runner status -c .local_cache/cnb-issue-runner/runner.local.yaml
tail -n 30 /Users/m4/Documents/code/zcode/.local_cache/cnb-issue-runner/runner.log
```

如果 `Active workers: 0` 且日志只有 `poll_start` / `poll_complete`，说明 runner 在轮询，但当前没有任务被派发。
