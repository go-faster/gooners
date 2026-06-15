package opencode

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		case "/permission", "/question":
			writeJSON(w, `[]`)
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
		case "/permission":
			writeJSON(w, `[{"id":"perm_1","sessionID":"ses_1","title":"shell","text":"approve?"}]`)
		case "/question":
			writeJSON(w, `[]`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})
	mgr := NewManager(t.Context(), client, slog.Default())

	_, res, err := checkHandler(client, mgr)(t.Context(), nil, checkParams{SessionID: "ses_1"})
	require.NoError(t, err)
	// No job tracked by this server — status comes from opencode session state.
	require.Equal(t, string(JobRunning), res.Status)
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
		case "/permission", "/question":
			writeJSON(w, `[]`)
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
		case "/permission", "/question":
			writeJSON(w, `[]`)
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
		case "/permission", "/question":
			writeJSON(w, `[]`)
		default:
			require.Failf(t, "unexpected request", "path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, Config{BaseURL: server.URL})
	mgr := NewManager(t.Context(), client, slog.Default())
	mgr.jobs["ses_1"] = &Job{
		SessionID:       "ses_1",
		Status:          JobDone,
		PromptMessageID: "msg_prompt",
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

	// With messages present: submit timed out while session was running.
	t.Run("with messages", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/session/ses_1/message":
				writeJSON(w, `[{"id":"msg_1","role":"assistant","content":[{"text":"partial"}]}]`)
			case "/api/session/ses_1/context":
				writeJSON(w, `{"data":{"tokens":1}}`)
			case "/permission", "/question":
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
		require.Contains(t, res.Message, "may still be running")
	})

	// Without messages: clean error, session never produced output.
	t.Run("no messages", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/session/ses_1/message":
				writeJSON(w, `[]`)
			case "/api/session/ses_1/context":
				writeJSON(w, `{}`)
			case "/permission", "/question":
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
			Err:       errors.New("connection refused"),
		}

		_, res, err := checkHandler(client, mgr)(t.Context(), nil, checkParams{SessionID: "ses_1"})
		require.NoError(t, err)
		require.Equal(t, string(JobError), res.Status)
		require.Equal(t, "connection refused", res.Error)
		require.Equal(t, "handoff failed", res.Message)
	})
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
		case "/permission", "/question":
			writeJSON(w, `[]`)
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

func TestHealthHandler(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/health", r.URL.Path)
		writeJSON(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL + "/"})
	_, got, err := healthHandler(client)(t.Context(), nil, struct{}{})
	require.NoError(t, err)
	require.True(t, got.OK)
	require.Equal(t, server.URL, got.BaseURL)
	require.Equal(t, "opencode server is reachable", got.Message)
	require.JSONEq(t, `{"ok":true}`, string(got.Data))
}

func TestAgentsHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/agent", r.URL.Path)
		require.Equal(t, dir, r.URL.Query().Get("directory"))
		writeJSON(w, `[{"name":"build","description":"Build things","mode":"subagent"}]`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, got, err := agentsHandler(client)(t.Context(), nil, locationParams{Directory: dir})
	require.NoError(t, err)
	require.Equal(t, AgentsResult{Agents: []Agent{{Name: "build", Description: "Build things", Mode: "subagent"}}}, got)
}

func TestModelsHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/provider", r.URL.Path)
		require.Equal(t, dir, r.URL.Query().Get("directory"))
		writeJSON(w, `{"data":[{"id":"anthropic","name":"Anthropic","models":{"claude-3-5-sonnet":{"name":"Sonnet"}}}]}`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, got, err := modelsHandler(client)(t.Context(), nil, locationParams{Directory: dir})
	require.NoError(t, err)
	require.Equal(t, ModelsResult{
		Providers: []ProviderSummary{{ID: "anthropic", Name: "Anthropic", Models: 1}},
		Models:    []ModelSummary{{ProviderID: "anthropic", ID: "claude-3-5-sonnet", Name: "Sonnet"}},
	}, got)
}

func TestSessionsHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/session", r.URL.Path)
		require.Equal(t, dir, r.URL.Query().Get("directory"))
		writeJSON(w, "["+minimalSession+"]")
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, got, err := sessionsHandler(client)(t.Context(), nil, SessionsRequest{Location: Location{Directory: dir}})
	require.NoError(t, err)
	require.Len(t, got.Sessions, 1)
	require.Equal(t, "ses_1", got.Sessions[0].ID)
	require.Equal(t, "task", got.Sessions[0].Title)
	require.Equal(t, int64(1), got.Sessions[0].CreatedAt)
	require.Equal(t, int64(2), got.Sessions[0].UpdatedAt)
}

func TestPermissionsHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/permission", r.URL.Path)
		require.Equal(t, dir, r.URL.Query().Get("directory"))
		writeJSON(w, `[{"id":"perm_1","sessionID":"ses_1","title":"shell","text":"approve?"}]`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, got, err := permissionsHandler(client)(t.Context(), nil, requestListParams{locationParams: locationParams{Directory: dir}, SessionID: "ses_1"})
	require.NoError(t, err)
	require.Equal(t, RequestsResult{
		Requests: []RequestSummary{{ID: "perm_1", SessionID: "ses_1", Kind: "permission", Title: "shell", Text: "approve?", Preview: "approve?"}},
		Count:    1,
	}, got)
}

func TestPermissionReplyHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/permission/perm_1/reply", r.URL.Path)
		require.Equal(t, dir, r.URL.Query().Get("directory"))

		var body struct {
			Reply string `json:"reply"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "always", body.Reply)
		writeJSON(w, "true")
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, got, err := permissionReplyHandler(client)(t.Context(), nil, permissionReplyParams{
		locationParams: locationParams{Directory: dir},
		SessionID:      "ses_1",
		RequestID:      "perm_1",
		Reply:          "always",
	})
	require.NoError(t, err)
	require.True(t, got.OK)
	require.Equal(t, json.RawMessage("true"), got.Data)
}

func TestQuestionsHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/question", r.URL.Path)
		require.Equal(t, dir, r.URL.Query().Get("directory"))
		writeJSON(w, `[{"id":"q_1","sessionID":"ses_1","title":"which?","text":"pick one"}]`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	_, got, err := questionsHandler(client)(t.Context(), nil, requestListParams{locationParams: locationParams{Directory: dir}})
	require.NoError(t, err)
	require.Equal(t, RequestsResult{
		Requests: []RequestSummary{{ID: "q_1", SessionID: "ses_1", Kind: "question", Title: "which?", Text: "pick one", Preview: "pick one"}},
		Count:    1,
	}, got)
}

func TestQuestionReplyHandler(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		reject   bool
		path     string
		wantBody string
	}{
		{
			name:     "reply",
			reject:   false,
			path:     "/question/q_1/reply",
			wantBody: `{"answers":[["yes","no"]]}`,
		},
		{
			name:     "reject",
			reject:   true,
			path:     "/question/q_1/reject",
			wantBody: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, tc.path, r.URL.Path)
				require.Equal(t, dir, r.URL.Query().Get("directory"))

				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Equal(t, tc.wantBody, string(body))
				writeJSON(w, "true")
			}))
			t.Cleanup(server.Close)

			client := newTestClient(t, Config{BaseURL: server.URL})
			_, got, err := questionReplyHandler(client)(t.Context(), nil, questionReplyParams{
				locationParams: locationParams{Directory: dir},
				SessionID:      "ses_1",
				RequestID:      "q_1",
				Answers:        [][]string{{"yes", "no"}},
				Reject:         tc.reject,
			})
			require.NoError(t, err)
			require.True(t, got.OK)
		})
	}
}

func TestManagerJobsReturnsSnapshots(t *testing.T) {
	t.Parallel()
	mgr := NewManager(t.Context(), nil, nil)
	mgr.jobs["ses_1"] = &Job{
		SessionID:       "ses_1",
		Status:          JobRunning,
		PromptMessageID: "msg_1",
		Err:             errors.New("boom"),
		CreatedAt:       time.Unix(1, 0),
		UpdatedAt:       time.Unix(2, 0),
	}

	jobs := mgr.Jobs()
	require.Len(t, jobs, 1)
	require.Equal(t, "ses_1", jobs[0].SessionID)
	require.Equal(t, JobRunning, jobs[0].Status)
	require.Equal(t, "msg_1", jobs[0].PromptMessageID)
	require.EqualError(t, jobs[0].Err, "boom")
	require.Equal(t, time.Unix(1, 0), jobs[0].CreatedAt)
	require.Equal(t, time.Unix(2, 0), jobs[0].UpdatedAt)

	mgr.jobs["ses_1"].Status = JobDone
	require.Equal(t, JobRunning, jobs[0].Status)
}

func TestManagerStateDirPersistence(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	running := jobRecord{
		SessionID:       "ses_1",
		Status:          JobRunning,
		PromptMessageID: "msg_1",
		CreatedAt:       time.Unix(1, 0),
		UpdatedAt:       time.Unix(2, 0),
	}
	raw, err := json.Marshal(running)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "ses_1.json"), raw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "bad.json"), []byte(`{`), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(stateDir, "nested.json"), 0o700))

	mgr := NewManagerWithStateDir(t.Context(), nil, nil, stateDir)
	job, ok := mgr.Job("ses_1")
	require.True(t, ok)
	require.Equal(t, "ses_1", job.SessionID)
	require.Equal(t, JobUnknown, job.Status)
	require.NoError(t, job.Err)
	require.Equal(t, running.PromptMessageID, job.PromptMessageID)
	require.True(t, running.CreatedAt.Equal(job.CreatedAt))
	require.True(t, job.UpdatedAt.After(running.UpdatedAt))

	valid := &Job{
		SessionID:       "ses_2",
		Status:          JobDone,
		PromptMessageID: "msg_ok",
		CreatedAt:       time.Unix(3, 0),
		UpdatedAt:       time.Unix(4, 0),
	}
	mgr.saveJob(valid)
	saved, err := os.ReadFile(filepath.Join(stateDir, "ses_2.json"))
	require.NoError(t, err)
	require.Contains(t, string(saved), `"session_id":"ses_2"`)
	require.Contains(t, string(saved), `"status":"done"`)
	require.NotContains(t, string(saved), "err_message")

	invalid := &Job{SessionID: "../bad", Status: JobDone}
	mgr.saveJob(invalid)
	_, err = os.Stat(filepath.Join(stateDir, "bad.json"))
	require.NoError(t, err)
}

func TestClientBaseURL(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL + "/"})
	require.Equal(t, server.URL, client.BaseURL())
}

func TestRegister(t *testing.T) {
	t.Parallel()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})
	mgr := NewManager(t.Context(), client, nil)

	require.NotPanics(t, func() {
		Register(server, client, mgr)
	})
}

func TestClientValidationErrors(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})

	_, err := client.Messages(t.Context(), Location{}, "")
	require.EqualError(t, err, "session_id is required")

	_, err = client.Context(t.Context(), Location{}, "")
	require.EqualError(t, err, "session_id is required")

	_, err = client.Prompt(t.Context(), Location{}, "", PromptRequest{})
	require.EqualError(t, err, "session_id is required")

	_, err = client.PermissionReply(t.Context(), Location{}, "", "", "always", "")
	require.EqualError(t, err, "request_id is required")

	_, err = client.QuestionReply(t.Context(), Location{}, "", "", false, nil)
	require.EqualError(t, err, "request_id is required")

	mgr := NewManager(t.Context(), client, nil)
	_, err = mgr.Submit(t.Context(), Location{}, "", CreateSessionRequest{}, PromptRequest{})
	require.EqualError(t, err, "prompt is required")

	_, err = mgr.Submit(t.Context(), Location{}, "../bad", CreateSessionRequest{}, PromptRequest{Prompt: PromptPayload{Text: "do it"}})
	require.ErrorContains(t, err, "invalid sessionID")
}

func TestHandlerErrors(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})

	_, _, err := permissionReplyHandler(client)(t.Context(), nil, permissionReplyParams{})
	require.EqualError(t, err, "request_id is required")

	_, _, err = questionReplyHandler(client)(t.Context(), nil, questionReplyParams{})
	require.EqualError(t, err, "request_id is required")

	_, _, err = runHandler(client, NewManager(t.Context(), client, nil))(t.Context(), nil, runParams{})
	require.EqualError(t, err, "prompt is required")

	_, _, err = fireHandler(NewManager(t.Context(), client, nil))(t.Context(), nil, fireParams{})
	require.EqualError(t, err, "prompt is required")

	_, _, err = checkHandler(client, NewManager(t.Context(), client, nil))(t.Context(), nil, checkParams{})
	require.EqualError(t, err, "session_id is required")
}
