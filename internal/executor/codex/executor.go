package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"issue-agent-runner/internal/domain"
)

type Config struct {
	Bin              string
	Model            string
	Workspace        string
	Sandbox          string
	Approval         string
	ReasoningEffort  string
	SkipGitRepoCheck bool
}

type Executor struct {
	cfg              Config
	killProcessGroup func(pid int) error
}

func New(cfg Config) *Executor {
	if cfg.Bin == "" {
		cfg.Bin = "codex"
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "danger-full-access"
	}
	if cfg.Approval == "" {
		cfg.Approval = "never"
	}
	executor := &Executor{cfg: cfg}
	executor.killProcessGroup = defaultKillProcessGroup
	return executor
}

func (e *Executor) BuildArgs() []string {
	args := []string{
		"--ask-for-approval", e.cfg.Approval,
		"exec",
		"--cd", e.cfg.Workspace,
		"--sandbox", e.cfg.Sandbox,
	}
	if e.cfg.Model != "" {
		args = append(args, "--model", e.cfg.Model)
	}
	if e.cfg.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(e.cfg.ReasoningEffort))
	}
	if e.cfg.SkipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	args = append(args, "-")
	return args
}

func (e *Executor) Start(ctx context.Context, task domain.Task) (domain.WorkerState, error) {
	if err := os.MkdirAll(filepath.Dir(task.PromptPath), 0o755); err != nil {
		return domain.WorkerState{}, err
	}
	if err := os.WriteFile(task.PromptPath, []byte(task.Prompt), 0o644); err != nil {
		return domain.WorkerState{}, err
	}
	if err := os.MkdirAll(filepath.Dir(task.LogPath), 0o755); err != nil {
		return domain.WorkerState{}, err
	}
	logFile, err := os.OpenFile(task.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return domain.WorkerState{}, err
	}

	command := exec.CommandContext(ctx, e.cfg.Bin, e.BuildArgs()...)
	command.Stdin = strings.NewReader(task.Prompt)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	_, _ = fmt.Fprintf(logFile, "\n=== worker start %s ===\n", time.Now().Format("2006-01-02 15:04:05"))
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return domain.WorkerState{}, err
	}

	go func() {
		err := command.Wait()
		exitCode := command.ProcessState.ExitCode()
		if err != nil && exitCode == 0 {
			exitCode = 1
		}
		_, _ = fmt.Fprintf(logFile, "\n=== worker exit code %d %s ===\n", exitCode, time.Now().Format("2006-01-02 15:04:05"))
		_ = logFile.Close()
	}()

	return domain.WorkerState{
		IssueNumber: task.Issue.Number,
		PID:         command.Process.Pid,
		Title:       task.Issue.Title,
		StartedAt:   time.Now().Unix(),
		LogPath:     task.LogPath,
		PromptPath:  task.PromptPath,
		ResultPath:  task.ResultPath,
	}, nil
}

func (e *Executor) Alive(ctx context.Context, worker domain.WorkerState) bool {
	if worker.PID <= 0 {
		return false
	}
	if err := syscall.Kill(worker.PID, 0); err != nil {
		return false
	}
	return true
}

func (e *Executor) Result(ctx context.Context, worker domain.WorkerState) domain.WorkerResult {
	if worker.ResultPath != "" {
		raw, err := os.ReadFile(worker.ResultPath)
		if err == nil {
			var result domain.WorkerResult
			if err := json.Unmarshal(raw, &result); err == nil {
				return result
			}
		}
	}
	return resultFromLog(worker.LogPathValue())
}

func (e *Executor) Terminate(ctx context.Context, worker domain.WorkerState) error {
	if worker.PID <= 0 {
		return nil
	}
	return e.killProcessGroup(worker.PID)
}

func resultFromLog(path string) domain.WorkerResult {
	if path == "" {
		return domain.WorkerResult{ErrorMessage: "worker log path missing"}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.WorkerResult{ErrorMessage: "worker log unreadable: " + err.Error()}
	}
	text := string(raw)
	var exitCode *int
	matches := regexp.MustCompile(`=== worker exit code (\d+) `).FindAllStringSubmatch(text, -1)
	if len(matches) > 0 {
		code, err := strconv.Atoi(matches[len(matches)-1][1])
		if err == nil {
			exitCode = &code
		}
	}
	return domain.WorkerResult{ExitCode: exitCode, ErrorMessage: lastErrorLine(text)}
}

func lastErrorLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		lowered := strings.ToLower(line)
		if strings.Contains(lowered, "error:") ||
			strings.Contains(lowered, "traceback") ||
			strings.Contains(lowered, "exception") ||
			strings.Contains(lowered, "fail") {
			return line
		}
	}
	return ""
}

func defaultKillProcessGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
