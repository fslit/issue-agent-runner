package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderTemplateWithIssueData(t *testing.T) {
	templatePath := filepath.Join(t.TempDir(), "worker.md")
	require.NoError(t, os.WriteFile(templatePath, []byte(`Repo: {{ .RepoURL }}
Issue: #{{ .Number }}
Title: {{ .Title }}
Result: {{ .ResultPath }}
Command: {{ .CommandPrefix }}
{{- range .Repositories }}
Repo {{ .Name }} -> {{ .WorktreePath }} ({{ .IssueBranch }} <- {{ .BaselineBranch }})
{{- end }}
Body:
{{ .Body }}
`), 0o644))

	rendered, err := RenderFile(templatePath, Data{
		RepoURL:       "https://cnb.cool/dianyuniot/mlink/ml-svc-zcode",
		Number:        "35",
		Title:         "fix button",
		Body:          "body text",
		ResultPath:    "/tmp/result.json",
		CommandPrefix: "rtk",
		Repositories: []RepositoryData{
			{
				Name:           "ml-svc-zcode",
				WorktreePath:   "/tmp/worktrees/issue-35/ml-svc-zcode",
				IssueBranch:    "cnb/issue-35/ml-svc-zcode",
				BaselineBranch: "ai-v2.0",
			},
		},
	})
	require.NoError(t, err)

	require.Contains(t, rendered, "Issue: #35")
	require.Contains(t, rendered, "Result: /tmp/result.json")
	require.Contains(t, rendered, "Command: rtk")
	require.Contains(t, rendered, "Repo ml-svc-zcode -> /tmp/worktrees/issue-35/ml-svc-zcode")
}

func TestRenderTemplateFailsForMissingKeys(t *testing.T) {
	_, err := RenderText("{{ .Missing }}", Data{})
	require.ErrorContains(t, err, "map has no entry for key")
}

func TestWorkerPromptRequiresChineseCommitDescriptions(t *testing.T) {
	templatePath := filepath.Join("..", "..", "templates", "worker_prompt.md")
	rendered, err := RenderFile(templatePath, Data{
		RepoURL:       "https://cnb.cool/dianyuniot/mlink/ml-svc-zcode",
		Number:        "72",
		Title:         "field output label",
		Body:          "body text",
		ResultPath:    "/tmp/result.json",
		CommandPrefix: "rtk",
		Repositories: []RepositoryData{
			{
				Name:           "ml-svc-zcode",
				WorktreePath:   "/tmp/worktrees/issue-72/ml-svc-zcode",
				IssueBranch:    "cnb/issue-72/ml-svc-zcode",
				BaselineBranch: "ai-v2.0",
			},
		},
	})
	require.NoError(t, err)

	require.Contains(t, rendered, "fix(issue-72): <中文简短问题描述>")
	require.Contains(t, rendered, "问题描述必须使用中文")
	require.Contains(t, rendered, "commit body 必须使用中文描述")
	require.Contains(t, rendered, "修复内容:")
	require.Contains(t, rendered, "验证:")
	require.Contains(t, rendered, "所有 CNB 评论、任务回复、阻塞说明和完成说明都必须使用中文")
}

func TestWorkerPromptForbidsPushingIssueBranches(t *testing.T) {
	templatePath := filepath.Join("..", "..", "templates", "worker_prompt.md")
	rendered, err := RenderFile(templatePath, Data{
		RepoURL:       "https://cnb.cool/dianyuniot/mlink/ml-web-zcode",
		Number:        "45",
		Title:         "data source filter",
		Body:          "body text",
		ResultPath:    "/tmp/result.json",
		CommandPrefix: "rtk",
		Repositories: []RepositoryData{
			{
				Name:           "ml-web-zcode",
				WorktreePath:   "/tmp/worktrees/issue-45/ml-web-zcode",
				IssueBranch:    "cnb/issue-45/ml-web-zcode",
				BaselineBranch: "poc/api-adapter-validation",
			},
		},
	})
	require.NoError(t, err)

	require.Contains(t, rendered, "不要 push `cnb/issue-*` 分支")
	require.Contains(t, rendered, "`cnb/issue-*` 是本地临时分支")
}
