package opencode

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ogen-go/ogen/validate"

	"github.com/go-faster/jx"

	api "github.com/go-faster/gooners/internal/tools/opencode/opencodeapi"
)

const defaultBaseURL = "http://localhost:4096"

// Client calls opencode HTTP API endpoints via the generated client.
type Client struct {
	apiClient        *api.Client
	syncTimeout      time.Duration
	baseURL          string
	defaultDirectory string
}

// NewClient creates an opencode API client.
func NewClient(cfg Config, timeout time.Duration) (*Client, error) {
	baseURL := cmp.Or(strings.TrimSpace(cfg.BaseURL), defaultBaseURL)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	newHTTPClient := func(t time.Duration) *http.Client {
		c := &http.Client{Timeout: t}
		var base http.RoundTripper
		base = http.DefaultTransport.(*http.Transport).Clone()
		if cfg.Username != "" || cfg.Password != "" {
			base = &basicAuthTransport{
				username: cfg.Username,
				password: cfg.Password,
				base:     base,
			}
		}
		if cfg.APILogger != nil {
			base = &loggingTransport{base: base, logger: cfg.APILogger}
		}
		c.Transport = base
		return c
	}

	syncTimeout := cmp.Or(cfg.SyncTimeout, timeout)
	httpClient := newHTTPClient(timeout)
	apiClient, err := api.NewClient(baseURL, api.WithClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create API client: %w", err)
	}

	return &Client{
		apiClient:        apiClient,
		syncTimeout:      syncTimeout,
		baseURL:          strings.TrimRight(baseURL, "/"),
		defaultDirectory: cfg.DefaultDirectory,
	}, nil
}

func (c *Client) dir(loc Location) string {
	return cmp.Or(loc.Directory, c.defaultDirectory)
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) SyncTimeout() time.Duration {
	return c.syncTimeout
}

func (c *Client) Health(ctx context.Context) (json.RawMessage, error) {
	res, err := c.apiClient.V2HealthGet(ctx)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2HealthGetOK:
		raw, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	default:
		return nil, fmt.Errorf("unexpected health response type: %T", res)
	}
}

func (c *Client) Agents(ctx context.Context, loc Location) ([]Agent, error) {
	var params api.V2AgentListParams
	var listLoc api.V2AgentListLocation
	if dir := c.dir(loc); dir != "" {
		listLoc.Directory = api.NewOptString(dir)
	}
	if loc.Workspace != "" {
		listLoc.Workspace = api.NewOptString(loc.Workspace)
	}
	if listLoc.Directory.Set || listLoc.Workspace.Set {
		params.Location = api.NewOptV2AgentListLocation(listLoc)
	}

	res, err := c.apiClient.V2AgentList(ctx, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2AgentListOK:
		agents := make([]Agent, 0, len(r.Data))
		for _, a := range r.Data {
			agents = append(agents, Agent{
				Name:        a.ID,
				Description: a.Description.Value,
				Mode:        string(a.Mode),
			})
		}
		slices.SortFunc(agents, func(a, b Agent) int { return cmp.Compare(a.Name, b.Name) })
		return agents, nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	default:
		return nil, fmt.Errorf("unexpected agent list response: %T", res)
	}
}

func (c *Client) ProvidersAndModels(ctx context.Context, loc Location) (ModelsResult, error) {
	res, ok, err := c.apiProvidersAndModels(ctx, loc)
	if err != nil {
		return ModelsResult{}, err
	}
	if ok {
		return res, nil
	}
	return ModelsResult{}, fmt.Errorf("failed to retrieve providers and models")
}

