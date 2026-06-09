package opencode

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	opencodesdk "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

const defaultBaseURL = "http://localhost:4096"

// Client calls opencode HTTP API endpoints via the official SDK (instance routes)
// and direct HTTP calls for v2 API endpoints not covered by the SDK.
type Client struct {
	sdk              *opencodesdk.Client
	httpClient       *http.Client
	baseURL          string
	defaultDirectory string
}

// NewClient creates an opencode API client.
func NewClient(cfg Config, timeout time.Duration) (*Client, error) {
	baseURL := cmp.Or(strings.TrimSpace(cfg.BaseURL), defaultBaseURL)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	httpClient := &http.Client{Timeout: timeout}
	if cfg.Username != "" || cfg.Password != "" {
		httpClient.Transport = &basicAuthTransport{
			username: cfg.Username,
			password: cfg.Password,
			base:     http.DefaultTransport,
		}
	}

	sdk := opencodesdk.NewClient(
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
	)
	return &Client{
		sdk:              sdk,
		httpClient:       httpClient,
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

func (c *Client) Health(ctx context.Context) (json.RawMessage, error) {
	return c.v2Get(ctx, "/api/health", "")
}

func (c *Client) Agents(ctx context.Context, loc Location) ([]Agent, error) {
	params := opencodesdk.AgentListParams{}
	if dir := c.dir(loc); dir != "" {
		params.Directory = opencodesdk.F(dir)
	}
	res, err := c.sdk.Agent.List(ctx, params)
	if err != nil {
		return nil, err
	}
	agents := make([]Agent, 0, len(*res))
	for _, a := range *res {
		agents = append(agents, Agent{Name: a.Name, Description: a.Description, Mode: string(a.Mode)})
	}
	slices.SortFunc(agents, func(a, b Agent) int { return cmp.Compare(a.Name, b.Name) })
	return agents, nil
}

func (c *Client) ProvidersAndModels(ctx context.Context, loc Location) (ModelsResult, error) {
	res, err := c.appProviders(ctx, loc)
	if err != nil {
		return ModelsResult{}, err
	}
	providers := make([]ProviderSummary, 0, len(res.Providers))
	var models []ModelSummary
	for _, p := range res.Providers {
		providers = append(providers, ProviderSummary{ID: p.ID, Name: p.Name, Models: len(p.Models)})
		for id, m := range p.Models {
			models = append(models, ModelSummary{ProviderID: p.ID, ID: id, Name: m.Name})
		}
	}
	slices.SortFunc(providers, func(a, b ProviderSummary) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(models, func(a, b ModelSummary) int {
		if n := cmp.Compare(a.ProviderID, b.ProviderID); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return ModelsResult{Providers: providers, Models: models}, nil
}

func (c *Client) appProviders(ctx context.Context, loc Location) (*opencodesdk.AppProvidersResponse, error) {
	params := opencodesdk.AppProvidersParams{}
	if dir := c.dir(loc); dir != "" {
		params.Directory = opencodesdk.F(dir)
	}
	return c.sdk.App.Providers(ctx, params)
}

func (c *Client) Sessions(ctx context.Context, req SessionsRequest) (SessionsResult, error) {
	dir := cmp.Or(req.Directory, c.defaultDirectory)
	params := opencodesdk.SessionListParams{}
	if dir != "" {
		params.Directory = opencodesdk.F(dir)
	}
	res, err := c.sdk.Session.List(ctx, params)
	if err != nil {
		return SessionsResult{}, err
	}
	sessions := make([]Session, 0, len(*res))
	for _, s := range *res {
		sessions = append(sessions, sessionFromSDK(s))
	}
	return SessionsResult{Sessions: sessions}, nil
}

func (c *Client) CreateSession(ctx context.Context, loc Location, req CreateSessionRequest) (Session, error) {
	params := opencodesdk.SessionNewParams{
		Directory: opencodesdk.F(c.dir(loc)),
	}
	if req.Title != "" {
		params.Title = opencodesdk.F(req.Title)
	}
	if req.ParentID != "" {
		params.ParentID = opencodesdk.F(req.ParentID)
	}
	res, err := c.sdk.Session.New(ctx, params)
	if err != nil {
		return Session{}, fmt.Errorf("POST /session: %w", err)
	}
	return sessionFromSDK(*res), nil
}

func (c *Client) Prompt(ctx context.Context, loc Location, sessionID string, req PromptRequest) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	type textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type modelRef struct {
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
	}
	type promptBody struct {
		Parts []textPart `json:"parts"`
		Agent string     `json:"agent,omitempty"`
		Model *modelRef  `json:"model,omitempty"`
	}
	body := promptBody{
		Parts: []textPart{{Type: "text", Text: req.Prompt.Text}},
		Agent: req.Agent,
	}
	if req.Model != nil && req.Model.ModelID != "" {
		body.Model = &modelRef{
			ModelID:    req.Model.ModelID,
			ProviderID: req.Model.ProviderID,
		}
	}
	return c.instancePost(ctx, fmt.Sprintf("session/%s/message", sessionID), c.dir(loc), body)
}

func (c *Client) Messages(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	return c.instanceGet(ctx, fmt.Sprintf("session/%s/message", sessionID), c.dir(loc))
}

func (c *Client) Context(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	return c.v2Get(ctx, fmt.Sprintf("/api/session/%s/context", sessionID), c.dir(loc))
}

func (c *Client) Permissions(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if sessionID != "" {
		return c.v2Get(ctx, fmt.Sprintf("/api/session/%s/permission/request", sessionID), c.dir(loc))
	}
	return c.v2Get(ctx, "/api/permission/request", c.dir(loc))
}

func (c *Client) PermissionReply(ctx context.Context, loc Location, sessionID, requestID, reply, _ string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if requestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	res, err := c.sdk.Session.Permissions.Respond(ctx, sessionID, requestID,
		opencodesdk.SessionPermissionRespondParams{
			Response:  opencodesdk.F(opencodesdk.SessionPermissionRespondParamsResponse(reply)),
			Directory: opencodesdk.F(c.dir(loc)),
		})
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

func (c *Client) Questions(ctx context.Context, loc Location, _ string) (json.RawMessage, error) {
	return c.v2Get(ctx, "/api/question/request", c.dir(loc))
}

func (c *Client) QuestionReply(ctx context.Context, loc Location, sessionID, requestID string, reject bool, answers [][]string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if requestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	if reject {
		return c.v2Post(ctx, fmt.Sprintf("/api/session/%s/question/request/%s/reject", sessionID, requestID), c.dir(loc), nil)
	}
	type replyBody struct {
		Answers [][]string `json:"answers"`
	}
	return c.v2Post(ctx, fmt.Sprintf("/api/session/%s/question/request/%s/reply", sessionID, requestID), c.dir(loc), replyBody{Answers: answers})
}

// instanceGet makes a GET request to an instance route (no /api/ prefix).
func (c *Client) instanceGet(ctx context.Context, path, dir string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+path, http.NoBody)
	if err != nil {
		return nil, err
	}
	setDirQuery(req, dir)
	return c.doRaw(req)
}

// instancePost makes a POST request to an instance route (no /api/ prefix).
func (c *Client) instancePost(ctx context.Context, path, dir string, body any) (json.RawMessage, error) {
	return c.v2Post(ctx, "/"+path, dir, body)
}

// v2Get makes a GET request to a v2 API route (/api/...).
func (c *Client) v2Get(ctx context.Context, path, dir string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, http.NoBody)
	if err != nil {
		return nil, err
	}
	setDirQuery(req, dir)
	return c.doRaw(req)
}

// v2Post makes a POST request to a v2 API route (/api/...).
func (c *Client) v2Post(ctx context.Context, path, dir string, body any) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setDirQuery(req, dir)
	return c.doRaw(req)
}

func setDirQuery(req *http.Request, dir string) {
	if dir == "" {
		return
	}
	q := req.URL.Query()
	q.Set("directory", dir)
	req.URL.RawQuery = q.Encode()
}

func (c *Client) doRaw(req *http.Request) (json.RawMessage, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func sessionFromSDK(s opencodesdk.Session) Session {
	raw, _ := json.Marshal(s)
	return Session{
		ID:        s.ID,
		Title:     s.Title,
		ParentID:  s.ParentID,
		CreatedAt: int64(s.Time.Created),
		UpdatedAt: int64(s.Time.Updated),
		Raw:       raw,
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
