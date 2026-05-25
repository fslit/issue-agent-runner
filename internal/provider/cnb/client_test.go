package cnb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/domain"
)

func TestParseRemoteExtractsTokenAndRepo(t *testing.T) {
	token, repo, err := ParseRemote("https://cnb:secret-token@cnb.cool/dianyuniot/mlink/ml-svc-zcode.git")
	require.NoError(t, err)
	require.Equal(t, "secret-token", token)
	require.Equal(t, "dianyuniot/mlink/ml-svc-zcode", repo)
}

func TestListOpenPaginatesAndValidatesIssues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		require.Equal(t, "open", r.URL.Query().Get("state"))
		switch r.URL.Query().Get("page") {
		case "1":
			writeJSON(t, w, []map[string]any{{
				"number":   7,
				"title":    "fix issue",
				"body":     "body",
				"state":    "open",
				"labels":   []map[string]any{{"name": "前端"}},
				"html_url": "https://cnb.cool/repo/-/issues/7",
			}})
		case "2":
			writeJSON(t, w, []map[string]any{})
		default:
			t.Fatalf("unexpected page %s", r.URL.Query().Get("page"))
		}
	}))
	defer server.Close()

	client := New(ClientConfig{BaseURL: server.URL, Token: "token", Repo: "owner/repo", HTTPClient: server.Client()})
	issues, err := client.ListOpen(context.Background())
	require.NoError(t, err)

	require.Equal(t, []domain.Issue{{
		Number: "7",
		Title:  "fix issue",
		Body:   "body",
		State:  "open",
		Labels: []string{"前端"},
		URL:    "https://cnb.cool/repo/-/issues/7",
	}}, issues)
}

func TestAddLabelsMergesWithoutOverwritingExistingLabels(t *testing.T) {
	var putLabels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]any{
				"number": 7,
				"title":  "fix issue",
				"state":  "open",
				"labels": []map[string]any{{"name": "bug"}, {"name": "处理中"}},
			})
		case http.MethodPut:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&struct {
				Labels *[]string `json:"labels"`
			}{Labels: &putLabels}))
			writeJSON(t, w, map[string]any{})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	client := New(ClientConfig{BaseURL: server.URL, Token: "token", Repo: "owner/repo", HTTPClient: server.Client()})
	require.NoError(t, client.AddLabels(context.Background(), "7", []string{"已处理", "bug"}))

	require.ElementsMatch(t, []string{"bug", "处理中", "已处理"}, putLabels)
}

func TestProviderErrorClassifiesAuthAndMalformedJSON(t *testing.T) {
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer authServer.Close()

	client := New(ClientConfig{BaseURL: authServer.URL, Token: "token", Repo: "owner/repo", HTTPClient: authServer.Client()})
	_, err := client.ListOpen(context.Background())
	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, http.StatusUnauthorized, providerErr.StatusCode)
	require.Equal(t, domain.ErrorAuth, providerErr.Kind)

	badJSONServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"not":"a list"}`))
	}))
	defer badJSONServer.Close()

	client = New(ClientConfig{BaseURL: badJSONServer.URL, Token: "token", Repo: "owner/repo", HTTPClient: badJSONServer.Client()})
	_, err = client.ListOpen(context.Background())
	require.ErrorContains(t, err, "expected issue list")
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(value))
}
