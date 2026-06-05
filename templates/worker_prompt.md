继续处理 issue。必须只在下面列出的 worktree 路径内处理并提交本地 commit；directBaseline 模式下 worktree 就是目标分支所在仓库路径。

Repo: {{ .RepoURL }}
Issue: #{{ .Number }}
Title: {{ .Title }}
Result file: {{ .ResultPath }}

Body:
{{ .Body }}

Issue worktrees:
{{- range .Repositories }}
- repo: {{ .Name }}
  - worktree: {{ .WorktreePath }}
  - branch: {{ .IssueBranch }}
  - baseline: {{ .BaselineBranch }}
{{- else }}
- none provided; treat this as a runner bug and fail with `status=failed`.
{{- end }}

执行要求：
- 只处理这个 issue，不要顺手处理其他 issue。
- 所有 CNB 评论、任务回复、阻塞说明和完成说明都必须使用中文；不要使用英文作为主体回复。
- 先重新拉取 issue 详情确认状态；如果已经有 `已处理` 标签，直接结束。
- 修改前检查相关仓库 dirty 状态；不要还原用户或其他任务已有改动。
- 只允许在上面列出的 `worktree` 路径内修改代码、运行测试和提交 commit。
- 每个实际修改过的 repo 都必须保留本地 commit；如果 `branch` 与 `baseline` 相同，说明本次直接提交到目标分支。
- commit 标题建议格式：`fix(issue-{{ .Number }}): <中文简短问题描述>`，其中问题描述必须使用中文。
- commit body 必须使用中文描述，且包含 `修复内容:` 和 `验证:`，写清修了什么、验证命令和结果。
- shell 命令使用项目约定的命令前缀。
- 找到根因后实现最小修复，并运行相关测试/检查。
- 如果涉及前端、UI、浏览器交互或 console/network 报错，按配置执行浏览器验证。
- 结果必须包含修改文件清单：文件路径、起始行号或关键行号、修改的方法名/函数名/组件名、新增方法名/新增组件名，以及每个文件的简要说明。
- 不要直接修改远端 issue 标签；runner 会根据结果统一添加/移除 `处理中`、`已处理`、`处理失败`、`需人工确认`。
- 完成前必须写入 Result file，JSON 格式如下：

```json
{
  "status": "success",
  "summary": "一句话说明改动",
  "verification": ["实际运行过的命令"],
  "changedFiles": [
    {
      "path": "相对路径",
      "line": 123,
      "symbols": ["修改的方法名/函数名/组件名"],
      "newSymbols": ["新增方法名/新增组件名"],
      "description": "这个文件改了什么"
    }
  ],
  "repoResults": [
    {
      "repo": "ml-svc-zcode",
      "repoPath": "共享 workspace 里的 repo 路径",
      "worktreePath": "实际修改的 issue worktree 路径",
      "issueBranch": "实际工作分支；directBaseline 模式下等于目标分支",
      "baselineBranch": "ai-v2.0",
      "headSha": "worktree 当前 HEAD SHA",
      "commitSha": "本次 issue commit SHA"
    }
  ],
  "errorKind": "NONE",
  "errorMessage": ""
}
```

- 如果无法处理或验证失败，`status` 写 `blocked` 或 `failed`，说明 `errorKind` 和 `errorMessage`。
- 如果某个 repo 没有代码改动，不要为它生成 `repoResults`。
