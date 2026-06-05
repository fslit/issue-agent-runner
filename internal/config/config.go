package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Project      ProjectConfig      `yaml:"project"`
	Repositories []RepositoryConfig `yaml:"repositories"`
	Provider     ProviderConfig     `yaml:"provider"`
	Executor     ExecutorConfig     `yaml:"executor"`
	Runner       RunnerConfig       `yaml:"runner"`
	Labels       LabelConfig        `yaml:"labels"`
	Paths        PathConfig         `yaml:"paths"`
	Browser      BrowserConfig      `yaml:"browser"`
	Hooks        HookConfig         `yaml:"hooks"`
	Callback     CallbackConfig     `yaml:"callback"`
	Integration  IntegrationConfig  `yaml:"integration"`
}

type ProjectConfig struct {
	Name        string `yaml:"name"`
	Workspace   string `yaml:"workspace"`
	ServiceRepo string `yaml:"serviceRepo"`
}

type RepositoryConfig struct {
	Name              string `yaml:"name"`
	Path              string `yaml:"path"`
	IssueBranchPrefix string `yaml:"issueBranchPrefix"`
	BaselineBranch    string `yaml:"baselineBranch"`
	DirectBaseline    bool   `yaml:"directBaseline"`
}

type ProviderConfig struct {
	Type  string      `yaml:"type"`
	Repo  string      `yaml:"repo"`
	Token TokenConfig `yaml:"token"`
}

type TokenConfig struct {
	From  string `yaml:"from"`
	Value string `yaml:"value"`
}

type ExecutorConfig struct {
	Type             string `yaml:"type"`
	Bin              string `yaml:"bin"`
	Model            string `yaml:"model"`
	Sandbox          string `yaml:"sandbox"`
	Approval         string `yaml:"approval"`
	ReasoningEffort  string `yaml:"reasoningEffort"`
	SkipGitRepoCheck bool   `yaml:"skipGitRepoCheck"`
}

type RunnerConfig struct {
	IntervalSeconds        int `yaml:"intervalSeconds"`
	MaxConcurrency         int `yaml:"maxConcurrency"`
	MaxAttempts            int `yaml:"maxAttempts"`
	RetryBackoffSeconds    int `yaml:"retryBackoffSeconds"`
	CapacityBackoffSeconds int `yaml:"capacityBackoffSeconds"`
	QuotaBackoffSeconds    int `yaml:"quotaBackoffSeconds"`
	StaleWorkerSeconds     int `yaml:"staleWorkerSeconds"`
	HardWorkerSeconds      int `yaml:"hardWorkerSeconds"`
}

type LabelConfig struct {
	Running    string   `yaml:"running"`
	Done       string   `yaml:"done"`
	Failed     string   `yaml:"failed"`
	NeedsHuman string   `yaml:"needsHuman"`
	Skip       []string `yaml:"skip"`
}

type PathConfig struct {
	StateDir       string `yaml:"stateDir"`
	PromptTemplate string `yaml:"promptTemplate"`
	RunnerLog      string `yaml:"runnerLog"`
}

type BrowserConfig struct {
	Enabled         bool     `yaml:"enabled"`
	Mode            string   `yaml:"mode"`
	BaseURL         string   `yaml:"baseURL"`
	StartCommand    string   `yaml:"startCommand"`
	HealthCheckURL  string   `yaml:"healthCheckURL"`
	TimeoutSeconds  int      `yaml:"timeoutSeconds"`
	ArtifactDir     string   `yaml:"artifactDir"`
	EnableForLabels []string `yaml:"enableForLabels"`
}

type HookConfig struct {
	OnIssueClaim        HookEndpoint `yaml:"onIssueClaim"`
	OnIssuePaused       HookEndpoint `yaml:"onIssuePaused"`
	OnProgressUpdate    HookEndpoint `yaml:"onProgressUpdate"`
	OnIssueIntegrated   HookEndpoint `yaml:"onIssueIntegrated"`
	OnIssueRejected     HookEndpoint `yaml:"onIssueRejected"`
	OnIssueClosedPushed HookEndpoint `yaml:"onIssueClosedPushed"`
}

type IntegrationConfig struct {
	WorktreeRoot             string `yaml:"worktreeRoot"`
	MergeMode                string `yaml:"mergeMode"`
	PushOnClose              bool   `yaml:"pushOnClose"`
	CleanupOnClose           bool   `yaml:"cleanupOnClose"`
	AllowSinglePendingVerify bool   `yaml:"allowSinglePendingVerify"`
}

type HookEndpoint struct {
	Command        string        `yaml:"command"`
	TimeoutSeconds int           `yaml:"timeoutSeconds"`
	Webhook        WebhookConfig `yaml:"webhook"`
}

type WebhookConfig struct {
	URL    string `yaml:"url"`
	Method string `yaml:"method"`
}

type CallbackConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Listen       string `yaml:"listen"`
	Path         string `yaml:"path"`
	Secret       string `yaml:"secret"`
	SecretHeader string `yaml:"secretHeader"`
}

