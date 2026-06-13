package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"issue-agent-runner/internal/config"
	"issue-agent-runner/internal/domain"
)

type Manager struct {
	runCmd func(ctx context.Context, dir string, args ...string) (string, error)
}

func New() *Manager {
	return &Manager{runCmd: runGit}
}

type MergeResult struct {
	MergeCommit   string
	ConflictFiles []string
}

type MergeConflictError struct {
	RepoPath       string
	BaselineBranch string
	IssueBranch    string
	Files          []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge conflict integrating %s into %s", e.IssueBranch, e.BaselineBranch)
}

func (m *Manager) PrepareIssueWorktree(ctx context.Context, worktreeRoot string, issueNumber string, repo config.RepositoryConfig) (domain.TaskRepository, error) {
	if repo.DirectBaseline {
		if _, err := m.runCmd(ctx, repo.Path, "checkout", repo.BaselineBranch); err != nil {
			return domain.TaskRepository{}, err
		}
		status, err := m.runCmd(ctx, repo.Path, "status", "--porcelain")
		if err != nil {
			return domain.TaskRepository{}, err
		}
		if strings.TrimSpace(status) != "" {
			return domain.TaskRepository{}, fmt.Errorf("direct baseline workspace %s is dirty", repo.Path)
		}
		return domain.TaskRepository{
			Name:           repo.Name,
			Path:           repo.Path,
			WorktreePath:   repo.Path,
			IssueBranch:    repo.BaselineBranch,
			BaselineBranch: repo.BaselineBranch,
		}, nil
	}

	branch := issueBranchName(issueNumber, repo)
	worktreePath := filepath.Join(worktreeRoot, "issue-"+issueNumber, repo.Name)

	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return domain.TaskRepository{}, fmt.Errorf("create worktree parent: %w", err)
	}

	if _, err := os.Stat(worktreePath); err == nil {
		return domain.TaskRepository{
			Name:           repo.Name,
			Path:           repo.Path,
			WorktreePath:   worktreePath,
			IssueBranch:    branch,
			BaselineBranch: repo.BaselineBranch,
		}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.TaskRepository{}, fmt.Errorf("stat worktree path: %w", err)
	}

	existingWorktreePath, found, err := m.worktreePathForBranch(ctx, repo.Path, branch)
	if err != nil {
		return domain.TaskRepository{}, err
	}
	if found {
		existingWorktreePath = canonicalPath(existingWorktreePath)
		return domain.TaskRepository{
			Name:           repo.Name,
			Path:           repo.Path,
			WorktreePath:   existingWorktreePath,
			IssueBranch:    branch,
			BaselineBranch: repo.BaselineBranch,
		}, nil
	}

	if _, err := m.runCmd(ctx, repo.Path, "worktree", "add", "-B", branch, worktreePath, repo.BaselineBranch); err != nil {
		return domain.TaskRepository{}, err
	}
	return domain.TaskRepository{
		Name:           repo.Name,
		Path:           repo.Path,
		WorktreePath:   worktreePath,
		IssueBranch:    branch,
		BaselineBranch: repo.BaselineBranch,
	}, nil
}

