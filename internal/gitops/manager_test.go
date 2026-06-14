package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/config"
)

func TestPrepareIssueWorktreeCreatesBranchAndPath(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{
		Name:              "ml-svc-zcode",
		Path:              repoPath,
		IssueBranchPrefix: "cnb",
		BaselineBranch:    "ai-v2.0",
	}

	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.CleanupIssueWorktree(context.Background(), repoPath, worktree.WorktreePath, worktree.IssueBranch))
	})
	require.Equal(t, "cnb/issue-77/ml-svc-zcode", worktree.IssueBranch)
	require.DirExists(t, worktree.WorktreePath)

	current, err := runGit(context.Background(), worktree.WorktreePath, "branch", "--show-current")
	require.NoError(t, err)
	require.Equal(t, worktree.IssueBranch, current)
}

func TestPrepareIssueWorktreeDoesNotSetIssueBranchUpstream(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{
		Name:              "ml-svc-zcode",
		Path:              repoPath,
		IssueBranchPrefix: "cnb",
		BaselineBranch:    "ai-v2.0",
	}

	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.CleanupIssueWorktree(context.Background(), repoPath, worktree.WorktreePath, worktree.IssueBranch))
	})

	upstream, err := runGit(context.Background(), worktree.WorktreePath, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	require.Error(t, err)
	require.Empty(t, upstream)
}

func TestPrepareIssueWorktreeReusesExistingBranchWorktree(t *testing.T) {
	repoPath := createTestRepoWithBaseline(t, "poc/api-adapter-validation")
	manager := New()
	repo := config.RepositoryConfig{
		Name:              "ml-web-zcode",
		Path:              repoPath,
		IssueBranchPrefix: "cnb",
		BaselineBranch:    "poc/api-adapter-validation",
	}

	oldRoot := filepath.Join(t.TempDir(), "old-root")
	existing, err := manager.PrepareIssueWorktree(context.Background(), oldRoot, "37", repo)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.CleanupIssueWorktree(context.Background(), repoPath, existing.WorktreePath, existing.IssueBranch))
	})

	newRoot := filepath.Join(t.TempDir(), "new-root")
	reused, err := manager.PrepareIssueWorktree(context.Background(), newRoot, "37", repo)
	require.NoError(t, err)
	require.Equal(t, canonicalPath(existing.WorktreePath), reused.WorktreePath)
	require.Equal(t, existing.IssueBranch, reused.IssueBranch)
}

func TestPrepareIssueWorktreeDirectBaselineUsesRepositoryPath(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{
		Name:           "ml-svc-zcode",
		Path:           repoPath,
		BaselineBranch: "ai-v2.0",
		DirectBaseline: true,
	}

	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.NoError(t, err)
	require.Equal(t, repoPath, worktree.WorktreePath)
	require.Equal(t, repo.BaselineBranch, worktree.IssueBranch)

	current, err := runGit(context.Background(), repoPath, "branch", "--show-current")
	require.NoError(t, err)
	require.Equal(t, repo.BaselineBranch, current)
}

func TestPrepareIssueWorktreeDirectBaselineRejectsDirtyWorkspace(t *testing.T) {
	repoPath := createDirectTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{
		Name:           "ml-svc-zcode",
		Path:           repoPath,
		BaselineBranch: "ai-v2.0",
		DirectBaseline: true,
	}
	writeFile(t, filepath.Join(repoPath, "README.md"), "dirty\n")

	_, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.Error(t, err)
	require.Contains(t, err.Error(), "direct baseline workspace")
}

func TestMergeIssueBranchReturnsHeadWhenIssueBranchIsBaseline(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()

	writeFile(t, filepath.Join(repoPath, "feature.txt"), "direct baseline change\n")
	_, err := runGit(context.Background(), repoPath, "add", "feature.txt")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "commit", "-m", "fix(issue-77): direct baseline")
	require.NoError(t, err)
	head, err := runGit(context.Background(), repoPath, "rev-parse", "HEAD")
	require.NoError(t, err)

	result, err := manager.MergeIssueBranch(context.Background(), repoPath, "ai-v2.0", "ai-v2.0", "no-ff")
	require.NoError(t, err)
	require.Equal(t, head, result.MergeCommit)
}

