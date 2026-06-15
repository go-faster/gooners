package opencode

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-faster/jx"
	"github.com/stretchr/testify/require"

	opencodeapi "github.com/go-faster/gooners/internal/tools/opencode/opencodeapi"
)

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	client, err := NewClient(cfg, 5*time.Second)
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

func setupTestServer(t *testing.T, handler opencodeapi.Handler) (client *Client) {
	t.Helper()

	server, err := opencodeapi.NewServer(handler)
	require.NoError(t, err)
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)

	var errNew error
	client, errNew = NewClient(Config{BaseURL: ts.URL}, 5*time.Second)
	require.NoError(t, errNew)

	return client
}

func TestNewClient_BasicAuthAndLogger(t *testing.T) {
	t.Parallel()

	var called bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		username, password, ok := r.BasicAuth()
		require.True(t, ok)
		require.Equal(t, "user", username)
		require.Equal(t, "pass", password)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy": true}`))
	}))
	t.Cleanup(ts.Close)

	client, err := NewClient(Config{
		BaseURL:   ts.URL,
		Username:  "user",
		Password:  "pass",
		APILogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, time.Second)
	require.NoError(t, err)

	res, err := client.Health(t.Context())
	require.NoError(t, err)
	require.Contains(t, string(res), `"healthy"`)
	require.True(t, called)
}

func TestClient_Health(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2HealthGetFunc: func(ctx context.Context) (opencodeapi.V2HealthGetRes, error) {
			return &opencodeapi.V2HealthGetOK{Healthy: true}, nil
		},
	}
	client := setupTestServer(t, handler)
	res, err := client.Health(t.Context())
	require.NoError(t, err)
	require.JSONEq(t, `{"healthy": true}`, string(res))
}

func TestClient_Agents(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
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
	}
	client := setupTestServer(t, handler)
	agents, err := client.Agents(t.Context(), Location{Directory: "/src"})
	require.NoError(t, err)
	require.Len(t, agents, 1)
	require.Equal(t, "coder", agents[0].Name)
	require.Equal(t, "Writes code", agents[0].Description)
}

func TestClient_ProvidersAndModels(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
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
	}
	client := setupTestServer(t, handler)
	res, err := client.ProvidersAndModels(t.Context(), Location{})
	require.NoError(t, err)
	require.Len(t, res.Providers, 1)
	require.Equal(t, "anthropic", res.Providers[0].ID)
	require.Equal(t, "Anthropic", res.Providers[0].Name)
	require.Equal(t, 1, res.Providers[0].Models)

	require.Len(t, res.Models, 1)
	require.Equal(t, "claude-3-5-sonnet", res.Models[0].ID)
	require.Equal(t, "anthropic", res.Models[0].ProviderID)
}

func TestClient_Sessions(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2SessionListFunc: func(ctx context.Context, params opencodeapi.V2SessionListParams) (opencodeapi.V2SessionListRes, error) {
			s1 := opencodeapi.SessionV2Info{
				ID:    "ses-123",
				Title: "Test Session",
			}
			return &opencodeapi.SessionsResponse{
				Data: []opencodeapi.SessionV2Info{s1},
			}, nil
		},
	}
	client := setupTestServer(t, handler)
	res, err := client.Sessions(t.Context(), SessionsRequest{})
	require.NoError(t, err)
	require.Len(t, res.Sessions, 1)
	require.Equal(t, "ses-123", res.Sessions[0].ID)
	require.Equal(t, "Test Session", res.Sessions[0].Title)
}

func TestClient_CreateSession(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		SessionCreateFunc: func(ctx context.Context, req opencodeapi.OptSessionCreateReq, params opencodeapi.SessionCreateParams) (opencodeapi.SessionCreateRes, error) {
			s := opencodeapi.Session{
				ID:    "ses-new",
				Title: "New Session",
			}
			return &s, nil
		},
	}
	client := setupTestServer(t, handler)
	sess, err := client.CreateSession(t.Context(), Location{}, CreateSessionRequest{Title: "New Session"})
	require.NoError(t, err)
	require.Equal(t, "ses-new", sess.ID)
	require.Equal(t, "New Session", sess.Title)
}

func TestClient_Prompt(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		SessionMessageFunc: func(ctx context.Context, req *opencodeapi.SessionMessageReq, params opencodeapi.SessionMessageParams) (opencodeapi.SessionMessageRes, error) {
			return &opencodeapi.SessionMessageOK{
				Info: opencodeapi.SessionMessageOKInfo{
					ID:   "msg-123",
					Role: "assistant",
				},
			}, nil
		},
	}
	client := setupTestServer(t, handler)
	res, err := client.Prompt(t.Context(), Location{}, "ses-1", PromptRequest{
		Prompt: PromptPayload{Text: "hello"},
	})
	require.NoError(t, err)
	require.Contains(t, string(res), `"id":"msg-123"`)
}

func TestClient_Messages(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		SessionMessagesFunc: func(ctx context.Context, params opencodeapi.SessionMessagesParams) (opencodeapi.SessionMessagesRes, error) {
			var resp opencodeapi.SessionMessagesOKApplicationJSON
			return &resp, nil
		},
	}
	client := setupTestServer(t, handler)
	_, err := client.Messages(t.Context(), Location{}, "ses-1")
	require.NoError(t, err)
}

