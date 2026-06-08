package opencode

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaitHandlerReturnsBlockedState(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_1/wait":
			http.Error(w, "still running", http.StatusServiceUnavailable)
		case "/api/session/ses_1/message":
			_, _ = w.Write([]byte(`{"data":[{"id":"msg_1","role":"assistant","content":[{"text":"working"}]}]}`))
		case "/api/session/ses_1/context":
			_, _ = w.Write([]byte(`{"data":{"tokens":1}}`))
		case "/api/session/ses_1/permission":
			_, _ = w.Write([]byte(`{"data":[{"id":"perm_1","sessionID":"ses_1","title":"shell"}]}`))
		case "/api/session/ses_1/question":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	_, res, err := waitHandler(client, time.Second)(t.Context(), nil, sessionParams{SessionID: "ses_1"})
	if err != nil {
		t.Fatalf("waitHandler() error = %v", err)
	}
	if res.Status != "blocked_or_running" {
		t.Fatalf("status = %q, want blocked_or_running", res.Status)
	}
	if res.PendingPermissionCount != 1 || !strings.Contains(res.Message, "handoff_wait") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunHandlerHappyPathCompactResult(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			_, _ = w.Write([]byte(`{"data":{"id":"ses_1","title":"task"}}`))
		case "/api/session/ses_1/prompt":
			_, _ = w.Write([]byte(`{"data":{"messageID":"msg_prompt"}}`))
		case "/api/session/ses_1/wait":
			_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
		case "/api/session/ses_1/message":
			_, _ = w.Write([]byte(`{"data":[{"id":"msg_1","role":"assistant","content":[{"text":"done"}]}]}`))
		case "/api/session/ses_1/context":
			_, _ = w.Write([]byte(`{"data":{"tokens":1}}`))
		case "/api/session/ses_1/permission", "/api/session/ses_1/question":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	_, res, err := runHandler(client, time.Second)(t.Context(), nil, runParams{Prompt: "do it"})
	if err != nil {
		t.Fatalf("runHandler() error = %v", err)
	}
	if res.Status != "completed" || res.PromptMessageID != "msg_prompt" || res.FinalText != "done" {
		t.Fatalf("result = %+v", res)
	}
	if len(res.RawMessages) != 0 || len(res.RawContext) != 0 {
		t.Fatalf("raw output should be omitted by default: %+v", res)
	}
}