func TestCleanupIssueWorktreeSkipsRepositoryPath(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()

	err := manager.CleanupIssueWorktree(context.Background(), repoPath, repoPath, "ai-v2.0")
	require.NoError(t, err)

	branches, err := runGit(context.Background(), repoPath, "branch", "--list", "ai-v2.0")
	require.NoError(t, err)
	require.NotEmpty(t, branches)
}

func TestMergeIssueBranchReturnsMergeCommit(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{Name: "ml-svc-zcode", Path: repoPath, IssueBranchPrefix: "cnb", BaselineBranch: "ai-v2.0"}
	cleanupIntegrationWorktree(t, repoPath, repo.BaselineBranch)
	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.CleanupIssueWorktree(context.Background(), repoPath, worktree.WorktreePath, worktree.IssueBranch))
	})

	writeFile(t, filepath.Join(worktree.WorktreePath, "feature.txt"), "issue 77\n")
	_, err = runGit(context.Background(), worktree.WorktreePath, "add", "feature.txt")
	require.NoError(t, err)
	_, err = runGit(context.Background(), worktree.WorktreePath, "commit", "-m", "fix(issue-77): add feature")
	require.NoError(t, err)

	result, err := manager.MergeIssueBranch(context.Background(), repoPath, repo.BaselineBranch, worktree.IssueBranch, "no-ff")
	require.NoError(t, err)
	require.NotEmpty(t, result.MergeCommit)

	head, err := runGit(context.Background(), integrationWorktreePath(repoPath, repo.BaselineBranch), "rev-parse", "HEAD")
	require.NoError(t, err)
	require.Equal(t, result.MergeCommit, head)
}

func TestMergeIssueBranchDoesNotCheckoutBaselineInRepositoryWorktree(t *testing.T) {
	repoPath := createTestRepoWithBaseline(t, "poc/api-adapter-validation")
	manager := New()
	repo := config.RepositoryConfig{Name: "ml-web-zcode", Path: repoPath, IssueBranchPrefix: "cnb", BaselineBranch: "poc/api-adapter-validation"}
	cleanupIntegrationWorktree(t, repoPath, repo.BaselineBranch)
	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "37", repo)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.CleanupIssueWorktree(context.Background(), repoPath, worktree.WorktreePath, worktree.IssueBranch))
	})

	writeFile(t, filepath.Join(worktree.WorktreePath, "feature.txt"), "issue 37\n")
	_, err = runGit(context.Background(), worktree.WorktreePath, "add", "feature.txt")
	require.NoError(t, err)
	_, err = runGit(context.Background(), worktree.WorktreePath, "commit", "-m", "fix(issue-37): add feature")
	require.NoError(t, err)

	_, err = runGit(context.Background(), repoPath, "checkout", "-b", "local-user-work")
	require.NoError(t, err)
	baselineWorktree := filepath.Join(t.TempDir(), "baseline-occupied")
	_, err = runGit(context.Background(), repoPath, "worktree", "add", baselineWorktree, repo.BaselineBranch)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = runGit(context.Background(), repoPath, "worktree", "remove", "--force", baselineWorktree)
	})

	result, err := manager.MergeIssueBranch(context.Background(), repoPath, repo.BaselineBranch, worktree.IssueBranch, "no-ff")
	require.NoError(t, err)
	require.NotEmpty(t, result.MergeCommit)

	current, err := runGit(context.Background(), repoPath, "branch", "--show-current")
	require.NoError(t, err)
	require.Equal(t, "local-user-work", current)
}

