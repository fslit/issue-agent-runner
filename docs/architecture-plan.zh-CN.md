# IssueAgent Runner 架构规划

## 1. 项目名称

项目名：**IssueAgent Runner**

目录名：`issue-agent-runner`

定位：一个通用的 Go 后台调度器，用于从 CNB/GitHub/GitLab 等平台拉取 issue，派发 AI worker 处理，并通过状态机、重试退避、暂停保护、浏览器验证和 hook 回调沉淀成可复用的自动化机制。

## 2. 目标

IssueAgent Runner 负责稳定调度，不直接绑定某个业务项目。

核心目标：

- 定时拉取 open issue。
- 控制 AI worker 并发数量。
- 防止重复派发同一个 issue。
- 记录 attempts、lastError、nextRetryAt、paused 等状态。
- 对容量不足、额度不足、worker 卡死等场景做保护。
- 输出本地进展摘要，降低反复读日志和聊天查询成本。
- 支持可选浏览器验证，确认前端功能真的可用。
- 通过 hook 支持项目差异化行为。

非目标：

- 第一版不做 Web 管理后台。
- 第一版不做复杂分布式队列。
- 第一版不引入数据库，使用本地 state 文件即可。

## 3. 技术选型

推荐技术栈：

```text
Go 1.22+
cobra              CLI 命令
yaml.v3            配置解析
log/slog           标准库结构化日志
net/http           issue 平台 API client
os/exec            启动 Codex、Playwright 等外部进程
context            超时、取消、信号传播
gofrs/flock        state 文件锁
testify            测试断言
```

暂不需要：

- Gin/Echo/Fiber：第一版没有 HTTP 服务端。
- ORM：本地 JSON state 足够。
- 消息队列：本地进程和 state 文件足够。

## 4. 总体架构

```text
cmd/issue-agent-runner/main.go

internal/config        # YAML 配置加载、默认值、校验
internal/runner        # 调度核心和状态机
internal/state         # state.json 读写、锁、原子写、迁移
internal/provider      # issue 平台接口
internal/provider/cnb
internal/provider/github
internal/executor      # worker 执行器接口
internal/executor/codex
internal/browser       # Playwright / DevTools 浏览器验证
internal/prompt        # worker prompt 模板渲染
internal/report        # progress-summary.md/json
internal/hooks         # hook 回调
internal/logx          # slog 包装
```

Runner core 只依赖接口，不依赖 CNB、GitHub、Codex、Playwright 的具体实现。

## 5. 核心接口

### IssueProvider

```go
type IssueProvider interface {
    ListOpen(ctx context.Context) ([]Issue, error)
    Get(ctx context.Context, number string) (Issue, error)
    AddLabels(ctx context.Context, number string, labels []string) error
    RemoveLabels(ctx context.Context, number string, labels []string) error
    AddComment(ctx context.Context, number string, body string) error
}
```

第一版实现 CNB provider，后续再加 GitHub provider。

Provider 约束：

- `ListOpen` 必须处理分页。
- 第三方 API 返回值一律按不可信输入处理，先校验再转成内部 `Issue`。
- 标签操作使用增量语义，避免覆盖用户或其他自动化已经设置的标签。
- 401/403 归类为 `AUTH`，429 或额度响应归类为 `QUOTA`。

### Executor

```go
type Executor interface {
    Start(ctx context.Context, task Task) (WorkerHandle, error)
    Alive(ctx context.Context, h WorkerHandle) bool
    Result(ctx context.Context, h WorkerHandle) WorkerResult
    Terminate(ctx context.Context, h WorkerHandle) error
}
```

第一版实现 Codex executor：

```text
codex --ask-for-approval never exec --cd <workspace> --sandbox danger-full-access -
```

Executor 约束：

- worker 只负责修改代码、运行验证、写日志和 `result.json`。
- `result.json` 必须包含结构化修改文件清单：文件路径、起始行号或关键行号、修改的方法名/函数名/组件名、新增方法名/新增组件名、文件级说明。
- 远端 issue 的标签和评论由 runner 统一处理，避免 worker 与 runner 同时修改远端状态。
- Codex 进程必须放入独立 process group，stale terminate 先杀进程组。
- `Result` 优先读取结构化 `result.json`，缺失时再从日志 exit marker 和错误文本降级分类。

### BrowserVerifier

```go
type BrowserVerifier interface {
    Verify(ctx context.Context, task Task) (BrowserResult, error)
}
```

第一版优先接 Playwright。Chrome DevTools MCP 后续作为高级验证能力。

### Hook

```go
type Hook interface {
    OnIssueDiscovered(ctx context.Context, e IssueEvent) HookResult
    OnIssueClaim(ctx context.Context, e IssueEvent) HookResult
    OnWorkerStart(ctx context.Context, e WorkerEvent) HookResult
    OnWorkerExit(ctx context.Context, e WorkerExitEvent) HookResult
    OnIssueSuccess(ctx context.Context, e IssueEvent) HookResult
    OnIssueFailure(ctx context.Context, e FailureEvent) HookResult
    OnIssuePaused(ctx context.Context, e FailureEvent) HookResult
    OnProgressUpdate(ctx context.Context, e ProgressEvent) HookResult
}
```