func (m *Manager) MergeIssueBranch(ctx context.Context, repoPath string, baselineBranch string, issueBranch string, mergeMode string) (MergeResult, error) {
	if mergeMode == "" {
		mergeMode = "no-ff"
	}
	if issueBranch == baselineBranch {
		head, err := m.revParse(ctx, repoPath, issueBranch)
		if err != nil {
			return MergeResult{}, err
		}
		return MergeResult{MergeCommit: head}, nil
	}

	integrationPath, err := m.prepareIntegrationWorktree(ctx, repoPath, baselineBranch)
	if err != nil {
		return MergeResult{}, err
	}
	mergeArgs := []string{"merge", "--no-edit"}
	if mergeMode == "no-ff" {
		mergeArgs = append(mergeArgs, "--no-ff")
	}
	mergeArgs = append(mergeArgs, issueBranch)
	if _, err := m.runCmd(ctx, integrationPath, mergeArgs...); err != nil {
		files, filesErr := m.conflictFiles(ctx, integrationPath)
		if filesErr == nil && len(files) > 0 {
			_, _ = m.runCmd(ctx, integrationPath, "merge", "--abort")
			return MergeResult{ConflictFiles: files}, &MergeConflictError{
				RepoPath:       repoPath,
				BaselineBranch: baselineBranch,
				IssueBranch:    issueBranch,
				Files:          files,
			}
		}
		return MergeResult{}, err
	}

	head, err := m.revParse(ctx, integrationPath, "HEAD")
	if err != nil {
		return MergeResult{}, err
	}
	return MergeResult{MergeCommit: head}, nil
}

func (m *Manager) RevertMergeCommit(ctx context.Context, repoPath string, baselineBranch string, mergeCommit string) (string, error) {
	integrationPath, found, err := m.existingIntegrationWorktree(ctx, repoPath, baselineBranch)
	if err != nil {
		return "", err
	}
	if !found {
		integrationPath, err = m.prepareIntegrationWorktree(ctx, repoPath, baselineBranch)
		if err != nil {
			return "", err
		}
	}
	if _, err := m.runCmd(ctx, integrationPath, "merge-base", "--is-ancestor", mergeCommit, "HEAD"); err != nil {
		if resetErr := m.resetIntegrationWorktree(ctx, repoPath, integrationPath, baselineBranch); resetErr != nil {
			return "", resetErr
		}
		if _, ancestorErr := m.runCmd(ctx, integrationPath, "merge-base", "--is-ancestor", mergeCommit, "HEAD"); ancestorErr != nil {
			return "", ancestorErr
		}
	}
	if _, err := m.runCmd(ctx, integrationPath, "revert", "-m", "1", "--no-edit", mergeCommit); err != nil {
		return "", err
	}
	return m.revParse(ctx, integrationPath, "HEAD")
}

func (m *Manager) PushBaselineBranch(ctx context.Context, repoPath string, baselineBranch string) (string, error) {
	pushPath, found, err := m.existingIntegrationWorktree(ctx, repoPath, baselineBranch)
	if err != nil {
		return "", err
	}
	if !found {
		pushPath = repoPath
	}
	if _, err := m.runCmd(ctx, pushPath, "push", "origin", "HEAD:refs/heads/"+baselineBranch); err != nil {
		return "", err
	}
	return m.revParse(ctx, pushPath, "HEAD")
}

func (m *Manager) CleanupIssueWorktree(ctx context.Context, repoPath string, worktreePath string, issueBranch string) error {
	if samePath(repoPath, worktreePath) {
		return nil
	}
	if _, err := os.Stat(worktreePath); err == nil {
		if _, err := m.runCmd(ctx, repoPath, "worktree", "remove", "--force", worktreePath); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if issueBranch == "" {
		return nil
	}
	if _, err := m.runCmd(ctx, repoPath, "branch", "-D", issueBranch); err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "not exist") {
			return nil
		}
		return err
	}
	return nil
}

func canonicalPath(path string) string {
	value, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(value)
}

func (m *Manager) worktreePathForBranch(ctx context.Context, repoPath string, branch string) (string, bool, error) {
	value, err := m.runCmd(ctx, repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false, err
	}
	targetRef := "refs/heads/" + branch
	var currentPath string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			currentPath = ""
			continue
		}
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			currentPath = path
			continue
		}
		if ref, ok := strings.CutPrefix(line, "branch "); ok && ref == targetRef && currentPath != "" {
			return currentPath, true, nil
		}
	}
	return "", false, nil
}