func (c *Client) apiProvidersAndModels(ctx context.Context, loc Location) (ModelsResult, bool, error) {
	var (
		providerParams api.V2ProviderListParams
		providerLoc    api.V2ProviderListLocation
	)
	if dir := c.dir(loc); dir != "" {
		providerLoc.Directory = api.NewOptString(dir)
	}
	if loc.Workspace != "" {
		providerLoc.Workspace = api.NewOptString(loc.Workspace)
	}
	if providerLoc.Directory.Set || providerLoc.Workspace.Set {
		providerParams.Location = api.NewOptV2ProviderListLocation(providerLoc)
	}

	providerRes, err := c.apiClient.V2ProviderList(ctx, providerParams)
	if err != nil {
		var statusErr *validate.UnexpectedStatusCodeError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return ModelsResult{}, false, nil
		}
		return ModelsResult{}, false, err
	}

	var providerList *api.V2ProviderListOK
	switch r := providerRes.(type) {
	case *api.V2ProviderListOK:
		providerList = r
	case *api.InvalidRequestError:
		return ModelsResult{}, false, fmt.Errorf("invalid provider list request: %s", r.Message)
	case *api.UnauthorizedError:
		return ModelsResult{}, false, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.ServiceUnavailableError:
		return ModelsResult{}, false, fmt.Errorf("service unavailable: %s", r.Message)
	default:
		return ModelsResult{}, false, fmt.Errorf("unexpected provider list response: %T", providerRes)
	}

	var modelParams api.V2ModelListParams
	var modelLoc api.V2ModelListLocation
	if dir := c.dir(loc); dir != "" {
		modelLoc.Directory = api.NewOptString(dir)
	}
	if loc.Workspace != "" {
		modelLoc.Workspace = api.NewOptString(loc.Workspace)
	}
	if modelLoc.Directory.Set || modelLoc.Workspace.Set {
		modelParams.Location = api.NewOptV2ModelListLocation(modelLoc)
	}

	modelRes, err := c.apiClient.V2ModelList(ctx, modelParams)
	if err != nil {
		return ModelsResult{}, false, err
	}

	var modelList *api.V2ModelListOK
	switch r := modelRes.(type) {
	case *api.V2ModelListOK:
		modelList = r
	case *api.InvalidRequestError:
		return ModelsResult{}, false, fmt.Errorf("invalid model list request: %s", r.Message)
	case *api.UnauthorizedError:
		return ModelsResult{}, false, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.ServiceUnavailableError:
		return ModelsResult{}, false, fmt.Errorf("service unavailable: %s", r.Message)
	default:
		return ModelsResult{}, false, fmt.Errorf("unexpected model list response: %T", modelRes)
	}

	var providers []ProviderSummary
	for _, p := range providerList.Data {
		modelCount := 0
		for _, m := range modelList.Data {
			if m.ProviderID == p.ID {
				modelCount++
			}
		}
		providers = append(providers, ProviderSummary{
			ID:     p.ID,
			Name:   p.Name,
			Models: modelCount,
		})
	}

	var models []ModelSummary
	for _, m := range modelList.Data {
		models = append(models, ModelSummary{
			ProviderID: m.ProviderID,
			ID:         m.ID,
			Name:       m.Name,
		})
	}

	return sortModelsResult(ModelsResult{Providers: providers, Models: models}), true, nil
}

