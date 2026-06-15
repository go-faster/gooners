package opencode

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestManagerJobsReturnsSnapshots(t *testing.T) {
	t.Parallel()
	mgr := NewManager(t.Context(), nil, ManagerOptions{})
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

	mgr := NewManager(t.Context(), nil, ManagerOptions{StateDir: stateDir})
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
	mgr := NewManager(t.Context(), client, ManagerOptions{})

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

	mgr := NewManager(t.Context(), client, ManagerOptions{})
	_, err = mgr.Submit(t.Context(), Location{}, "", CreateSessionRequest{}, PromptRequest{})
	require.EqualError(t, err, "prompt is required")

	_, err = mgr.Submit(t.Context(), Location{}, "../bad", CreateSessionRequest{}, PromptRequest{Prompt: PromptPayload{Text: "do it"}})
	require.ErrorContains(t, err, "invalid sessionID")
}
