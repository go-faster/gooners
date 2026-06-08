package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientLocationAndAuthHeaders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/agent", r.URL.Path)
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		require.Equal(t, "opencode", user)
		require.Equal(t, "secret", pass)
		require.Equal(t, dir, r.Header.Get("x-opencode-directory"))
		require.Equal(t, "ws", r.Header.Get("x-opencode-workspace"))
		_, _ = w.Write([]byte(`{"data":{"build":{"description":"Build things","mode":"subagent"}}}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL, Username: "opencode", Password: "secret"})
	agents, err := client.Agents(t.Context(), Location{Directory: dir, Workspace: "ws"})
	require.NoError(t, err)
	require.Len(t, agents, 1)
	require.Equal(t, "build", agents[0].Name)
	require.Equal(t, "subagent", agents[0].Mode)
}

func TestClientCreateSessionAndPrompt(t *testing.T) {
	t.Parallel()
	var sawCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			require.Equal(t, http.MethodPost, r.Method)
			sawCreate = true
			var body CreateSessionRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "ws", r.Header.Get("x-opencode-workspace"))
			require.Equal(t, "Fix tests", body.Title)
			require.Equal(t, "build", body.Agent)
			_, _ = w.Write([]byte(`{"data":{"id":"ses_1","title":"Fix tests","time":{"created":1,"updated":2}}}`))
		case "/api/session/ses_1/prompt":
			require.True(t, sawCreate, "prompt called before create")
			var body PromptRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "do it", body.Prompt.Text)
			require.Equal(t, "build", body.Agent)
			require.NotNil(t, body.Model)
			require.Equal(t, "anthropic", body.Model.ProviderID)
			_, _ = w.Write([]byte(`{"data":{"messageID":"msg_1"}}`))
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	session, err := client.CreateSession(t.Context(), Location{Workspace: "ws"}, CreateSessionRequest{Title: "Fix tests", Agent: "build"})
	require.NoError(t, err)
	require.Equal(t, "ses_1", session.ID)
	require.EqualValues(t, 1, session.CreatedAt)
	require.EqualValues(t, 2, session.UpdatedAt)
	_, err = client.Prompt(t.Context(), Location{}, session.ID, PromptRequest{
		Prompt: PromptPayload{Text: "do it"},
		Agent:  "build",
		Model:  &ModelRef{ProviderID: "anthropic", ModelID: "claude"},
	})
	require.NoError(t, err)
}

func TestClientCreateSessionMissingRouteError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, err := client.CreateSession(t.Context(), Location{}, CreateSessionRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "POST /api/session")
}

func TestClientSessionsPagination(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "10", r.URL.Query().Get("limit"))
		require.Equal(t, "next", r.URL.Query().Get("cursor"))
		_, _ = w.Write([]byte(`{"data":[{"id":"ses_1","title":"one"}]}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	res, err := client.Sessions(t.Context(), SessionsRequest{Limit: 10, Cursor: "next"})
	require.NoError(t, err)
	require.Len(t, res.Sessions, 1)
	require.Equal(t, "ses_1", res.Sessions[0].ID)
}

func TestClientQuestionsUsesSessionRoute(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/session/ses_1/question", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, err := client.Questions(t.Context(), Location{}, "ses_1")
	require.NoError(t, err)
}

func TestClientInvalidDirectoryHeader(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be called")
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, err := client.Agents(t.Context(), Location{Directory: "bad\nvalue"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid header")
}

func TestFirstText(t *testing.T) {
	t.Parallel()
	got := firstText(json.RawMessage(`{"data":[{"role":"assistant","content":[{"type":"text","text":"first"},{"text":"second"}]}]}`))
	require.Equal(t, "first\nsecond", got)
}

func TestRawFieldsDoNotSerialize(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(AgentsResult{Agents: []Agent{{Name: "build", Raw: json.RawMessage(`{"large":true}`)}}})
	require.NoError(t, err)
	require.NotContains(t, string(data), "large")
	require.NotContains(t, string(data), "raw")
}

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	client, err := NewClient(cfg, time.Second)
	require.NoError(t, err)
	return client
}

func TestClientTimeout(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-done
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(done) })
	client := newTestClient(t, Config{BaseURL: server.URL})
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err := client.Health(ctx)
	require.Error(t, err)
}
