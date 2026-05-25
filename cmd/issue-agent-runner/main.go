package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"issue-agent-runner/internal/config"
	codexexecutor "issue-agent-runner/internal/executor/codex"
	"issue-agent-runner/internal/gitops"
	cnbprovider "issue-agent-runner/internal/provider/cnb"
	"issue-agent-runner/internal/runner"
	"issue-agent-runner/internal/state"
)

type cliOptions struct {
	configPath string
	dryRun     bool
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	opts := &cliOptions{}
	root := &cobra.Command{
		Use:   "issue-agent-runner",
		Short: "Poll issues and dispatch bounded AI workers",
	}
	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", "configs/runner.example.yaml", "config file")
	root.PersistentFlags().BoolVar(&opts.dryRun, "dry-run", false, "load config without remote or worker side effects")

	for _, command := range []*cobra.Command{
		newRunCommand(opts),
		newOnceCommand(opts),
		newStatusCommand(opts),
		newSummaryCommand(opts),
		newCurrentCommand(opts),
		newPauseCommand(opts),
		newResumeCommand(opts),
		newRejectCommand(opts),
		newResetAttemptsCommand(opts),
		newStopWorkerCommand(opts),
	} {
		root.AddCommand(command)
	}
	return root
}

func loadConfig(opts *cliOptions) (*config.Config, error) {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func newRunCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the polling loop",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, cfg, err := buildRunner(opts)
			if err != nil {
				return err
			}
			daemonLock, err := acquireDaemonLock(cfg.Paths.StateDir)
			if err != nil {
				return err
			}
			defer daemonLock.Unlock()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			trigger := make(chan string, 1)
			if cfg.Callback.Enabled {
				server, err := startCallbackServer(ctx, cfg.Callback, trigger, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				defer server.Shutdown(context.Background())
			}

			ticker := time.NewTicker(time.Duration(cfg.Runner.IntervalSeconds) * time.Second)
			defer ticker.Stop()
			for {
				if err := r.Once(ctx); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "poll failed: %v\n", err)
				}
				select {
				case <-ctx.Done():
					return nil
				case reason := <-trigger:
					fmt.Fprintf(cmd.ErrOrStderr(), "callback trigger received: %s\n", reason)
				case <-ticker.C:
				}
			}
		},
	}
}