func samePath(left string, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func (m *Manager) prepareIntegrationWorktree(ctx context.Context, repoPath string, baselineBranch string) (string, error) {
	integrationPath, found, err := m.existingIntegrationWorktree(ctx, repoPath, baselineBranch)
	if err != nil {
		return "", err
	}
	if !found {
		integrationPath = integrationWorktreePath(repoPath, baselineBranch)
		if err := os.MkdirAll(filepath.Dir(integrationPath), 0o755); err != nil {
			return "", fmt.Errorf("create integration worktree parent: %w", err)
		}
		baseRef := baselineBranch
		if err := m.fetchBaseline(ctx, repoPath, baselineBranch); err == nil {
			baseRef = "refs/remotes/origin/" + baselineBranch
		}
		if _, err := m.runCmd(ctx, repoPath, "worktree", "add", "-B", integrationBranchName(baselineBranch), integrationPath, baseRef); err != nil {
			return "", err
		}
	}
	if err := m.resetIntegrationWorktree(ctx, repoPath, integrationPath, baselineBranch); err != nil {
		return "", err
	}
	return integrationPath, nil
}

func (m *Manager) resetIntegrationWorktree(ctx context.Context, repoPath string, integrationPath string, baselineBranch string) error {
	baseRef := baselineBranch
	if err := m.fetchBaseline(ctx, repoPath, baselineBranch); err == nil {
		baseRef = "refs/remotes/origin/" + baselineBranch
	}
	if _, err := m.runCmd(ctx, integrationPath, "checkout", integrationBranchName(baselineBranch)); err != nil {
		return err
	}
	if _, err := m.runCmd(ctx, integrationPath, "reset", "--hard", baseRef); err != nil {
		return err
	}
	if _, err := m.runCmd(ctx, integrationPath, "clean", "-fd"); err != nil {
		return err
	}
	return nil
}

func (m *Manager) existingIntegrationWorktree(ctx context.Context, repoPath string, baselineBranch string) (string, bool, error) {
	if existingPath, found, err := m.worktreePathForBranch(ctx, repoPath, integrationBranchName(baselineBranch)); err != nil {
		return "", false, err
	} else if found {
		return canonicalPath(existingPath), true, nil
	}
	integrationPath := integrationWorktreePath(repoPath, baselineBranch)
	if _, err := os.Stat(integrationPath); err == nil {
		if _, err := m.runCmd(ctx, integrationPath, "rev-parse", "--is-inside-work-tree"); err != nil {
			return "", false, err
		}
		return canonicalPath(integrationPath), true, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	return "", false, nil
}

func (m *Manager) fetchBaseline(ctx context.Context, repoPath string, baselineBranch string) error {
	_, err := m.runCmd(ctx, repoPath, "fetch", "origin", baselineBranch+":refs/remotes/origin/"+baselineBranch)
	return err
}

func integrationWorktreePath(repoPath string, baselineBranch string) string {
	return filepath.Join(repoPath, ".worktrees", "issue-agent-runner", safeBranchPath(baselineBranch))
}

func integrationBranchName(baselineBranch string) string {
	return "issue-agent-runner/integration/" + safeBranchPath(baselineBranch)
}

func safeBranchPath(branch string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-")
	value := strings.Trim(replacer.Replace(branch), "-.")
	if value == "" {
		return "baseline"
	}
	return value
}

func (m *Manager) revParse(ctx context.Context, repoPath string, ref string) (string, error) {
	value, err := m.runCmd(ctx, repoPath, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (m *Manager) conflictFiles(ctx context.Context, repoPath string) ([]string, error) {
	value, err := m.runCmd(ctx, repoPath, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(value), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func issueBranchName(issueNumber string, repo config.RepositoryConfig) string {
	prefix := repo.IssueBranchPrefix
	if prefix == "" {
		prefix = "cnb"
	}
	return fmt.Sprintf("%s/issue-%s/%s", prefix, issueNumber, repo.Name)
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return strings.TrimSpace(stdout.String()), nil
}