func TestMergeIssueBranchReturnsConflictFilesAndAborts(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{Name: "ml-svc-zcode", Path: repoPath, IssueBranchPrefix: "cnb", BaselineBranch: "ai-v2.0"}
	cleanupIntegrationWorktree(t, repoPath, repo.BaselineBranch)
	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.CleanupIssueWorktree(context.Background(), repoPath, worktree.WorktreePath, worktree.IssueBranch))
	})

	writeFile(t, filepath.Join(worktree.WorktreePath, "shared.txt"), "issue branch change\n")
	_, err = runGit(context.Background(), worktree.WorktreePath, "add", "shared.txt")
	require.NoError(t, err)
	_, err = runGit(context.Background(), worktree.WorktreePath, "commit", "-m", "fix(issue-77): issue side")
	require.NoError(t, err)

	_, err = runGit(context.Background(), repoPath, "checkout", repo.BaselineBranch)
	require.NoError(t, err)
	writeFile(t, filepath.Join(repoPath, "shared.txt"), "baseline change\n")
	_, err = runGit(context.Background(), repoPath, "add", "shared.txt")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "commit", "-m", "chore: baseline side")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "push", "origin", repo.BaselineBranch)
	require.NoError(t, err)

	_, err = manager.MergeIssueBranch(context.Background(), repoPath, repo.BaselineBranch, worktree.IssueBranch, "no-ff")
	require.Error(t, err)

	var conflictErr *MergeConflictError
	require.ErrorAs(t, err, &conflictErr)
	require.Contains(t, conflictErr.Files, "shared.txt")

	status, statusErr := runGit(context.Background(), integrationWorktreePath(repoPath, repo.BaselineBranch), "status", "--short")
	require.NoError(t, statusErr)
	require.Empty(t, status)
}

func TestRevertMergeCommitRestoresBaseline(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{Name: "ml-svc-zcode", Path: repoPath, IssueBranchPrefix: "cnb", BaselineBranch: "ai-v2.0"}
	cleanupIntegrationWorktree(t, repoPath, repo.BaselineBranch)
	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.CleanupIssueWorktree(context.Background(), repoPath, worktree.WorktreePath, worktree.IssueBranch))
	})

	writeFile(t, filepath.Join(worktree.WorktreePath, "feature.txt"), "issue 77\n")
	_, err = runGit(context.Background(), worktree.WorktreePath, "add", "feature.txt")
	require.NoError(t, err)
	_, err = runGit(context.Background(), worktree.WorktreePath, "commit", "-m", "fix(issue-77): add feature")
	require.NoError(t, err)

	mergeResult, err := manager.MergeIssueBranch(context.Background(), repoPath, repo.BaselineBranch, worktree.IssueBranch, "no-ff")
	require.NoError(t, err)

	revertCommit, err := manager.RevertMergeCommit(context.Background(), repoPath, repo.BaselineBranch, mergeResult.MergeCommit)
	require.NoError(t, err)
	require.NotEmpty(t, revertCommit)

	_, err = os.Stat(filepath.Join(integrationWorktreePath(repoPath, repo.BaselineBranch), "feature.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPushBaselineBranchUpdatesOrigin(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{Name: "ml-svc-zcode", Path: repoPath, IssueBranchPrefix: "cnb", BaselineBranch: "ai-v2.0"}
	cleanupIntegrationWorktree(t, repoPath, repo.BaselineBranch)
	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.CleanupIssueWorktree(context.Background(), repoPath, worktree.WorktreePath, worktree.IssueBranch))
	})

	writeFile(t, filepath.Join(worktree.WorktreePath, "feature.txt"), "issue 77\n")
	_, err = runGit(context.Background(), worktree.WorktreePath, "add", "feature.txt")
	require.NoError(t, err)
	_, err = runGit(context.Background(), worktree.WorktreePath, "commit", "-m", "fix(issue-77): add feature")
	require.NoError(t, err)

	mergeResult, err := manager.MergeIssueBranch(context.Background(), repoPath, repo.BaselineBranch, worktree.IssueBranch, "no-ff")
	require.NoError(t, err)

	pushedSHA, err := manager.PushBaselineBranch(context.Background(), repoPath, repo.BaselineBranch)
	require.NoError(t, err)
	require.Equal(t, mergeResult.MergeCommit, pushedSHA)

	originSHA, err := runGit(context.Background(), repoPath, "ls-remote", "origin", "refs/heads/"+repo.BaselineBranch)
	require.NoError(t, err)
	require.Contains(t, originSHA, pushedSHA)
}

