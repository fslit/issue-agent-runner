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
	if _, err := m.runCmd(ctx, repoPath, "checkout", baselineBranch); err != nil {
		return MergeResult{}, err
	}
	if issueBranch == baselineBranch {
		head, err := m.revParse(ctx, repoPath, "HEAD")
		if err != nil {
			return MergeResult{}, err
		}
		return MergeResult{MergeCommit: head}, nil
	}

	mergeArgs := []string{"merge", "--no-edit"}
	if mergeMode == "no-ff" {
		mergeArgs = append(mergeArgs, "--no-ff")
	}
	mergeArgs = append(mergeArgs, issueBranch)
	if _, err := m.runCmd(ctx, repoPath, mergeArgs...); err != nil {
		files, filesErr := m.conflictFiles(ctx, repoPath)
		if filesErr == nil && len(files) > 0 {
			_, _ = m.runCmd(ctx, repoPath, "merge", "--abort")
			return MergeResult{ConflictFiles: files}, &MergeConflictError{
				RepoPath:       repoPath,
				BaselineBranch: baselineBranch,
				IssueBranch:    issueBranch,
				Files:          files,
			}
		}
		return MergeResult{}, err
	}

	head, err := m.revParse(ctx, repoPath, "HEAD")
	if err != nil {
		return MergeResult{}, err
	}
	return MergeResult{MergeCommit: head}, nil
}

func (m *Manager) RevertMergeCommit(ctx context.Context, repoPath string, baselineBranch string, mergeCommit string) (string, error) {
	if _, err := m.runCmd(ctx, repoPath, "checkout", baselineBranch); err != nil {
		return "", err
	}
	if _, err := m.runCmd(ctx, repoPath, "revert", "-m", "1", "--no-edit", mergeCommit); err != nil {
		return "", err
	}
	return m.revParse(ctx, repoPath, "HEAD")
}

func (m *Manager) PushBaselineBranch(ctx context.Context, repoPath string, baselineBranch string) (string, error) {
	if _, err := m.runCmd(ctx, repoPath, "push", "origin", baselineBranch); err != nil {
		return "", err
	}
	return m.revParse(ctx, repoPath, "HEAD")
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

func samePath(left string, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
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