func TestClient_Context(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2SessionContextFunc: func(ctx context.Context, params opencodeapi.V2SessionContextParams) (opencodeapi.V2SessionContextRes, error) {
			return &opencodeapi.V2SessionContextOK{
				Data: []opencodeapi.SessionMessage{},
			}, nil
		},
	}
	client := setupTestServer(t, handler)
	_, err := client.Context(t.Context(), Location{}, "ses-1")
	require.NoError(t, err)
}

func TestClient_Permissions(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2PermissionRequestListFunc: func(ctx context.Context, params opencodeapi.V2PermissionRequestListParams) (opencodeapi.V2PermissionRequestListRes, error) {
			return &opencodeapi.V2PermissionRequestListOK{
				Data: []opencodeapi.PermissionV2Request{
					{
						ID:        "per-1",
						Action:    "read_file",
						SessionID: "ses-1",
						Resources: []string{},
					},
				},
			}, nil
		},
	}
	client := setupTestServer(t, handler)
	res, err := client.Permissions(t.Context(), Location{}, "ses-1")
	require.NoError(t, err)
	require.Contains(t, string(res), `"per-1"`)
}

func TestClient_PermissionReply(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2SessionPermissionReplyFunc: func(ctx context.Context, req *opencodeapi.V2SessionPermissionReplyReq, params opencodeapi.V2SessionPermissionReplyParams) (opencodeapi.V2SessionPermissionReplyRes, error) {
			return &opencodeapi.V2SessionPermissionReplyNoContent{}, nil
		},
	}
	client := setupTestServer(t, handler)
	_, err := client.PermissionReply(t.Context(), Location{}, "ses-1", "per-1", "once", "ok")
	require.NoError(t, err)
}

func TestClient_Questions(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2QuestionRequestListFunc: func(ctx context.Context, params opencodeapi.V2QuestionRequestListParams) (opencodeapi.V2QuestionRequestListRes, error) {
			return &opencodeapi.V2QuestionRequestListOK{
				Data: []opencodeapi.QuestionV2Request{
					{
						ID:        "que-1",
						SessionID: "ses-1",
						Questions: []opencodeapi.QuestionV2Info{},
					},
				},
			}, nil
		},
	}
	client := setupTestServer(t, handler)
	res, err := client.Questions(t.Context(), Location{}, "ses-1")
	require.NoError(t, err)
	require.Contains(t, string(res), `"que-1"`)
}

func TestClient_QuestionReply(t *testing.T) {
	t.Parallel()

	var rejected, replied bool
	handler := &HandlerMock{
		V2SessionQuestionRejectFunc: func(ctx context.Context, params opencodeapi.V2SessionQuestionRejectParams) (opencodeapi.V2SessionQuestionRejectRes, error) {
			rejected = true
			return &opencodeapi.V2SessionQuestionRejectNoContent{}, nil
		},
		V2SessionQuestionReplyFunc: func(ctx context.Context, req *opencodeapi.QuestionV2Reply, params opencodeapi.V2SessionQuestionReplyParams) (opencodeapi.V2SessionQuestionReplyRes, error) {
			replied = true
			return &opencodeapi.V2SessionQuestionReplyNoContent{}, nil
		},
	}
	client := setupTestServer(t, handler)

	_, err := client.QuestionReply(t.Context(), Location{}, "ses-1", "que-1", true, nil)
	require.NoError(t, err)
	require.True(t, rejected)

	_, err = client.QuestionReply(t.Context(), Location{}, "ses-1", "que-1", false, [][]string{{"ans1"}})
	require.NoError(t, err)
	require.True(t, replied)
}

func TestClient_SessionPermissionRequests(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2SessionPermissionListFunc: func(ctx context.Context, params opencodeapi.V2SessionPermissionListParams) (opencodeapi.V2SessionPermissionListRes, error) {
			return &opencodeapi.V2SessionPermissionListOK{
				Data: []opencodeapi.PermissionV2Request{
					{
						ID:        "per-1",
						Action:    "write_file",
						SessionID: "ses-1",
						Resources: []string{},
					},
				},
			}, nil
		},
	}
	client := setupTestServer(t, handler)
	res, err := client.SessionPermissionRequests(t.Context(), Location{}, "ses-1")
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, "per-1", res[0]["id"])
}

func TestClient_SessionQuestionRequests(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2QuestionRequestListFunc: func(ctx context.Context, params opencodeapi.V2QuestionRequestListParams) (opencodeapi.V2QuestionRequestListRes, error) {
			return &opencodeapi.V2QuestionRequestListOK{
				Data: []opencodeapi.QuestionV2Request{
					{
						ID:        "que-1",
						SessionID: "ses-1",
						Questions: []opencodeapi.QuestionV2Info{},
					},
				},
			}, nil
		},
	}
	client := setupTestServer(t, handler)
	res, err := client.SessionQuestionRequests(t.Context(), Location{}, "ses-1")
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, "que-1", res[0]["id"])
}

func TestClient_Wait(t *testing.T) {
	t.Parallel()

	handler := &HandlerMock{
		V2SessionWaitFunc: func(ctx context.Context, params opencodeapi.V2SessionWaitParams) (opencodeapi.V2SessionWaitRes, error) {
			return &opencodeapi.V2SessionWaitNoContent{}, nil
		},
	}
	client := setupTestServer(t, handler)
	err := client.Wait(t.Context(), Location{}, "ses-1")
	require.NoError(t, err)
}