Hook 用于项目差异化行为，不能破坏 runner 的核心状态机。

## 6. 状态机

| 状态 | 进入条件 | runner 动作 | 下一状态 |
| --- | --- | --- | --- |
| `DISCOVERED` | open issue 被 provider 拉到 | 记录 issue 快照，触发候选过滤 | `CLAIMED` 或保持等待 |
| `CLAIMED` | 满足候选规则且有并发容量 | 增量添加 `处理中` 标签，渲染 prompt | `RUNNING` 或 `FAILED_RETRYABLE` |
| `RUNNING` | executor 启动成功 | 记录 worker pid、log、prompt、result 路径 | `VERIFYING`、`SUCCEEDED`、`FAILED_RETRYABLE`、`PAUSED` |
| `VERIFYING` | worker 成功且 issue 需要浏览器验证 | 运行 verifier，写 artifacts | `SUCCEEDED` 或 `FAILED_RETRYABLE` |
| `SUCCEEDED` | worker 和必要验证成功 | 移除 `处理中`，添加 `已处理`，评论摘要 | 终态 |
| `FAILED_RETRYABLE` | 可重试失败、capacity、stale 或测试失败 | 记录 attempts、lastError、nextRetryAt，必要时清理 worker | `CLAIMED` 或 `PAUSED` |
| `PAUSED` | quota/auth 或 attempts 达到上限 | 移除 `处理中`，添加 `需人工确认` 或 `处理失败`，评论阻塞原因 | 人工 `resume` 后回到候选池 |

核心规则：

- runner core 控制状态流转。
- hook 只能扩展节点行为。
- attempts/backoff/pause/stale 由 runner core 统一处理。
- worker 不直接设置终态标签；终态标签和评论是 runner 的副作用。

## 7. 调度流程

每轮 `intervalSeconds` 执行：

```text
1. 加锁读取 state.json
2. 拉取 open issues
3. 清理远端已处理 issue 的 worker
4. 检测 worker 是否存活
5. 检测 worker 日志是否 stale
6. 记录 worker 成功/失败/退出
7. 更新 attempts、lastError、nextRetryAt、paused
8. 计算可用并发
9. 过滤候选 issue
10. 生成 prompt
11. 启动 worker
12. 写 state.json
13. 写 progress-summary.md/json
14. 触发 OnProgressUpdate hook
```

## 8. 候选 issue 规则

一个 issue 必须满足：

```text
state=open
没有 skip labels
不在 active workers 中
paused=false
now >= nextRetryAt
并发容量 > 0
```

默认跳过标签：

```yaml
skip:
  - 已处理
  - 处理中
  - 处理失败
  - 需人工确认
```

## 9. 错误分类和退避策略

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
```

默认策略：

```text
MODEL_CAPACITY -> 15 分钟退避
QUOTA          -> 直接 pause，避免继续烧 token
AUTH           -> pause
STALE          -> 5 分钟退避
TEST_FAILURE   -> 5 分钟退避，最多 3 次
UNKNOWN        -> 5 分钟退避，最多 3 次
```

连续失败：

```text
attempts >= maxAttempts -> paused=true
```

## 10. State 文件设计

`state.json`：

```json
{
  "version": 1,
  "workers": {
    "35": {
      "issueNumber": "35",
      "pid": 12345,
      "title": "...",
      "startedAt": "2026-05-07T12:00:00+08:00",
      "logPath": ".../issue-35.log",
      "promptPath": ".../issue-35.prompt.md",
      "resultPath": ".../issue-35.result.json"
    }
  },
  "issues": {
    "35": {
      "attempts": 2,
      "paused": false,
      "pauseReason": "",
      "lastExitCode": 1,
      "lastError": "Selected model is at capacity",
      "lastStartedAt": "...",
      "lastFinishedAt": "...",
      "nextRetryAt": "..."
    }
  }
}
```

写入要求：

- 使用文件锁。
- 先写 `.tmp`，再 rename。
- 带 `version`，未来支持迁移。
- 第一版必须兼容旧 Python runner 的无 `version` state。
- 第一版必须兼容旧 worker 字段 `log`，读取时等价于 `logPath`。
- 时间字段读取时兼容 Unix 秒和 RFC3339；写入可以先保持 Unix 秒，减少迁移风险。

## 11. 配置文件

参考 `configs/runner.example.yaml`。

配置原则：

- 项目路径、provider、executor、标签、退避参数全部配置化。
- prompt 模板外置。
- browser 验证默认可关闭。
- hook 命令和 webhook 都通过配置注册。

## 12. 浏览器验证

浏览器验证是可选验收阶段，不默认启用所有 issue。

启用条件：

- issue 有 `前端`、`UI`、`浏览器验证` 等标签。
- worker 修改了前端路径。
- prompt 或 hook 判断该 issue 涉及页面交互。

验证方式：

1. Playwright：稳定流程优先。
2. Chrome DevTools MCP：复杂 UI 诊断后续支持。

产物目录：

```text
.local_cache/issue-agent-runner/browser/issue-37/
  report.md
  report.json
  before.png
  after.png
  console.log
  network.json
  trace.zip