func sortModelsResult(res ModelsResult) ModelsResult {
	slices.SortFunc(res.Providers, func(a, b ProviderSummary) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(res.Models, func(a, b ModelSummary) int {
		if n := cmp.Compare(a.ProviderID, b.ProviderID); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return res
}

func (c *Client) Sessions(ctx context.Context, req SessionsRequest) (SessionsResult, error) {
	var params api.V2SessionListParams
	dir := cmp.Or(req.Directory, c.defaultDirectory)
	if dir != "" {
		params.Directory = api.NewOptString(dir)
	}

	res, err := c.apiClient.V2SessionList(ctx, params)
	if err != nil {
		return SessionsResult{}, err
	}
	switch r := res.(type) {
	case *api.SessionsResponse:
		sessions := make([]Session, 0, len(r.Data))
		for _, s := range r.Data {
			sessions = append(sessions, sessionFromSessionV2(s))
		}
		return SessionsResult{Sessions: sessions}, nil
	case *api.UnauthorizedError:
		return SessionsResult{}, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.V2SessionListBadRequestApplicationJSON:
		return SessionsResult{}, fmt.Errorf("bad request: %s", string(*r))
	default:
		return SessionsResult{}, fmt.Errorf("unexpected sessions response: %T", res)
	}
}

func (c *Client) CreateSession(ctx context.Context, loc Location, req CreateSessionRequest) (Session, error) {
	var body api.SessionCreateReq
	if req.Title != "" {
		body.Title.SetTo(req.Title)
	}
	if req.ParentID != "" {
		body.ParentID.SetTo(req.ParentID)
	}
	if req.ProviderID != "" || req.ModelID != "" {
		body.Model.SetTo(api.SessionCreateReqModel{
			ProviderID: req.ProviderID,
			ID:         req.ModelID,
		})
	}
	if loc.Workspace != "" {
		body.WorkspaceID.SetTo(loc.Workspace)
	}

	var params api.SessionCreateParams
	if dir := c.dir(loc); dir != "" {
		params.Directory = api.NewOptString(dir)
		params.Directory.SetTo(dir)
	}
	if loc.Workspace != "" {
		params.Workspace.SetTo(loc.Workspace)
	}

	res, err := c.apiClient.SessionCreate(ctx, api.NewOptSessionCreateReq(body), params)
	if err != nil {
		return Session{}, fmt.Errorf("POST /session: %w", err)
	}
	switch r := res.(type) {
	case *api.Session:
		return sessionFromSessionV1(*r), nil
	case *api.SessionCreateBadRequestApplicationJSON:
		return Session{}, fmt.Errorf("POST /session bad request: %s", string(*r))
	default:
		return Session{}, fmt.Errorf("unexpected SessionCreate response: %T", res)
	}
}

func (c *Client) Prompt(ctx context.Context, loc Location, sessionID string, req PromptRequest) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	textPart := api.SessionMessageReqPartsItem{}
	textPart.SetType("text")
	textPart.SetText(api.NewOptString(req.Prompt.Text))

	body := api.SessionMessageReq{
		Parts: []api.SessionMessageReqPartsItem{textPart},
	}
	if req.Agent != "" {
		body.SetAgent(api.NewOptString(req.Agent))
	}

	params := api.SessionMessageParams{
		SessionID: sessionID,
	}
	if dir := c.dir(loc); dir != "" {
		params.Directory = api.NewOptString(dir)
	}

	res, err := c.apiClient.SessionMessage(ctx, &body, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.SessionMessageOK:
		raw, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.ConflictError:
		return nil, fmt.Errorf("conflict: %s", r.Message)
	case *api.SessionMessageBadRequestApplicationJSON:
		return nil, fmt.Errorf("bad request: %s", string(*r))
	case *api.SessionMessageNotFoundApplicationJSON:
		return nil, fmt.Errorf("session not found: %s", string(*r))
	default:
		return nil, fmt.Errorf("unexpected session message response: %T", res)
	}
}

// Messages returns the raw message array for a session using the v1
// GET /session/{id}/message endpoint. The v2 /api/session/{id}/message
// endpoint returns only event stream items (e.g. agent-switched) and does
// not include user/assistant message text.
func (c *Client) Messages(ctx context.Context, _ Location, sessionID string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	var params api.SessionMessagesParams
	params.SessionID = sessionID

	res, err := c.apiClient.SessionMessages(ctx, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.SessionMessagesOKApplicationJSON:
		raw, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.SessionMessagesBadRequestApplicationJSON:
		return nil, fmt.Errorf("bad request: %s", string(*r))
	case *api.SessionMessagesNotFoundApplicationJSON:
		return nil, fmt.Errorf("session not found: %s", string(*r))
	default:
		return nil, fmt.Errorf("unexpected session messages response: %T", res)
	}
}

func (c *Client) Context(ctx context.Context, _ Location, sessionID string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	var params api.V2SessionContextParams
	params.SessionID = sessionID

	res, err := c.apiClient.V2SessionContext(ctx, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2SessionContextOK:
		raw, err := json.Marshal(r.Data)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.UnknownError1:
		return nil, fmt.Errorf("unknown error: %s", r.Message)
	case *api.V2SessionContextNotFoundApplicationJSON:
		return nil, fmt.Errorf("session context not found: %s", string(*r))
	default:
		return nil, fmt.Errorf("unexpected session context response: %T", res)
	}
}

func (c *Client) Permissions(ctx context.Context, loc Location, _ string) (json.RawMessage, error) {
	var params api.V2PermissionRequestListParams
	var listLoc api.V2PermissionRequestListLocation
	if dir := c.dir(loc); dir != "" {
		listLoc.Directory = api.NewOptString(dir)
	}
	if loc.Workspace != "" {
		listLoc.Workspace = api.NewOptString(loc.Workspace)
	}
	if listLoc.Directory.Set || listLoc.Workspace.Set {
		params.Location = api.NewOptV2PermissionRequestListLocation(listLoc)
	}

	res, err := c.apiClient.V2PermissionRequestList(ctx, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2PermissionRequestListOK:
		raw, err := json.Marshal(r.Data)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	default:
		return nil, fmt.Errorf("unexpected permission requests response: %T", res)
	}
}

func (c *Client) PermissionReply(ctx context.Context, _ Location, sessionID, requestID, reply, message string) (json.RawMessage, error) {
	if requestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	var body api.V2SessionPermissionReplyReq
	body.Reply = api.PermissionV2Reply(reply)
	if message != "" {
		body.Message = api.NewOptString(message)
	}

	var params api.V2SessionPermissionReplyParams
	params.SessionID = sessionID
	params.RequestID = requestID

	res, err := c.apiClient.V2SessionPermissionReply(ctx, &body, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2SessionPermissionReplyNoContent:
		return json.RawMessage("{}"), nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.V2SessionPermissionReplyNotFoundApplicationJSON:
		return nil, fmt.Errorf("permission request not found: %s", string(*r))
	default:
		return nil, fmt.Errorf("unexpected permission reply response: %T", res)
	}
}

func (c *Client) Questions(ctx context.Context, loc Location, _ string) (json.RawMessage, error) {
	var params api.V2QuestionRequestListParams
	var listLoc api.V2QuestionRequestListLocation
	if dir := c.dir(loc); dir != "" {
		listLoc.Directory = api.NewOptString(dir)
	}
	if loc.Workspace != "" {
		listLoc.Workspace = api.NewOptString(loc.Workspace)
	}
	if listLoc.Directory.Set || listLoc.Workspace.Set {
		params.Location = api.NewOptV2QuestionRequestListLocation(listLoc)
	}

	res, err := c.apiClient.V2QuestionRequestList(ctx, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2QuestionRequestListOK:
		raw, err := json.Marshal(r.Data)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	default:
		return nil, fmt.Errorf("unexpected question requests response: %T", res)
	}
}

func (c *Client) QuestionReply(ctx context.Context, _ Location, sessionID, requestID string, reject bool, answers [][]string) (json.RawMessage, error) {
	if requestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	if reject {
		var params api.V2SessionQuestionRejectParams
		params.SessionID = sessionID
		params.RequestID = requestID

		res, err := c.apiClient.V2SessionQuestionReject(ctx, params)
		if err != nil {
			return nil, err
		}
		switch r := res.(type) {
		case *api.V2SessionQuestionRejectNoContent:
			return json.RawMessage("{}"), nil
		case *api.InvalidRequestError:
			return nil, fmt.Errorf("invalid request: %s", r.Message)
		case *api.UnauthorizedError:
			return nil, fmt.Errorf("unauthorized: %s", r.Message)
		case *api.V2SessionQuestionRejectNotFoundApplicationJSON:
			return nil, fmt.Errorf("question request not found: %s", string(*r))
		default:
			return nil, fmt.Errorf("unexpected question reject response: %T", res)
		}
	}

	var answersList []api.QuestionV2Answer
	for _, ans := range answers {
		answersList = append(answersList, api.QuestionV2Answer(ans))
	}
	reqBody := api.QuestionV2Reply{Answers: answersList}

	var params api.V2SessionQuestionReplyParams
	params.SessionID = sessionID
	params.RequestID = requestID

	res, err := c.apiClient.V2SessionQuestionReply(ctx, &reqBody, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2SessionQuestionReplyNoContent:
		return json.RawMessage("{}"), nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.V2SessionQuestionReplyNotFoundApplicationJSON:
		return nil, fmt.Errorf("question request not found: %s", string(*r))
	default:
		return nil, fmt.Errorf("unexpected question reply response: %T", res)
	}
}

func (c *Client) SessionPermissionRequests(ctx context.Context, _ Location, sessionID string) ([]map[string]any, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("sessionID is required")
	}
	var params api.V2SessionPermissionListParams
	params.SessionID = sessionID

	res, err := c.apiClient.V2SessionPermissionList(ctx, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2SessionPermissionListOK:
		raw, err := json.Marshal(r.Data)
		if err != nil {
			return nil, err
		}
		var list []map[string]any
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		return list, nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	case *api.V2SessionPermissionListNotFoundApplicationJSON:
		return nil, fmt.Errorf("session not found: %s", string(*r))
	default:
		return nil, fmt.Errorf("unexpected session permission list response: %T", res)
	}
}

func (c *Client) SessionQuestionRequests(ctx context.Context, loc Location, sessionID string) ([]map[string]any, error) {
	var params api.V2QuestionRequestListParams
	var listLoc api.V2QuestionRequestListLocation
	if dir := c.dir(loc); dir != "" {
		listLoc.Directory = api.NewOptString(dir)
	}
	if loc.Workspace != "" {
		listLoc.Workspace = api.NewOptString(loc.Workspace)
	}
	if listLoc.Directory.Set || listLoc.Workspace.Set {
		params.Location = api.NewOptV2QuestionRequestListLocation(listLoc)
	}

	res, err := c.apiClient.V2QuestionRequestList(ctx, params)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *api.V2QuestionRequestListOK:
		raw, err := json.Marshal(r.Data)
		if err != nil {
			return nil, err
		}
		var list []map[string]any
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		var filtered []map[string]any
		for _, q := range list {
			if q["sessionID"] == sessionID || q["session_id"] == sessionID {
				filtered = append(filtered, q)
			}
		}
		return filtered, nil
	case *api.InvalidRequestError:
		return nil, fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return nil, fmt.Errorf("unauthorized: %s", r.Message)
	default:
		return nil, fmt.Errorf("unexpected question request list response: %T", res)
	}
}

