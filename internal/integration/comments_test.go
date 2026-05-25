package integration

import (
	"testing"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/domain"
)

func TestAwaitingVerifyCommentIncludesMergedReposAndVerification(t *testing.T) {
	comment := AwaitingVerifyComment(domain.Issue{Number: "77"}, domain.IssueState{
		Result: &domain.WorkerResult{
			Summary:      "fix timeline toggle visibility",
			Verification: []string{"rtk go test ./..."},
		},
		Repositories: map[string]domain.IssueRepoState{
			"ml-svc-zcode": {BaselineBranch: "ai-v2.0", BaselineMergeCommit: "abc123"},
		},
	}, AgentInfo{Model: "gpt-5.5", ReasoningEffort: "xhigh"})

	require.Contains(t, comment, "问题 #77")
	require.Contains(t, comment, "Model: `gpt-5.5` (reasoning: `xhigh`)")
	require.Contains(t, comment, "fix timeline toggle visibility")
	require.Contains(t, comment, "`ml-svc-zcode` -> `ai-v2.0` (abc123)")
	require.Contains(t, comment, "`rtk go test ./...`")
}

func TestMergeConflictCommentIncludesConflictFiles(t *testing.T) {
	comment := MergeConflictComment(domain.Issue{Number: "89"}, domain.IssueState{
		Repositories: map[string]domain.IssueRepoState{
			"ml-web-zcode": {
				BaselineBranch: "poc/api-adapter-validation",
				ConflictFiles:  []string{"packages/app.tsx", "packages/index.less"},
			},
		},
	}, AgentInfo{Model: "gpt-5.5", ReasoningEffort: "xhigh"})

	require.Contains(t, comment, "问题 #89")
	require.Contains(t, comment, "Model: `gpt-5.5` (reasoning: `xhigh`)")
	require.Contains(t, comment, "`ml-web-zcode` -> `poc/api-adapter-validation`")
	require.Contains(t, comment, "`packages/app.tsx`")
	require.Contains(t, comment, "`packages/index.less`")
}

func TestRejectCommentIncludesIssueNumber(t *testing.T) {
	comment := RejectComment(domain.Issue{Number: "91"}, AgentInfo{Model: "gpt-5.5", ReasoningEffort: "xhigh"})
	require.Contains(t, comment, "问题 #91")
	require.Contains(t, comment, "Model: `gpt-5.5` (reasoning: `xhigh`)")
	require.Contains(t, comment, "人工验证未通过")
}

func TestClosedPushedCommentIncludesPushedShas(t *testing.T) {
	comment := ClosedPushedComment(domain.Issue{Number: "92"}, domain.IssueState{
		Repositories: map[string]domain.IssueRepoState{
			"ml-web-zcode": {
				BaselineBranch:    "poc/api-adapter-validation",
				PushedBaselineSHA: "def456",
			},
		},
	}, AgentInfo{Model: "gpt-5.5", ReasoningEffort: "xhigh"})

	require.Contains(t, comment, "问题 #92 已关闭")
	require.Contains(t, comment, "Model: `gpt-5.5` (reasoning: `xhigh`)")
	require.Contains(t, comment, "`ml-web-zcode` -> `poc/api-adapter-validation` (def456)")
}
