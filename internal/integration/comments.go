package integration

import (
	"fmt"
	"sort"
	"strings"

	"issue-agent-runner/internal/domain"
)

type AgentInfo struct {
	Model           string
	ReasoningEffort string
}

func AwaitingVerifyComment(issue domain.Issue, state domain.IssueState, agent AgentInfo) string {
	lines := []string{
		fmt.Sprintf("问题 #%s 已合入并推送到目标分支，等待人工验证。", issue.Number),
	}
	lines = appendAgentInfo(lines, agent)
	if state.Result != nil && state.Result.Summary != "" {
		lines = append(lines, "", "修复摘要:", state.Result.Summary)
	}

	repos := sortedRepoNames(state.Repositories)
	if len(repos) > 0 {
		lines = append(lines, "", "已合入基准分支:")
		for _, repo := range repos {
			item := state.Repositories[repo]
			if item.BaselineMergeCommit == "" {
				continue
			}
			lines = append(lines, fmt.Sprintf("- `%s` -> `%s` (%s)", repo, item.BaselineBranch, item.BaselineMergeCommit))
		}
	}
	if state.Result != nil && len(state.Result.Verification) > 0 {
		lines = append(lines, "", "Worker 验证:")
		for _, command := range state.Result.Verification {
			lines = append(lines, "- `"+command+"`")
		}
	}
	lines = append(lines, "", "验证通过后关闭该 CNB issue；验证失败请执行 `issue-agent-runner reject "+issue.Number+"` 回滚该 issue 的验证合入。")
	return strings.Join(lines, "\n")
}

func MergeConflictComment(issue domain.Issue, state domain.IssueState, agent AgentInfo) string {
	lines := []string{
		fmt.Sprintf("问题 #%s 无法自动合入基准分支，已暂停等待人工处理。", issue.Number),
	}
	lines = appendAgentInfo(lines, agent)
	repos := sortedRepoNames(state.Repositories)
	for _, repo := range repos {
		item := state.Repositories[repo]
		if len(item.ConflictFiles) == 0 {
			continue
		}
		lines = append(lines, "", fmt.Sprintf("冲突位置 `%s` -> `%s`:", repo, item.BaselineBranch))
		for _, file := range item.ConflictFiles {
			lines = append(lines, "- `"+file+"`")
		}
	}
	return strings.Join(lines, "\n")
}

func RejectComment(issue domain.Issue, agent AgentInfo) string {
	lines := []string{
		fmt.Sprintf("问题 #%s 人工验证未通过。已回滚基准分支的合并提交并推送回滚结果，任务已退回队列。", issue.Number),
	}
	lines = appendAgentInfo(lines, agent)
	return strings.Join(lines, "\n")
}

func WorkerFailureComment(issue domain.Issue, state domain.IssueState, agent AgentInfo) string {
	lines := []string{
		fmt.Sprintf("问题 #%s 已失败并暂停，等待人工处理。", issue.Number),
	}
	lines = appendAgentInfo(lines, agent)
	if state.Attempts > 0 {
		lines = append(lines, "", fmt.Sprintf("已失败 %d 次。", state.Attempts))
	}
	if state.LastError != "" {
		lines = append(lines, "", "失败原因:", state.LastError)
	}
	if state.PauseReason != "" {
		lines = append(lines, "", "暂停原因:", state.PauseReason)
	}
	return strings.Join(lines, "\n")
}

func ClosedPushedComment(issue domain.Issue, state domain.IssueState, agent AgentInfo) string {
	lines := []string{
		fmt.Sprintf("问题 #%s 已关闭。基准分支已推送，相关本地 issue worktree/branch 已清理。", issue.Number),
	}
	lines = appendAgentInfo(lines, agent)
	repos := sortedRepoNames(state.Repositories)
	if len(repos) > 0 {
		lines = append(lines, "", "已推送基准分支:")
		for _, repo := range repos {
			item := state.Repositories[repo]
			if item.PushedBaselineSHA == "" {
				continue
			}
			lines = append(lines, fmt.Sprintf("- `%s` -> `%s` (%s)", repo, item.BaselineBranch, item.PushedBaselineSHA))
		}
	}
	return strings.Join(lines, "\n")
}

func appendAgentInfo(lines []string, agent AgentInfo) []string {
	switch {
	case agent.Model != "" && agent.ReasoningEffort != "":
		return append(lines, fmt.Sprintf("Model: `%s` (reasoning: `%s`)", agent.Model, agent.ReasoningEffort))
	case agent.Model != "":
		return append(lines, fmt.Sprintf("Model: `%s`", agent.Model))
	case agent.ReasoningEffort != "":
		return append(lines, fmt.Sprintf("Reasoning: `%s`", agent.ReasoningEffort))
	default:
		return lines
	}
}

func sortedRepoNames(items map[string]domain.IssueRepoState) []string {
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
