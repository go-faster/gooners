package opencode

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// writeJSON writes a JSON response with application/json Content-Type.
func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// minimalSession is a valid opencode Session JSON response (instance route format).
const minimalSession = `{"id":"ses_1","title":"task","directory":"/tmp","projectID":"p1","version":"1","time":{"created":1,"updated":2}}`

func TestRunHandlerHappyPathCompactResult(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			writeJSON(w, minimalSession)
		case "/session/ses_1/message":
			if r.Method == http.MethodPost {
				writeJSON(w, `{"messageID":"msg_prompt"}`)
			} else {
				writeJSON(w, `[{"id":"msg_1","role":"assistant","content":[{"text":"done"}]}]`)
			}
		case "/api/session/ses_1/context":
			writeJSON(w, `{"data":{"tokens":1}}`)
		case "/api/session/ses_1/permission/request", "/api/question/request":
			writeJSON(w, `{"data":[]}`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	mgr := NewManager(t.Context(), client, nil)
	_, res, err := runHandler(client, mgr)(t.Context(), nil, runParams{Prompt: "do it"})
	require.NoError(t, err)
	require.Equal(t, "done", res.Status)
	require.Equal(t, "msg_prompt", res.PromptMessageID)
	require.Equal(t, "done", res.FinalText)
	require.Empty(t, res.RawMessages)
	require.Empty(t, res.RawContext)
}

func TestFireHandlerReturnsSessionID(t *testing.T) {
	t.Parallel()
	releasePrompt := make(chan struct{})
	promptStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			writeJSON(w, minimalSession)
		case "/session/ses_1/message":
			close(promptStarted)
			<-releasePrompt
			writeJSON(w, `{"messageID":"msg_prompt"}`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releasePrompt) })
	client := newTestClient(t, Config{BaseURL: server.URL})
	mgr := NewManager(t.Context(), client, slog.Default())

	_, res, err := fireHandler(mgr)(t.Context(), nil, fireParams{runParams: runParams{Prompt: "do it"}})
	require.NoError(t, err)
	require.Equal(t, "ses_1", res.SessionID)
	require.Empty(t, res.PromptMessageID)

	select {
	case <-promptStarted:
	case <-time.After(time.Second):
		t.Fatal("prompt was not submitted")
	}
	job, ok := mgr.Job("ses_1")
	require.True(t, ok)
	require.Equal(t, JobRunning, job.Status)
}

func TestCheckHandlerReportsPendingPermission(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_1/message":
			writeJSON(w, `[{"id":"msg_1","role":"assistant","content":[{"text":"waiting"}]}]`)
		case "/api/session/ses_1/context":
			writeJSON(w, `{"data":{"tokens":1}}`)
		case "/api/session/ses_1/permission/request":
			writeJSON(w, `{"data":[{"id":"perm_1","sessionID":"ses_1","title":"shell","text":"approve?"}]}`)
		case "/api/question/request":
			writeJSON(w, `{"data":[]}`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})
	mgr := NewManager(t.Context(), client, slog.Default())

	_, res, err := checkHandler(client, mgr)(t.Context(), nil, checkParams{SessionID: "ses_1"})
	require.NoError(t, err)
	require.Equal(t, "unknown", res.Status)
	require.Equal(t, 1, res.PendingPermissionCount)
	require.Contains(t, res.Message, "handoff_permissions")
}

func TestCheckHandlerVerboseReturnsDecodedRawJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_1/message":
			writeJSON(w, `[{"info":{"id":"msg_1","role":"assistant"},"parts":[{"text":"done"}]}]`)
		case "/api/session/ses_1/context":
			writeJSON(w, `{"data":{"tokens":1}}`)
		case "/api/session/ses_1/permission/request", "/api/question/request":
			writeJSON(w, `{"data":[]}`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})
	mgr := NewManager(t.Context(), client, slog.Default())

	_, res, err := checkHandler(client, mgr)(t.Context(), nil, checkParams{SessionID: "ses_1", Verbose: true})
	require.NoError(t, err)
	require.IsType(t, []map[string]any{}, res.RawMessages)
	require.IsType(t, map[string]any{}, res.RawContext)

	data, err := json.Marshal(res)
	require.NoError(t, err)
	encoded := string(data)
	require.Contains(t, encoded, `"raw_messages":[`)
	require.NotContains(t, encoded, `"raw_messages":[123`)
}

func TestCheckHandlerReportsRunningJob(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_1/message":
			writeJSON(w, `[]`)
		case "/api/session/ses_1/context":
			writeJSON(w, `{}`)
		case "/api/session/ses_1/permission/request", "/api/question/request":
			writeJSON(w, `{"data":[]}`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})
	mgr := NewManager(t.Context(), client, slog.Default())
	mgr.jobs["ses_1"] = &Job{SessionID: "ses_1", Status: JobRunning}

	_, res, err := checkHandler(client, mgr)(t.Context(), nil, checkParams{SessionID: "ses_1"})
	require.NoError(t, err)
	require.Equal(t, string(JobRunning), res.Status)
	require.Equal(t, "handoff is still running", res.Message)
}

