package executor

import (
	"context"

	"issue-agent-runner/internal/domain"
)

type Executor interface {
	Start(ctx context.Context, task domain.Task) (domain.WorkerState, error)
	Alive(ctx context.Context, worker domain.WorkerState) bool
	Result(ctx context.Context, worker domain.WorkerState) domain.WorkerResult
	Terminate(ctx context.Context, worker domain.WorkerState) error
}
