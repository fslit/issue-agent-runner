package cnb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"issue-agent-runner/internal/domain"
)

const defaultBaseURL = "https://api.cnb.cool"

type ClientConfig struct {
	BaseURL    string
	Token      string
	Repo       string
	HTTPClient *http.Client
}

type Client struct {
	baseURL    string
	token      string
	repo       string
	httpClient *http.Client
}

type ProviderError struct {
	StatusCode int
	Body       string
	Kind       domain.ErrorKind
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("CNB request failed: %d %s", e.StatusCode, e.Body)
}

func New(cfg ClientConfig) *Client {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      cfg.Token,
		repo:       strings.Trim(cfg.Repo, "/"),
		httpClient: httpClient,
	}
}

func ParseRemote(remoteURL string) (string, string, error) {
	const prefix = "https://cnb:"
	const hostMarker = "@cnb.cool/"
	if !strings.HasPrefix(remoteURL, prefix) || !strings.Contains(remoteURL, hostMarker) {
		return "", "", errors.New("remote.origin.url must be https://cnb:<token>@cnb.cool/<owner>/<repo>.git")
	}
	rest := strings.TrimPrefix(remoteURL, prefix)
	parts := strings.SplitN(rest, hostMarker, 2)
	token := parts[0]
	repo := strings.TrimSuffix(strings.Trim(parts[1], "/"), ".git")
	if token == "" || repo == "" {
		return "", "", errors.New("CNB token or repository path is empty")
	}
	return token, repo, nil
}

func (c *Client) ListOpen(ctx context.Context) ([]domain.Issue, error) {
	var issues []domain.Issue
	for page := 1; ; page++ {
		var raw any
		path := fmt.Sprintf("/%s/-/issues?page=%d&page_size=50&state=open", c.repo, page)
		if err := c.request(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}
		pageIssues, err := parseIssueList(raw)
		if err != nil {
			return nil, err
		}
		if len(pageIssues) == 0 {
			break
		}
		issues = append(issues, pageIssues...)
	}
	return issues, nil
}

func (c *Client) Get(ctx context.Context, number string) (domain.Issue, error) {
	var raw any
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/%s/-/issues/%s", c.repo, url.PathEscape(number)), nil, &raw); err != nil {
		return domain.Issue{}, err
	}
	return parseIssue(raw)
}

func (c *Client) AddLabels(ctx context.Context, number string, labels []string) error {
	issue, err := c.Get(ctx, number)
	if err != nil {
		return err
	}
	merged := mergeLabels(issue.Labels, labels)
	return c.putLabels(ctx, number, merged)
}

func (c *Client) RemoveLabels(ctx context.Context, number string, labels []string) error {
	issue, err := c.Get(ctx, number)
	if err != nil {
		return err
	}
	remove := labelSet(labels)
	kept := []string{}
	for _, label := range issue.Labels {
		if !remove[label] {
			kept = append(kept, label)
		}
	}
	return c.putLabels(ctx, number, kept)
}

func (c *Client) AddComment(ctx context.Context, number string, body string) error {
	return c.request(ctx, http.MethodPost, fmt.Sprintf("/%s/-/issues/%s/comments", c.repo, url.PathEscape(number)), map[string]string{"body": body}, nil)
}

func (c *Client) putLabels(ctx context.Context, number string, labels []string) error {
	sort.Strings(labels)
	return c.request(ctx, http.MethodPut, fmt.Sprintf("/%s/-/issues/%s/labels", c.repo, url.PathEscape(number)), map[string][]string{"labels": labels}, nil)
}

func (c *Client) request(ctx context.Context, method string, path string, body any, out *any) error {
	var requestBody *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		requestBody = bytes.NewReader(raw)
	} else {
		requestBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, requestBody)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return &ProviderError{StatusCode: resp.StatusCode, Body: buf.String(), Kind: errorKindForStatus(resp.StatusCode)}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

func parseIssueList(raw any) ([]domain.Issue, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("expected issue list")
	}
	issues := make([]domain.Issue, 0, len(items))
	for _, item := range items {
		issue, err := parseIssue(item)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

func parseIssue(raw any) (domain.Issue, error) {
	item, ok := raw.(map[string]any)
	if !ok {
		return domain.Issue{}, errors.New("expected issue object")
	}
	number, err := stringField(item, "number", "id")
	if err != nil {
		return domain.Issue{}, err
	}
	title, err := optionalStringField(item, "title")
	if err != nil {
		return domain.Issue{}, err
	}
	body, err := optionalStringField(item, "body")
	if err != nil {
		return domain.Issue{}, err
	}
	state, err := optionalStringField(item, "state")
	if err != nil {
		return domain.Issue{}, err
	}
	htmlURL, err := optionalStringField(item, "html_url")
	if err != nil {
		return domain.Issue{}, err
	}
	if htmlURL == "" {
		htmlURL, err = optionalStringField(item, "url")
		if err != nil {
			return domain.Issue{}, err
		}
	}
	labels, err := parseLabels(item["labels"])
	if err != nil {
		return domain.Issue{}, err
	}
	return domain.Issue{Number: number, Title: title, Body: body, State: state, Labels: labels, URL: htmlURL}, nil
}

func stringField(item map[string]any, keys ...string) (string, error) {
	for _, key := range keys {
		value, ok := item[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			if typed != "" {
				return typed, nil
			}
		case float64:
			return strconv.FormatInt(int64(typed), 10), nil
		}
	}
	return "", fmt.Errorf("issue field %s is required", strings.Join(keys, "/"))
}

func optionalStringField(item map[string]any, key string) (string, error) {
	value, ok := item[key]
	if !ok || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("issue field %s must be a string", key)
	}
	return text, nil
}

func parseLabels(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("issue labels must be a list")
	}
	labels := []string{}
	for _, item := range items {
		switch typed := item.(type) {
		case string:
			labels = append(labels, typed)
		case map[string]any:
			name, ok := typed["name"].(string)
			if !ok {
				return nil, errors.New("issue label name must be a string")
			}
			labels = append(labels, name)
		default:
			return nil, errors.New("issue label must be a string or object")
		}
	}
	return labels, nil
}

func mergeLabels(existing []string, additions []string) []string {
	set := labelSet(existing)
	for _, label := range additions {
		if label != "" {
			set[label] = true
		}
	}
	merged := make([]string, 0, len(set))
	for label := range set {
		merged = append(merged, label)
	}
	return merged
}

func labelSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		if label != "" {
			set[label] = true
		}
	}
	return set
}

func errorKindForStatus(status int) domain.ErrorKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return domain.ErrorAuth
	case status == http.StatusTooManyRequests:
		return domain.ErrorQuota
	default:
		return domain.ErrorUnknown
	}
}