func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("config path is required")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.normalize(path); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func defaultConfig() Config {
	return Config{
		Executor: ExecutorConfig{
			Type:            "codex",
			Bin:             "codex",
			Model:           "gpt-5.5",
			Sandbox:         "danger-full-access",
			Approval:        "never",
			ReasoningEffort: "xhigh",
		},
		Runner: RunnerConfig{
			IntervalSeconds:        60,
			MaxConcurrency:         5,
			MaxAttempts:            3,
			RetryBackoffSeconds:    300,
			CapacityBackoffSeconds: 900,
			QuotaBackoffSeconds:    3600,
			StaleWorkerSeconds:     7200,
			HardWorkerSeconds:      14400,
		},
		Labels: LabelConfig{
			Running:    "处理中",
			Done:       "已处理",
			Failed:     "处理失败",
			NeedsHuman: "需人工确认",
			Skip:       []string{"已处理", "处理中", "处理失败", "需人工确认"},
		},
		Paths: PathConfig{
			StateDir:       ".local_cache/cnb-issue-runner",
			PromptTemplate: "templates/worker_prompt.md",
		},
		Browser: BrowserConfig{
			Mode:           "playwright",
			TimeoutSeconds: 180,
		},
		Callback: CallbackConfig{
			Listen:       "127.0.0.1:8791",
			Path:         "/callbacks/cnb",
			SecretHeader: "X-Issue-Agent-Token",
		},
		Integration: IntegrationConfig{
			WorktreeRoot:             ".local_worktrees/issue-agent-runner",
			MergeMode:                "no-ff",
			PushOnClose:              true,
			CleanupOnClose:           true,
			AllowSinglePendingVerify: true,
		},
	}
}

func (c *Config) normalize(configPath string) error {
	if c.Project.Workspace != "" {
		workspace, err := filepath.Abs(c.Project.Workspace)
		if err != nil {
			return fmt.Errorf("resolve project.workspace: %w", err)
		}
		c.Project.Workspace = workspace
	}

	if c.Paths.StateDir != "" && !filepath.IsAbs(c.Paths.StateDir) && c.Project.Workspace != "" {
		c.Paths.StateDir = filepath.Join(c.Project.Workspace, c.Paths.StateDir)
	}
	if c.Paths.RunnerLog == "" && c.Paths.StateDir != "" {
		c.Paths.RunnerLog = filepath.Join(c.Paths.StateDir, "runner.log")
	} else if c.Paths.RunnerLog != "" && !filepath.IsAbs(c.Paths.RunnerLog) && c.Project.Workspace != "" {
		c.Paths.RunnerLog = filepath.Join(c.Project.Workspace, c.Paths.RunnerLog)
	}
	if c.Paths.PromptTemplate != "" && !filepath.IsAbs(c.Paths.PromptTemplate) {
		c.Paths.PromptTemplate = filepath.Join(filepath.Dir(configPath), c.Paths.PromptTemplate)
	}
	if c.Browser.ArtifactDir != "" && !filepath.IsAbs(c.Browser.ArtifactDir) && c.Project.Workspace != "" {
		c.Browser.ArtifactDir = filepath.Join(c.Project.Workspace, c.Browser.ArtifactDir)
	}
	if c.Callback.Path == "" {
		c.Callback.Path = "/callbacks/cnb"
	}
	if c.Callback.Listen == "" {
		c.Callback.Listen = "127.0.0.1:8791"
	}
	if c.Callback.SecretHeader == "" {
		c.Callback.SecretHeader = "X-Issue-Agent-Token"
	}
	if c.Integration.WorktreeRoot != "" && !filepath.IsAbs(c.Integration.WorktreeRoot) && c.Project.Workspace != "" {
		c.Integration.WorktreeRoot = filepath.Join(c.Project.Workspace, c.Integration.WorktreeRoot)
	}
	for i := range c.Repositories {
		if c.Repositories[i].IssueBranchPrefix == "" {
			c.Repositories[i].IssueBranchPrefix = "cnb"
		}
		if c.Repositories[i].Path != "" && !filepath.IsAbs(c.Repositories[i].Path) && c.Project.Workspace != "" {
			c.Repositories[i].Path = filepath.Join(c.Project.Workspace, c.Repositories[i].Path)
		}
	}
	return nil
}

func (c Config) Validate() error {
	if c.Project.Workspace == "" {
		return errors.New("project.workspace is required")
	}
	if c.Provider.Type != "cnb" {
		return fmt.Errorf("unsupported provider.type %q", c.Provider.Type)
	}
	if c.Executor.Type != "codex" {
		return fmt.Errorf("unsupported executor.type %q", c.Executor.Type)
	}
	if c.Runner.MaxConcurrency < 1 {
		return errors.New("runner.maxConcurrency must be at least 1")
	}
	if c.Runner.MaxAttempts < 1 {
		return errors.New("runner.maxAttempts must be at least 1")
	}
	if c.Runner.IntervalSeconds < 1 {
		return errors.New("runner.intervalSeconds must be at least 1")
	}
	if c.Paths.StateDir == "" {
		return errors.New("paths.stateDir is required")
	}
	if c.Paths.PromptTemplate == "" {
		return errors.New("paths.promptTemplate is required")
	}
	if c.Callback.Enabled {
		if c.Callback.Listen == "" {
			return errors.New("callback.listen is required when callback.enabled is true")
		}
		if c.Callback.Path == "" || c.Callback.Path[0] != '/' {
			return errors.New("callback.path must start with /")
		}
		if c.Callback.Secret != "" && c.Callback.SecretHeader == "" {
			return errors.New("callback.secretHeader is required when callback.secret is set")
		}
	}
	for _, repo := range c.Repositories {
		if repo.Name == "" {
			return errors.New("repositories.name is required")
		}
		if repo.Path == "" {
			return fmt.Errorf("repositories[%s].path is required", repo.Name)
		}
		if repo.BaselineBranch == "" {
			return fmt.Errorf("repositories[%s].baselineBranch is required", repo.Name)
		}
	}
	return nil
}
