package provider

import (
	"context"

	"issue-agent-runner/internal/domain"
)

type IssueProvider interface {
	ListOpen(ctx context.Context) ([]domain.Issue, error)
	Get(ctx context.Context, number string) (domain.Issue, error)
	AddLabels(ctx context.Context, number string, labels []string) error
	RemoveLabels(ctx context.Context, number string, labels []string) error
	AddComment(ctx context.Context, number string, body string) error
}
