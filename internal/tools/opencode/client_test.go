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

func TestClientAgentsBasicAuth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/agent", r.URL.Path)
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		require.Equal(t, "opencode", user)
		require.Equal(t, "secret", pass)
		require.Equal(t, dir, r.URL.Query().Get("directory"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"build","description":"Build things","mode":"subagent"}]`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL, Username: "opencode", Password: "secret"})
	agents, err := client.Agents(t.Context(), Location{Directory: dir})
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
		case "/session":
			require.Equal(t, http.MethodPost, r.Method)
			sawCreate = true
			var body struct {
				Title string `json:"title"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "Fix tests", body.Title)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ses_1","title":"Fix tests","time":{"created":1,"updated":2},"directory":"/tmp","projectID":"p1","version":"1"}`))
		case "/session/ses_1/message":
			require.True(t, sawCreate, "prompt called before create")
			require.Equal(t, http.MethodPost, r.Method)
			var body struct {
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
				Agent string `json:"agent,omitempty"`
				Model *struct {
					ModelID    string `json:"modelID"`
					ProviderID string `json:"providerID"`
				} `json:"model,omitempty"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Len(t, body.Parts, 1)
			require.Equal(t, "text", body.Parts[0].Type)
			require.Equal(t, "do it", body.Parts[0].Text)
			require.Equal(t, "build", body.Agent)
			require.NotNil(t, body.Model)
			require.Equal(t, "anthropic", body.Model.ProviderID)
			_, _ = w.Write([]byte(`{"messageID":"msg_1"}`))
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	session, err := client.CreateSession(t.Context(), Location{}, CreateSessionRequest{Title: "Fix tests"})
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
	require.Contains(t, err.Error(), "POST /session")
}

func TestClientSessionsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/session", r.URL.Path)
		require.Equal(t, dir, r.URL.Query().Get("directory"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"ses_1","title":"one","directory":"/tmp","projectID":"p1","version":"1","time":{"created":1,"updated":2}}]`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	res, err := client.Sessions(t.Context(), SessionsRequest{Location: Location{Directory: dir}})
	require.NoError(t, err)
	require.Len(t, res.Sessions, 1)
	require.Equal(t, "ses_1", res.Sessions[0].ID)
}

func TestClientQuestionsUsesGlobalRequestRoute(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/question/request", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, err := client.Questions(t.Context(), Location{}, "ses_1")
	require.NoError(t, err)
}

func TestFirstTextReturnsLastText(t *testing.T) {
	t.Parallel()
	// firstText returns the last text (final answer), not all texts joined.
	got := firstText(json.RawMessage(`{"data":[{"role":"assistant","content":[{"type":"text","text":"first"},{"text":"second"}]}]}`))
	require.Equal(t, "second", got)
}

func TestIntermediatePartsExcludedFromSummary(t *testing.T) {
	t.Parallel()
	// Parts without a role (intermediate model steps) must not appear in message summaries.
	// final_text should be the last text, not everything concatenated.
	raw := json.RawMessage(`[
		{"id":"msg_1","role":"user","content":[{"text":"hello"}]},
		{"id":"prt_1","text":"internal step"},
		{"id":"msg_2","role":"assistant","content":[{"text":"Hello."}]}
	]`)
	require.Equal(t, "Hello.", firstText(raw))
	msgs := summarizeMessages(raw, 10)
	require.Len(t, msgs, 2)
	require.Equal(t, "user", msgs[0].Role)
	require.Equal(t, "assistant", msgs[1].Role)
}

func TestAgentsResultSerializes(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(AgentsResult{Agents: []Agent{{Name: "build", Mode: "subagent"}}})
	require.NoError(t, err)
	require.Contains(t, string(data), "build")
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
