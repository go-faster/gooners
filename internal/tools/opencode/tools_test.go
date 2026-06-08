package opencode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	_, res, err := waitHandler(client, time.Second)(t.Context(), nil, sessionParams{SessionID: "ses_1"})
	require.NoError(t, err)
	require.Equal(t, "blocked_or_running", res.Status)
	require.Equal(t, 1, res.PendingPermissionCount)
	require.Contains(t, res.Message, "handoff_wait")
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
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	_, res, err := runHandler(client, time.Second)(t.Context(), nil, runParams{Prompt: "do it"})
	require.NoError(t, err)
	require.Equal(t, "completed", res.Status)
	require.Equal(t, "msg_prompt", res.PromptMessageID)
	require.Equal(t, "done", res.FinalText)
	require.Empty(t, res.RawMessages)
	require.Empty(t, res.RawContext)
}

func TestFireHandlerReturnsSessionID(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			_, _ = w.Write([]byte(`{"data":{"id":"ses_1","title":"task"}}`))
		case "/api/session/ses_1/prompt":
			_, _ = w.Write([]byte(`{"data":{"messageID":"msg_prompt"}}`))
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	_, res, err := fireHandler(client)(t.Context(), nil, fireParams{runParams: runParams{Prompt: "do it"}})
	require.NoError(t, err)
	require.Equal(t, "ses_1", res.SessionID)
	require.Equal(t, "msg_prompt", res.PromptMessageID)
}

func TestCheckHandlerReportsPendingPermission(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_1/message":
			_, _ = w.Write([]byte(`{"data":[{"id":"msg_1","role":"assistant","content":[{"text":"waiting"}]}]}`))
		case "/api/session/ses_1/context":
			_, _ = w.Write([]byte(`{"data":{"tokens":1}}`))
		case "/api/session/ses_1/permission":
			_, _ = w.Write([]byte(`{"data":[{"id":"perm_1","sessionID":"ses_1","title":"shell","text":"approve?"}]}`))
		case "/api/session/ses_1/question":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	_, res, err := checkHandler(client)(t.Context(), nil, checkParams{SessionID: "ses_1"})
	require.NoError(t, err)
	require.Equal(t, 1, res.PendingPermissionCount)
	require.Contains(t, res.Message, "handoff_permissions")
}

func TestRunHandlerBlockedState(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			_, _ = w.Write([]byte(`{"data":{"id":"ses_1","title":"task"}}`))
		case "/api/session/ses_1/prompt":
			_, _ = w.Write([]byte(`{"data":{"messageID":"msg_prompt"}}`))
		case "/api/session/ses_1/wait":
			http.Error(w, "not idle", http.StatusServiceUnavailable)
		case "/api/session/ses_1/message":
			_, _ = w.Write([]byte(`{"data":[{"id":"msg_1","role":"assistant","content":[{"text":"still working"}]}]}`))
		case "/api/session/ses_1/context":
			_, _ = w.Write([]byte(`{"data":{"tokens":1}}`))
		case "/api/session/ses_1/permission", "/api/session/ses_1/question":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	_, res, err := runHandler(client, time.Second)(t.Context(), nil, runParams{Prompt: "do it"})
	require.NoError(t, err)
	require.Equal(t, "blocked_or_running", res.Status)
	require.Contains(t, res.Message, "handoff_wait")
}

func TestSummaryOutputOmitsDuplicateAndRawFields(t *testing.T) {
	t.Parallel()
	msg := MessageSummary{ID: "msg_1", Role: "assistant", Text: strings.Repeat("a", 1201)}
	data, err := json.Marshal(struct {
		Health HealthResult          `json:"health"`
		Perm   PermissionReplyResult `json:"perm"`
		Msg    MessageSummary        `json:"msg"`
	}{
		Health: HealthResult{OK: true, Data: json.RawMessage(`{"raw":true}`)},
		Perm:   PermissionReplyResult{OK: true, Data: json.RawMessage(`{"raw":true}`)},
		Msg:    msg,
	})
	require.NoError(t, err)
	encoded := string(data)
	require.NotContains(t, encoded, "preview")
	require.NotContains(t, encoded, "raw")
}

func TestCollectTextTraversesToolBlocks(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"data":[{"role":"assistant","tool_result":{"output":{"text":"tool output"}},"tool_use":{"input":{"text":"tool input"}}}]}`)
	got := firstText(raw)
	require.Contains(t, got, "tool output")
	require.Contains(t, got, "tool input")
}