func (c *Client) Wait(ctx context.Context, _ Location, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}

	var params api.V2SessionWaitParams
	params.SessionID = sessionID

	res, err := c.apiClient.V2SessionWait(ctx, params)
	if err != nil {
		return err
	}
	switch r := res.(type) {
	case *api.V2SessionWaitNoContent:
		return nil
	case *api.InvalidRequestError:
		return fmt.Errorf("invalid request: %s", r.Message)
	case *api.UnauthorizedError:
		return fmt.Errorf("unauthorized: %s", r.Message)
	case *api.ServiceUnavailableError:
		return fmt.Errorf("service unavailable: %s", r.Message)
	case *api.V2SessionWaitNotFoundApplicationJSON:
		return fmt.Errorf("session not found: %s", string(*r))
	default:
		return fmt.Errorf("unexpected session wait response: %T", res)
	}
}

func sessionFromSessionV2(s api.SessionV2Info) Session {
	var e jx.Encoder
	s.Encode(&e)
	return Session{
		ID:        s.ID,
		Title:     s.Title,
		ParentID:  s.ParentID.Value,
		CreatedAt: int64(s.Time.Created.Or(-1)),
		UpdatedAt: int64(s.Time.Updated.Or(-1)),
		Raw:       e.Bytes(),
	}
}