```

通过条件：

- 页面能打开。
- 关键交互能完成。
- console 无 error。
- 关键 network 请求成功。
- 截图/DOM 符合预期。

## 13. Hook 设计

推荐三类 hook：

1. 内置 Go hook：稳定、类型安全。
2. 外部命令 hook：项目自定义脚本。
3. HTTP webhook：通知飞书、企业微信、自建平台。

配置示例：

```yaml
hooks:
  onIssueClaim:
    command: ./scripts/hooks/on-claim.sh
    timeoutSeconds: 30

  onIssuePaused:
    webhook:
      url: https://example.com/webhook
      method: POST

  onProgressUpdate:
    command: ./scripts/hooks/progress-extra.sh
```

外部命令通过 stdin 接收 JSON 事件，通过 stdout 返回 JSON 结果。

## 14. CLI 命令

```bash
issue-agent-runner run -c runner.yaml
issue-agent-runner once -c runner.yaml
issue-agent-runner status -c runner.yaml
issue-agent-runner summary -c runner.yaml
issue-agent-runner pause -c runner.yaml 35
issue-agent-runner resume -c runner.yaml 35 36 --clear-remote-running-label
issue-agent-runner reset-attempts -c runner.yaml 35
issue-agent-runner stop-worker -c runner.yaml 35
```

## 15. 进展输出

生成：

```text
progress-summary.md
progress-summary.json
```

Markdown 给人看，JSON 给脚本或 UI 用。

摘要内容：

```text
Updated
Open issues
Active workers
Retry backoff issues
Paused issues
Active list
Waiting retry list
Paused list
```

## 16. 部署方式

MVP：

```bash
issue-agent-runner run -c runner.yaml
```

macOS：

```text
launchd plist
```

Linux：

```text
systemd service
```

短期也可继续用 tmux。

## 17. 测试策略

单元测试：

- 配置默认值和校验。
- label 过滤。
- 并发容量计算。
- attempts 递增。
- 普通 backoff。
- capacity backoff。
- quota pause。
- stale worker 检测。
- resume 清状态。
- state 原子写。
- progress summary 渲染。

集成测试：

- fake provider + fake executor。
- worker success。
- worker failure。
- worker stale。
- remote 已处理释放 worker。
- maxAttempts pause。
- resume 后重新派发。

浏览器测试：

- Playwright verifier 成功/失败。
- console error 捕获。
- network error 捕获。
- artifact 生成。

## 18. 实施阶段

### Phase 1：Go MVP

- cobra CLI
- YAML config
- state store
- CNB provider
- Codex executor
- runner loop
- attempts/backoff/pause
- stale worker kill
- quota/capacity 分类
- progress summary
- run/once/status/summary/resume/pause/reset-attempts/stop-worker
- legacy state 兼容

### Phase 2：稳定性增强

- 更完整日志
- 全局 quota/auth pause
- 更细的错误分类规则
- 外部命令 hook
- launchd/systemd 模板

### Phase 3：浏览器验证

- Playwright verifier
- dev server health check
- artifacts
- browser result 接入验收流程

### Phase 4：复用和扩展

- GitHub provider
- HTTP webhook
- README 和 ADR

## 19. 迁移路线

```text
1. Go runner 先兼容当前 state 目录 `.local_cache/cnb-issue-runner` 和旧 state schema。
2. 用 once 对比 Python runner 行为。
3. 停 Python daemon。
4. 启 Go daemon。
5. 观察 1 天。
6. 保留 Python fallback 一段时间。
7. 稳定后删除 Python daemon。
```

## 20. MVP 验收标准

第一版必须满足：

```text
能从 CNB 拉 open issue
能按标签跳过
能最多启动 5 个 Codex worker
能写 state.json
能写 progress-summary.md
能识别 worker 成功/失败
能 capacity backoff
能 quota pause
能 stale terminate
能 resume 已暂停 issue
核心调度逻辑有单元测试
```

## 21. 关键设计原则

- runner core 管确定性：调度、状态、并发、重试、暂停。
- hook 管项目差异：prompt、验证、通知、标签、评论、测试策略。
- provider 管平台差异：CNB、GitHub、GitLab。
- executor 管执行差异：Codex、shell、其他 agent。
- browser verifier 管真实 UI 验收。

这能保证工具既能复用到其他项目，又不会把核心调度做成不可控脚本集合。