func acquireDaemonLock(stateDir string) (*flock.Flock, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	lock := flock.New(filepath.Join(stateDir, "runner.daemon.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock runner daemon: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("runner daemon already running for state dir %s", stateDir)
	}
	return lock, nil
}

func startCallbackServer(ctx context.Context, cfg config.CallbackConfig, trigger chan<- string, stderr io.Writer) (*http.Server, error) {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen callback server: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.Path, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !callbackAuthorized(cfg, req) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, req.Body, 1<<20))
		_ = req.Body.Close()

		reason := req.Header.Get("X-CNB-Event")
		if reason == "" {
			reason = "callback"
		}
		select {
		case trigger <- reason:
		default:
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}` + "\n"))
	})

	server := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		fmt.Fprintf(stderr, "callback server listening on http://%s%s\n", cfg.Listen, cfg.Path)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(stderr, "callback server failed: %v\n", err)
		}
	}()
	return server, nil
}

func callbackAuthorized(cfg config.CallbackConfig, req *http.Request) bool {
	if cfg.Secret == "" {
		return true
	}
	got := req.Header.Get(cfg.SecretHeader)
	return subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Secret)) == 1
}

func newOnceCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "once",
		Short: "Run one polling pass",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, cfg, err := buildRunner(opts)
			if err != nil {
				return err
			}
			if err := r.Once(cmd.Context()); err != nil {
				return err
			}
			mode := "once"
			if opts.dryRun {
				mode = "dry run"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s complete for %s\n", mode, cfg.Project.Name)
			return nil
		},
	}
}

func newStatusCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print runner status",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := buildRunner(opts)
			if err != nil {
				return err
			}
			summary, err := r.Status(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Open issues: %d\nActive workers: %d\nRetry backoff issues: %d\nPaused issues: %d\n",
				summary.OpenIssues,
				len(summary.ActiveWorkers),
				len(summary.WaitingRetry),
				len(summary.PausedIssues),
			)
			return nil
		},
	}
}

func newSummaryCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "summary",
		Short: "Print progress summary",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := buildRunner(opts)
			if err != nil {
				return err
			}
			markdown, err := r.SummaryMarkdown(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), markdown)
			return nil
		},
	}
}

func newCurrentCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Print the issue currently awaiting manual validation",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := buildRunner(opts)
			if err != nil {
				return err
			}
			markdown, err := r.CurrentValidationMarkdown(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), markdown)
			return nil
		},
	}
}

func newPauseCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "pause ISSUE...",
		Short: "Pause one or more issues",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := buildRunner(opts)
			if err != nil {
				return err
			}
			return r.Pause(cmd.Context(), args, "paused by operator")
		},
	}
}

func newResumeCommand(opts *cliOptions) *cobra.Command {
	var clearRemoteRunningLabel bool
	command := &cobra.Command{
		Use:   "resume ISSUE...",
		Short: "Resume one or more issues",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := buildRunner(opts)
			if err != nil {
				return err
			}
			if err := r.Resume(cmd.Context(), args); err != nil {
				return err
			}
			if clearRemoteRunningLabel {
				return r.ClearRemoteRunningLabels(cmd.Context(), args)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&clearRemoteRunningLabel, "clear-remote-running-label", false, "remove remote running label during resume")
	return command
}

func newResetAttemptsCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "reset-attempts ISSUE...",
		Short: "Reset retry attempts",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := buildRunner(opts)
			if err != nil {
				return err
			}
			return r.ResetAttempts(cmd.Context(), args)
		},
	}
}

func newRejectCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "reject ISSUE",
		Short: "Reject a manually verified issue and revert its baseline merge commit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := buildRunner(opts)
			if err != nil {
				return err
			}
			return r.Reject(cmd.Context(), args[0])
		},
	}
}

func newStopWorkerCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "stop-worker ISSUE",
		Short: "Stop an active worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := buildRunner(opts)
			if err != nil {
				return err
			}
			return r.StopWorker(cmd.Context(), args[0])
		},
	}
}

func buildRunner(opts *cliOptions) (*runner.Runner, *config.Config, error) {
	cfg, err := loadConfig(opts)
	if err != nil {
		return nil, nil, err
	}
	token, repo, err := resolveCNBCredentials(context.Background(), cfg)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Provider.Repo == "" {
		cfg.Provider.Repo = repo
	}

	provider := cnbprovider.New(cnbprovider.ClientConfig{
		Token: token,
		Repo:  cfg.Provider.Repo,
	})
	exec := codexexecutor.New(codexexecutor.Config{
		Bin:              cfg.Executor.Bin,
		Model:            cfg.Executor.Model,
		Workspace:        cfg.Project.Workspace,
		Sandbox:          cfg.Executor.Sandbox,
		Approval:         cfg.Executor.Approval,
		ReasoningEffort:  cfg.Executor.ReasoningEffort,
		SkipGitRepoCheck: cfg.Executor.SkipGitRepoCheck,
	})
	return runner.New(runner.Dependencies{
		Config:   cfg,
		Store:    state.New(cfg.Paths.StateDir),
		Provider: provider,
		Executor: exec,
		GitOps:   gitops.New(),
		DryRun:   opts.dryRun,
	}), cfg, nil
}

func resolveCNBCredentials(ctx context.Context, cfg *config.Config) (string, string, error) {
	if cfg.Provider.Token.Value != "" {
		return cfg.Provider.Token.Value, cfg.Provider.Repo, nil
	}
	if cfg.Provider.Token.From != "gitRemote" && cfg.Provider.Token.From != "" {
		return "", "", fmt.Errorf("unsupported provider.token.from %q", cfg.Provider.Token.From)
	}
	repoPath := cfg.Project.ServiceRepo
	if repoPath == "" {
		repoPath = "."
	}
	remoteURL, err := gitRemoteURL(ctx, cfg.Project.Workspace, repoPath)
	if err != nil {
		return "", "", err
	}
	return cnbprovider.ParseRemote(remoteURL)
}

func gitRemoteURL(ctx context.Context, workspace string, serviceRepo string) (string, error) {
	repoDir := serviceRepo
	if !strings.HasPrefix(repoDir, "/") {
		repoDir = workspace + string(os.PathSeparator) + serviceRepo
	}
	command := exec.CommandContext(ctx, "git", "-C", repoDir, "config", "--get", "remote.origin.url")
	raw, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read git remote: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}
