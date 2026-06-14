package domain

import (
	"encoding/json"
	"strconv"
	"time"
)

type ErrorKind string

const (
	ErrorNone          ErrorKind = "NONE"
	ErrorModelCapacity ErrorKind = "MODEL_CAPACITY"
	ErrorQuota         ErrorKind = "QUOTA"
	ErrorAuth          ErrorKind = "AUTH"
	ErrorTestFailure   ErrorKind = "TEST_FAILURE"
	ErrorStale         ErrorKind = "STALE"
	ErrorTimeout       ErrorKind = "TIMEOUT"
	ErrorUnknown       ErrorKind = "UNKNOWN"
)

type Issue struct {
	Number string
	Title  string
	Body   string
	State  string
	Labels []string
	URL    string
}

type IssueWorkflowStatus string

const (
	IssueWorkflowStatusUnknown          IssueWorkflowStatus = ""
	IssueWorkflowStatusRunning          IssueWorkflowStatus = "RUNNING"
	IssueWorkflowStatusReadyToIntegrate IssueWorkflowStatus = "READY_TO_INTEGRATE"
	IssueWorkflowStatusAwaitingVerify   IssueWorkflowStatus = "AWAITING_VERIFY"
	IssueWorkflowStatusMergeConflict    IssueWorkflowStatus = "MERGE_CONFLICT"
	IssueWorkflowStatusVerifyRejected   IssueWorkflowStatus = "VERIFY_REJECTED"
	IssueWorkflowStatusClosedPushed     IssueWorkflowStatus = "CLOSED_PUSHED"
	IssueWorkflowStatusCleaned          IssueWorkflowStatus = "CLEANED"
)

type Task struct {
	Issue      Issue
	RepoURL    string
	Workspace  string
	PromptPath string
	LogPath    string
	ResultPath string
	Prompt     string
	Repos      []TaskRepository
}

type TaskRepository struct {
	Name           string `json:"name,omitempty"`
	Path           string `json:"path,omitempty"`
	WorktreePath   string `json:"worktreePath,omitempty"`
	IssueBranch    string `json:"issueBranch,omitempty"`
	BaselineBranch string `json:"baselineBranch,omitempty"`
}

type WorkerResult struct {
	ExitCode     *int               `json:"exitCode,omitempty"`
	Status       string             `json:"status,omitempty"`
	Summary      string             `json:"summary,omitempty"`
	Verification []string           `json:"verification,omitempty"`
	ChangedFiles []ChangedFile      `json:"changedFiles,omitempty"`
	RepoResults  []RepoWorkerResult `json:"repoResults,omitempty"`
	ErrorKind    ErrorKind          `json:"errorKind,omitempty"`
	ErrorMessage string             `json:"errorMessage,omitempty"`
}

type ChangedFile struct {
	Path        string   `json:"path,omitempty"`
	Line        int      `json:"line,omitempty"`
	Symbols     []string `json:"symbols,omitempty"`
	NewSymbols  []string `json:"newSymbols,omitempty"`
	Description string   `json:"description,omitempty"`
}

type RepoWorkerResult struct {
	Repo           string `json:"repo,omitempty"`
	RepoPath       string `json:"repoPath,omitempty"`
	WorktreePath   string `json:"worktreePath,omitempty"`
	IssueBranch    string `json:"issueBranch,omitempty"`
	BaselineBranch string `json:"baselineBranch,omitempty"`
	HeadSHA        string `json:"headSha,omitempty"`
	CommitSHA      string `json:"commitSha,omitempty"`
}

type State struct {
	Version int                    `json:"version,omitempty"`
	Workers map[string]WorkerState `json:"workers"`
	Issues  map[string]IssueState  `json:"issues,omitempty"`
}

func (s *State) Normalize() {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Workers == nil {
		s.Workers = map[string]WorkerState{}
	}
	if s.Issues == nil {
		s.Issues = map[string]IssueState{}
	}
}