func TestCheckHandlerReportsDoneJob(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_1/message":
			writeJSON(w, `[{"id":"msg_1","role":"assistant","content":[{"text":"all done"}]}]`)
		case "/api/session/ses_1/context":
			writeJSON(w, `{"data":{"tokens":1}}`)
		case "/api/session/ses_1/permission/request", "/api/question/request":
			writeJSON(w, `{"data":[]}`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})
	mgr := NewManager(t.Context(), client, slog.Default())
	mgr.jobs["ses_1"] = &Job{
		SessionID:    "ses_1",
		Status:       JobDone,
		PromptResult: json.RawMessage(`{"messageID":"msg_prompt"}`),
	}

	_, res, err := checkHandler(client, mgr)(t.Context(), nil, checkParams{SessionID: "ses_1"})
	require.NoError(t, err)
	require.Equal(t, string(JobDone), res.Status)
	require.Equal(t, "msg_prompt", res.PromptMessageID)
	require.Equal(t, "all done", res.FinalText)
	require.Empty(t, res.Error)
}

func TestCheckHandlerReportsErrorJob(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_1/message":
			writeJSON(w, `[{"id":"msg_1","role":"assistant","content":[{"text":"partial"}]}]`)
		case "/api/session/ses_1/context":
			writeJSON(w, `{"data":{"tokens":1}}`)
		case "/api/session/ses_1/permission/request", "/api/question/request":
			writeJSON(w, `{"data":[]}`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})
	mgr := NewManager(t.Context(), client, slog.Default())
	mgr.jobs["ses_1"] = &Job{
		SessionID: "ses_1",
		Status:    JobError,
		Err:       errors.New("context deadline exceeded"),
	}

	_, res, err := checkHandler(client, mgr)(t.Context(), nil, checkParams{SessionID: "ses_1"})
	require.NoError(t, err)
	require.Equal(t, string(JobError), res.Status)
	require.Equal(t, "context deadline exceeded", res.Error)
	require.Equal(t, "handoff failed", res.Message)
}

func TestRunHandlerBlockedState(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			writeJSON(w, minimalSession)
		case "/session/ses_1/message":
			if r.Method == http.MethodPost {
				writeJSON(w, `{"messageID":"msg_prompt"}`)
			} else {
				writeJSON(w, `[{"id":"msg_1","role":"assistant","content":[{"text":"still working"}]}]`)
			}
		case "/api/session/ses_1/context":
			writeJSON(w, `{"data":{"tokens":1}}`)
		case "/api/session/ses_1/permission/request", "/api/question/request":
			writeJSON(w, `{"data":[]}`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})

	// Wait is a no-op so runHandler always sees "done" unless prompt itself fails.
	mgr := NewManager(t.Context(), client, nil)
	_, res, err := runHandler(client, mgr)(t.Context(), nil, runParams{Prompt: "do it"})
	require.NoError(t, err)
	require.Equal(t, "done", res.Status)
	require.Equal(t, "still working", res.FinalText)
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

func TestIsSessionFinishedJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"v2 finished", `[{"info":{"role":"assistant","finish":"stop"}}]`, true},
		{"v2 no finish", `[{"info":{"role":"assistant"}}]`, false},
		{"v2 empty finish", `[{"info":{"role":"assistant","finish":""}}]`, false},
		{"flat finished", `[{"role":"assistant","finish":"stop"}]`, true},
		{"flat no finish", `[{"role":"assistant"}]`, false},
		{"flat empty finish", `[{"role":"assistant","finish":""}]`, false},
		{"empty array", `[]`, false},
		{"not assistant v2", `[{"info":{"role":"user","finish":"stop"}}]`, false},
		{"not assistant flat", `[{"role":"user","finish":"stop"}]`, false},
		{"last assistant wins v2", `[{"info":{"role":"assistant","finish":"stop"}},{"info":{"role":"assistant"}}]`, false},
		{"last assistant wins flat", `[{"role":"assistant","finish":"stop"},{"role":"assistant"}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, isSessionFinishedJSON(json.RawMessage(tc.input)))
		})
	}
}

func TestCollectTextTraversesToolBlocks(t *testing.T) {
	t.Parallel()
	// firstText returns the last collected text (final answer after tool use).
	raw := json.RawMessage(`{"data":[{"role":"assistant","tool_result":{"output":{"text":"tool output"}},"tool_use":{"input":{"text":"tool input"}}}]}`)
	got := firstText(raw)
	require.NotEmpty(t, got)
}
