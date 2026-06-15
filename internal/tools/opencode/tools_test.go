package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-faster/jx"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	opencodeapi "github.com/go-faster/gooners/internal/tools/opencode/opencodeapi"
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
	_, err = mgr.Submit(t.Context(), Location{}, "", PromptRequest{})
	require.EqualError(t, err, "prompt is required")

	_, err = mgr.Submit(t.Context(), Location{}, "../bad", PromptRequest{Prompt: PromptPayload{Text: "do it"}})
	require.ErrorContains(t, err, "invalid sessionID")

	_, err = mgr.Submit(t.Context(), Location{}, "", PromptRequest{Prompt: PromptPayload{Text: "do it"}})
	require.EqualError(t, err, "session_id is required")
}

func TestToolHandlers(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2HealthGetFunc: func(ctx context.Context) (opencodeapi.V2HealthGetRes, error) {
			return &opencodeapi.V2HealthGetOK{Healthy: true}, nil
		},
		V2AgentListFunc: func(ctx context.Context, params opencodeapi.V2AgentListParams) (opencodeapi.V2AgentListRes, error) {
			return &opencodeapi.V2AgentListOK{
				Data: []opencodeapi.AgentV2Info{
					{
						ID:          "coder",
						Description: opencodeapi.NewOptString("Writes code"),
						Mode:        opencodeapi.AgentV2InfoModePrimary,
					},
				},
			}, nil
		},
		V2ProviderListFunc: func(ctx context.Context, params opencodeapi.V2ProviderListParams) (opencodeapi.V2ProviderListRes, error) {
			return &opencodeapi.V2ProviderListOK{
				Data: []opencodeapi.ProviderV2Info{
					{
						ID:      "anthropic",
						Name:    "Anthropic",
						API:     jx.Raw(`{}`),
						Enabled: jx.Raw(`true`),
					},
				},
			}, nil
		},
		V2ModelListFunc: func(ctx context.Context, params opencodeapi.V2ModelListParams) (opencodeapi.V2ModelListRes, error) {
			return &opencodeapi.V2ModelListOK{
				Data: []opencodeapi.ModelV2Info{
					{
						ProviderID: "anthropic",
						ID:         "claude-3-5-sonnet",
						Name:       "Claude 3.5 Sonnet",
						API:        jx.Raw(`{}`),
						Capabilities: opencodeapi.ModelV2InfoCapabilities{
							Input:  []string{},
							Output: []string{},
						},
						Cost:     []opencodeapi.ModelV2InfoCostItem{},
						Status:   opencodeapi.ModelV2InfoStatusActive,
						Variants: []opencodeapi.ModelV2InfoVariantsItem{},
						Time: opencodeapi.ModelV2InfoTime{
							Released: jx.Raw(`"2026-06-15"`),
						},
					},
				},
			}, nil
		},
		V2SessionListFunc: func(ctx context.Context, params opencodeapi.V2SessionListParams) (opencodeapi.V2SessionListRes, error) {
			s1 := opencodeapi.SessionV2Info{ID: "ses-123", Title: "Test Session"}
			return &opencodeapi.SessionsResponse{Data: []opencodeapi.SessionV2Info{s1}}, nil
		},
		SessionCreateFunc: func(ctx context.Context, req opencodeapi.OptSessionCreateReq, params opencodeapi.SessionCreateParams) (opencodeapi.SessionCreateRes, error) {
			return &opencodeapi.Session{ID: "ses-new", Title: "New Session"}, nil
		},
		SessionMessageFunc: func(ctx context.Context, req *opencodeapi.SessionMessageReq, params opencodeapi.SessionMessageParams) (opencodeapi.SessionMessageRes, error) {
			return &opencodeapi.SessionMessageOK{
				Info: opencodeapi.SessionMessageOKInfo{
					ID:   "msg-123",
					Role: "assistant",
				},
			}, nil
		},
		V2SessionPermissionListFunc: func(ctx context.Context, params opencodeapi.V2SessionPermissionListParams) (opencodeapi.V2SessionPermissionListRes, error) {
			return &opencodeapi.V2SessionPermissionListOK{
				Data: []opencodeapi.PermissionV2Request{
					{
						ID:        "per-1",
						Action:    "read_file",
						SessionID: "ses-123",
						Resources: []string{},
					},
				},
			}, nil
		},
		V2SessionPermissionReplyFunc: func(ctx context.Context, req *opencodeapi.V2SessionPermissionReplyReq, params opencodeapi.V2SessionPermissionReplyParams) (opencodeapi.V2SessionPermissionReplyRes, error) {
			return &opencodeapi.V2SessionPermissionReplyNoContent{}, nil
		},
		V2QuestionRequestListFunc: func(ctx context.Context, params opencodeapi.V2QuestionRequestListParams) (opencodeapi.V2QuestionRequestListRes, error) {
			return &opencodeapi.V2QuestionRequestListOK{
				Data: []opencodeapi.QuestionV2Request{
					{
						ID:        "que-1",
						SessionID: "ses-123",
						Questions: []opencodeapi.QuestionV2Info{},
					},
				},
			}, nil
		},
		V2SessionQuestionReplyFunc: func(ctx context.Context, req *opencodeapi.QuestionV2Reply, params opencodeapi.V2SessionQuestionReplyParams) (opencodeapi.V2SessionQuestionReplyRes, error) {
			return &opencodeapi.V2SessionQuestionReplyNoContent{}, nil
		},
		SessionMessagesFunc: func(ctx context.Context, params opencodeapi.SessionMessagesParams) (opencodeapi.SessionMessagesRes, error) {
			var resp opencodeapi.SessionMessagesOKApplicationJSON
			return &resp, nil
		},
		V2SessionContextFunc: func(ctx context.Context, params opencodeapi.V2SessionContextParams) (opencodeapi.V2SessionContextRes, error) {
			return &opencodeapi.V2SessionContextOK{Data: []opencodeapi.SessionMessage{}}, nil
		},
		V2PermissionRequestListFunc: func(ctx context.Context, params opencodeapi.V2PermissionRequestListParams) (opencodeapi.V2PermissionRequestListRes, error) {
			return &opencodeapi.V2PermissionRequestListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
		},
	}
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{})

	// 1. healthHandler
	{
		h := healthHandler(client)
		_, res, err := h(t.Context(), nil, struct{}{})
		require.NoError(t, err)
		require.True(t, res.OK)
	}

	// 2. agentsHandler
	{
		h := agentsHandler(client)
		_, res, err := h(t.Context(), nil, locationParams{})
		require.NoError(t, err)
		require.Len(t, res.Agents, 1)
		require.Equal(t, "coder", res.Agents[0].Name)
	}

	// 3. modelsHandler
	{
		h := modelsHandler(client)
		_, res, err := h(t.Context(), nil, modelsParams{IncludeModels: true})
		require.NoError(t, err)
		require.Len(t, res.Models, 1)
		require.Equal(t, "claude-3-5-sonnet", res.Models[0].ID)
	}

	// 4. sessionsHandler
	{
		h := sessionsHandler(client)
		_, res, err := h(t.Context(), nil, SessionsRequest{})
		require.NoError(t, err)
		require.Len(t, res.Sessions, 1)
		require.Equal(t, "ses-123", res.Sessions[0].ID)
	}

	// 5. createSessionHandler
	{
		h := createSessionHandler(client)
		_, res, err := h(t.Context(), nil, createSessionParams{Title: "New Session"})
		require.NoError(t, err)
		require.Equal(t, "ses-new", res.SessionID)
	}

	// 6. fireHandler
	{
		h := fireHandler(client, mgr)
		_, res, err := h(t.Context(), nil, fireParams{SessionID: "ses-123", Prompt: "hello"})
		require.NoError(t, err)
		require.Equal(t, "ses-123", res.SessionID)
	}

	// 7. permissionReplyHandler
	{
		h := permissionReplyHandler(client)
		_, res, err := h(t.Context(), nil, permissionReplyParams{SessionID: "ses-123", Reply: "once"})
		require.NoError(t, err)
		require.True(t, res.OK)
	}

	// 8. questionReplyHandler
	{
		h := questionReplyHandler(client)
		_, res, err := h(t.Context(), nil, questionReplyParams{SessionID: "ses-123", Answers: [][]string{{"ans1"}}})
		require.NoError(t, err)
		require.True(t, res.OK)
	}

	// 9. checkHandler
	{
		h := checkHandler(client, mgr)
		_, res, err := h(t.Context(), nil, checkParams{SessionID: "ses-123"})
		require.NoError(t, err)
		require.Equal(t, "ses-123", res.SessionID)
	}
}