type WorkerState struct {
	IssueNumber string `json:"issueNumber,omitempty"`
	PID         int    `json:"pid"`
	Title       string `json:"title,omitempty"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	Log         string `json:"log,omitempty"`
	LogPath     string `json:"logPath,omitempty"`
	PromptPath  string `json:"promptPath,omitempty"`
	ResultPath  string `json:"resultPath,omitempty"`
}

func (w WorkerState) LogPathValue() string {
	if w.LogPath != "" {
		return w.LogPath
	}
	return w.Log
}

type IssueState struct {
	Attempts             int                        `json:"attempts,omitempty"`
	Paused               bool                       `json:"paused,omitempty"`
	PauseReason          string                     `json:"pauseReason,omitempty"`
	LastExitCode         *int                       `json:"lastExitCode,omitempty"`
	LastError            string                     `json:"lastError,omitempty"`
	FailureCommentedAt   int64                      `json:"failureCommentedAt,omitempty"`
	ErrorKind            ErrorKind                  `json:"errorKind,omitempty"`
	LastStartedAt        int64                      `json:"lastStartedAt,omitempty"`
	LastFinishedAt       int64                      `json:"lastFinishedAt,omitempty"`
	NextRetryAt          int64                      `json:"nextRetryAt,omitempty"`
	Title                string                     `json:"title,omitempty"`
	WorkflowStatus       IssueWorkflowStatus        `json:"workflowStatus,omitempty"`
	AgentModel           string                     `json:"agentModel,omitempty"`
	AgentReasoningEffort string                     `json:"agentReasoningEffort,omitempty"`
	ReadyAt              int64                      `json:"readyAt,omitempty"`
	IntegratedAt         int64                      `json:"integratedAt,omitempty"`
	RejectedAt           int64                      `json:"rejectedAt,omitempty"`
	ClosedAt             int64                      `json:"closedAt,omitempty"`
	CleanedAt            int64                      `json:"cleanedAt,omitempty"`
	Repositories         map[string]IssueRepoState  `json:"repositories,omitempty"`
	Result               *WorkerResult              `json:"result,omitempty"`
	Extra                map[string]json.RawMessage `json:"-"`
}

type IssueRepoState struct {
	Repo                string   `json:"repo,omitempty"`
	RepoPath            string   `json:"repoPath,omitempty"`
	WorktreePath        string   `json:"worktreePath,omitempty"`
	IssueBranch         string   `json:"issueBranch,omitempty"`
	BaselineBranch      string   `json:"baselineBranch,omitempty"`
	IssueHeadSHA        string   `json:"issueHeadSha,omitempty"`
	CommitSHA           string   `json:"commitSha,omitempty"`
	BaselineMergeCommit string   `json:"baselineMergeCommit,omitempty"`
	PushedBaselineSHA   string   `json:"pushedBaselineSha,omitempty"`
	ConflictFiles       []string `json:"conflictFiles,omitempty"`
}

func (s *IssueState) UnmarshalJSON(data []byte) error {
	values := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}

	decode := func(key string, target any) error {
		raw, ok := values[key]
		if !ok {
			return nil
		}
		delete(values, key)
		return json.Unmarshal(raw, target)
	}

	if err := decode("attempts", &s.Attempts); err != nil {
		return err
	}
	if err := decode("paused", &s.Paused); err != nil {
		return err
	}
	if err := decode("pauseReason", &s.PauseReason); err != nil {
		return err
	}
	if err := decode("lastExitCode", &s.LastExitCode); err != nil {
		return err
	}
	if err := decode("lastError", &s.LastError); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "failureCommentedAt", &s.FailureCommentedAt); err != nil {
		return err
	}
	if err := decode("errorKind", &s.ErrorKind); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "lastStartedAt", &s.LastStartedAt); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "lastFinishedAt", &s.LastFinishedAt); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "nextRetryAt", &s.NextRetryAt); err != nil {
		return err
	}
	if err := decode("title", &s.Title); err != nil {
		return err
	}
	if err := decode("workflowStatus", &s.WorkflowStatus); err != nil {
		return err
	}
	if err := decode("agentModel", &s.AgentModel); err != nil {
		return err
	}
	if err := decode("agentReasoningEffort", &s.AgentReasoningEffort); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "readyAt", &s.ReadyAt); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "integratedAt", &s.IntegratedAt); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "rejectedAt", &s.RejectedAt); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "closedAt", &s.ClosedAt); err != nil {
		return err
	}
	if err := decodeFlexibleTime(values, "cleanedAt", &s.CleanedAt); err != nil {
		return err
	}
	if err := decode("repositories", &s.Repositories); err != nil {
		return err
	}
	if err := decode("result", &s.Result); err != nil {
		return err
	}
	s.Extra = values
	return nil
}

func (s IssueState) MarshalJSON() ([]byte, error) {
	values := map[string]any{}
	for key, raw := range s.Extra {
		values[key] = raw
	}
	if s.Attempts != 0 {
		values["attempts"] = s.Attempts
	}
	if s.Paused {
		values["paused"] = s.Paused
	}
	if s.PauseReason != "" {
		values["pauseReason"] = s.PauseReason
	}
	if s.LastExitCode != nil {
		values["lastExitCode"] = s.LastExitCode
	}
	if s.LastError != "" {
		values["lastError"] = s.LastError
	}
	if s.FailureCommentedAt != 0 {
		values["failureCommentedAt"] = s.FailureCommentedAt
	}
	if s.ErrorKind != "" {
		values["errorKind"] = s.ErrorKind
	}
	if s.LastStartedAt != 0 {
		values["lastStartedAt"] = s.LastStartedAt
	}
	if s.LastFinishedAt != 0 {
		values["lastFinishedAt"] = s.LastFinishedAt
	}
	if s.NextRetryAt != 0 {
		values["nextRetryAt"] = s.NextRetryAt
	}
	if s.Title != "" {
		values["title"] = s.Title
	}
	if s.WorkflowStatus != "" {
		values["workflowStatus"] = s.WorkflowStatus
	}
	if s.AgentModel != "" {
		values["agentModel"] = s.AgentModel
	}
	if s.AgentReasoningEffort != "" {
		values["agentReasoningEffort"] = s.AgentReasoningEffort
	}
	if s.ReadyAt != 0 {
		values["readyAt"] = s.ReadyAt
	}
	if s.IntegratedAt != 0 {
		values["integratedAt"] = s.IntegratedAt
	}
	if s.RejectedAt != 0 {
		values["rejectedAt"] = s.RejectedAt
	}
	if s.ClosedAt != 0 {
		values["closedAt"] = s.ClosedAt
	}
	if s.CleanedAt != 0 {
		values["cleanedAt"] = s.CleanedAt
	}
	if len(s.Repositories) > 0 {
		values["repositories"] = s.Repositories
	}
	if s.Result != nil {
		values["result"] = s.Result
	}
	return json.Marshal(values)
}

func (w *WorkerResult) UnmarshalJSON(data []byte) error {
	type alias WorkerResult
	var raw struct {
		alias
		ChangedFiles json.RawMessage `json:"changedFiles"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*w = WorkerResult(raw.alias)
	if len(raw.ChangedFiles) == 0 || string(raw.ChangedFiles) == "null" {
		return nil
	}

	var objects []ChangedFile
	if err := json.Unmarshal(raw.ChangedFiles, &objects); err == nil {
		w.ChangedFiles = objects
		return nil
	}

	var paths []string
	if err := json.Unmarshal(raw.ChangedFiles, &paths); err != nil {
		return err
	}
	w.ChangedFiles = make([]ChangedFile, 0, len(paths))
	for _, path := range paths {
		w.ChangedFiles = append(w.ChangedFiles, ChangedFile{Path: path})
	}
	return nil
}

func decodeFlexibleTime(values map[string]json.RawMessage, key string, target *int64) error {
	raw, ok := values[key]
	if !ok {
		return nil
	}
	delete(values, key)

	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		*target = number
		return nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
		*target = parsed
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return err
	}
	*target = parsed.Unix()
	return nil
}