func sessionFromSessionV1(s api.Session) Session {
	var e jx.Encoder
	s.Encode(&e)
	return Session{
		ID:        s.ID,
		Title:     s.Title,
		ParentID:  s.ParentID.Value,
		CreatedAt: int64(s.Time.Created),
		UpdatedAt: int64(s.Time.Updated),
		Raw:       e.Bytes(),
	}
}

type basicAuthTransport struct {
	username, password string
	base               http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(t.username, t.password)
	return t.base.RoundTrip(r)
}

// loggingTransport logs every outgoing HTTP request and its response body at
// debug level. It is intended for debugging opencode API interactions.
type loggingTransport struct {
	base   http.RoundTripper
	logger *slog.Logger
}

func (t *loggingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var reqSnippet string
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(data))

		snippetLen := min(len(data), 512)
		snippet := slices.Clip(data[:snippetLen])
		if len(data) > snippetLen {
			snippet = append(snippet, "..."...)
		}
		reqSnippet = string(snippet)
	}
	t.logger.DebugContext(r.Context(), "opencode API request",
		"method", r.Method,
		"url", r.URL.String(),
		"body", reqSnippet,
	)

	resp, err := t.base.RoundTrip(r)
	if err != nil {
		t.logger.DebugContext(r.Context(), "opencode API error",
			"method", r.Method, "url", r.URL.String(), "err", err)
		return nil, err
	}
	oldBody := resp.Body
	defer func() {
		_ = oldBody.Close()
	}()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))

	snippetLen := min(len(data), 1024)
	snippet := slices.Clip(data[:snippetLen])
	if len(data) > snippetLen {
		snippet = append(snippet, "..."...)
	}

	t.logger.DebugContext(r.Context(), "opencode API response",
		"method", r.Method,
		"url", r.URL.String(),
		"status", resp.StatusCode,
		"body", string(snippet),
	)
	return resp, nil
}
