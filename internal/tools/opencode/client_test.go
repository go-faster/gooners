package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientLocationAndAuthHeaders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent" {
			t.Fatalf("path = %q, want /api/agent", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "secret" {
			t.Fatalf("basic auth = %q/%q/%v", user, pass, ok)
		}
		if got := r.Header.Get("x-opencode-directory"); got != dir {
			t.Fatalf("x-opencode-directory = %q, want %q", got, dir)
		}
		if got := r.Header.Get("x-opencode-workspace"); got != "ws" {
			t.Fatalf("x-opencode-workspace = %q, want ws", got)
		}
		_, _ = w.Write([]byte(`{"data":{"build":{"description":"Build things","mode":"subagent"}}}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL, Username: "opencode", Password: "secret"})
	agents, err := client.Agents(t.Context(), Location{Directory: dir, Workspace: "ws"})
	if err != nil {
		t.Fatalf("Agents() error = %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "build" || agents[0].Mode != "subagent" {
		t.Fatalf("agents = %+v", agents)
	}
}

func TestClientCreateSessionAndPrompt(t *testing.T) {
	t.Parallel()
	var sawCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			if r.Method != http.MethodPost {
				t.Fatalf("create method = %s", r.Method)
			}
			sawCreate = true
			var body CreateSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if got := r.Header.Get("x-opencode-workspace"); got != "ws" {
				t.Fatalf("x-opencode-workspace = %q, want ws", got)
			}
			if body.Title != "Fix tests" || body.Agent != "build" || body.Location != nil {
				t.Fatalf("create body = %+v", body)
			}
			_, _ = w.Write([]byte(`{"data":{"id":"ses_1","title":"Fix tests","time":{"created":1,"updated":2}}}`))
		case "/api/session/ses_1/prompt":
			if !sawCreate {
				t.Fatal("prompt called before create")
			}
			var body PromptRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode prompt body: %v", err)
			}
			if body.Prompt.Text != "do it" || body.Agent != "build" || body.Model == nil || body.Model.ProviderID != "anthropic" {
				t.Fatalf("prompt body = %+v", body)
			}
			_, _ = w.Write([]byte(`{"data":{"messageID":"msg_1"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	session, err := client.CreateSession(t.Context(), Location{Workspace: "ws"}, CreateSessionRequest{Title: "Fix tests", Agent: "build"})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if session.ID != "ses_1" || session.CreatedAt != 1 || session.UpdatedAt != 2 {
		t.Fatalf("session = %+v", session)
	}
	_, err = client.Prompt(t.Context(), Location{}, session.ID, PromptRequest{
		Prompt: PromptPayload{Text: "do it"},
		Agent:  "build",
		Model:  &ModelRef{ProviderID: "anthropic", ModelID: "claude"},
	})
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
}

func TestClientCreateSessionMissingRouteError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, err := client.CreateSession(t.Context(), Location{}, CreateSessionRequest{})
	if err == nil {
		t.Fatal("CreateSession() error = nil")
	}
	if !strings.Contains(err.Error(), "POST /api/session") {
		t.Fatalf("error = %q, want route hint", err.Error())
	}
}

func TestClientSessionsPagination(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Fatalf("limit = %q, want 10", got)
		}
		if got := r.URL.Query().Get("cursor"); got != "next" {
			t.Fatalf("cursor = %q, want next", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"ses_1","title":"one"}]}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	res, err := client.Sessions(t.Context(), SessionsRequest{Limit: 10, Cursor: "next"})
	if err != nil {
		t.Fatalf("Sessions() error = %v", err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].ID != "ses_1" {
		t.Fatalf("sessions = %+v", res.Sessions)
	}
}

func TestClientQuestionsUsesSessionRoute(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/ses_1/question" {
			t.Fatalf("path = %q, want per-session question route", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, err := client.Questions(t.Context(), Location{}, "ses_1")
	if err != nil {
		t.Fatalf("Questions() error = %v", err)
	}
}

func TestClientInvalidDirectoryHeader(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be called")
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, err := client.Agents(t.Context(), Location{Directory: "bad\nvalue"})
	if err == nil {
		t.Fatal("Agents() error = nil")
	}
	if !strings.Contains(err.Error(), "invalid header") {
		t.Fatalf("error = %q, want invalid header", err.Error())
	}
}

func TestFirstText(t *testing.T) {
	t.Parallel()
	got := firstText(json.RawMessage(`{"data":[{"role":"assistant","content":[{"type":"text","text":"first"},{"text":"second"}]}]}`))
	if got != "first\nsecond" {
		t.Fatalf("firstText() = %q", got)
	}
}

func TestRawFieldsDoNotSerialize(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(AgentsResult{Agents: []Agent{{Name: "build", Raw: json.RawMessage(`{"large":true}`)}}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(data), "large") || strings.Contains(string(data), "raw") {
		t.Fatalf("serialized raw data: %s", data)
	}
}

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	client, err := NewClient(cfg, time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
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
	if err == nil {
		t.Fatal("Health() error = nil")
	}
}