func TestPushBaselineBranchRefusesTemporaryIssueBranch(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()

	_, err := manager.PushBaselineBranch(context.Background(), repoPath, "cnb/issue-77/ml-svc-zcode")
	require.ErrorContains(t, err, "refusing to push temporary issue branch")

	originSHA, remoteErr := runGit(context.Background(), repoPath, "ls-remote", "origin", "refs/heads/cnb/issue-77/ml-svc-zcode")
	require.NoError(t, remoteErr)
	require.Empty(t, originSHA)
}

func TestCleanupIssueWorktreeRemovesWorktreeAndBranch(t *testing.T) {
	repoPath := createTestRepo(t)
	manager := New()
	repo := config.RepositoryConfig{Name: "ml-svc-zcode", Path: repoPath, IssueBranchPrefix: "cnb", BaselineBranch: "ai-v2.0"}
	worktree, err := manager.PrepareIssueWorktree(context.Background(), t.TempDir(), "77", repo)
	require.NoError(t, err)

	err = manager.CleanupIssueWorktree(context.Background(), repoPath, worktree.WorktreePath, worktree.IssueBranch)
	require.NoError(t, err)
	require.NoDirExists(t, worktree.WorktreePath)

	branches, err := runGit(context.Background(), repoPath, "branch", "--list", worktree.IssueBranch)
	require.NoError(t, err)
	require.Empty(t, branches)
}

func createTestRepo(t *testing.T) string {
	return createTestRepoWithBaseline(t, "ai-v2.0")
}

func createTestRepoWithBaseline(t *testing.T, baselineBranch string) string {
	t.Helper()

	root := t.TempDir()
	originPath := filepath.Join(root, "origin.git")
	repoPath := filepath.Join(root, "repo")

	_, err := runGit(context.Background(), root, "init", "--bare", originPath)
	require.NoError(t, err)
	_, err = runGit(context.Background(), root, "clone", originPath, repoPath)
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "config", "user.name", "Issue Agent Runner")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "config", "user.email", "runner@example.com")
	require.NoError(t, err)

	writeFile(t, filepath.Join(repoPath, "README.md"), "hello\n")
	_, err = runGit(context.Background(), repoPath, "add", "README.md")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "commit", "-m", "chore: initial commit")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "branch", "-M", baselineBranch)
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "push", "-u", "origin", baselineBranch)
	require.NoError(t, err)

	return repoPath
}

func createDirectTestRepo(t *testing.T) string {
	t.Helper()

	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	_, err := runGit(context.Background(), repoPath, "init")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "config", "user.name", "Issue Agent Runner")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "config", "user.email", "runner@example.com")
	require.NoError(t, err)

	writeFile(t, filepath.Join(repoPath, "README.md"), "hello\n")
	_, err = runGit(context.Background(), repoPath, "add", "README.md")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "commit", "-m", "chore: initial commit")
	require.NoError(t, err)
	_, err = runGit(context.Background(), repoPath, "branch", "-M", "ai-v2.0")
	require.NoError(t, err)

	return repoPath
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func cleanupIntegrationWorktree(t *testing.T, repoPath string, baselineBranch string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = runGit(context.Background(), repoPath, "worktree", "remove", "--force", integrationWorktreePath(repoPath, baselineBranch))
		_, _ = runGit(context.Background(), repoPath, "branch", "-D", integrationBranchName(baselineBranch))
	})
}
