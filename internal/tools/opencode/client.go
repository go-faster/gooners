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

	opencodesdk "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

const defaultBaseURL = "http://localhost:4096"

// Client calls opencode HTTP API endpoints via the official SDK (instance routes)
// and direct HTTP calls for v2 API endpoints not covered by the SDK.
type Client struct {
	sdk              *opencodesdk.Client
	httpClient       *http.Client
	syncHTTPClient   *http.Client
	syncTimeout      time.Duration
	baseURL          string
	defaultDirectory string
}

type apiModel struct {
	Name string `json:"name"`
}

type apiProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Models map[string]apiModel `json:"models"`
}

type apiProviderList struct {
	All       []apiProvider `json:"all"`
	Connected []string      `json:"connected"`
}

type apiDataProviderList struct {
	Data []apiProvider `json:"data"`
}

// NewClient creates an opencode API client.
func NewClient(cfg Config, timeout time.Duration) (*Client, error) {
	baseURL := cmp.Or(strings.TrimSpace(cfg.BaseURL), defaultBaseURL)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	newHTTPClient := func(t time.Duration) *http.Client {
		c := &http.Client{Timeout: t}
		var base http.RoundTripper = http.DefaultTransport
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
		if base != http.DefaultTransport {
			c.Transport = base
		}
		return c
	}

	httpClient := newHTTPClient(timeout)
	syncTimeout := cmp.Or(cfg.SyncTimeout, timeout)
	syncHTTPClient := httpClient
	if syncTimeout != timeout {
		syncHTTPClient = newHTTPClient(syncTimeout)
	}

	sdk := opencodesdk.NewClient(
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
	)
	return &Client{
		sdk:              sdk,
		httpClient:       httpClient,
		syncHTTPClient:   syncHTTPClient,
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
	if res, ok, err := c.apiProvidersAndModels(ctx, loc); err != nil {
		return ModelsResult{}, err
	} else if ok {
		return res, nil
	}

	res, err := c.appProviders(ctx, loc)
	if err != nil {
		return ModelsResult{}, err
	}
	return modelsResultFromProviders(res.Providers), nil
}

func (c *Client) apiProvidersAndModels(ctx context.Context, loc Location) (ModelsResult, bool, error) {
	raw, err := c.v2Get(ctx, "/api/provider", c.dir(loc))
	if err != nil {
		// 404 means the endpoint doesn't exist in this opencode version; fall back
		// to the SDK app-route path instead of propagating the error.
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return ModelsResult{}, false, nil
		}
		return ModelsResult{}, false, err
	}

	// opencode exposes two response shapes for /api/provider depending on version:
	//   {all:[...], connected:[...]} — filter to connected providers only
	//   {data:[...]}                 — all providers, no connected filter
	// If neither shape matches, return (_, false, nil) so the caller falls back
	// to the SDK app-route path.
	var list apiProviderList
	if err := json.Unmarshal(raw, &list); err == nil && (list.All != nil || list.Connected != nil) {
		connected := make(map[string]bool, len(list.Connected))
		for _, id := range list.Connected {
			connected[id] = true
		}
		providers := make([]apiProvider, 0, len(list.All))
		for _, p := range list.All {
			if connected[p.ID] {
				providers = append(providers, p)
			}
		}
		return modelsResultFromAPIProviders(providers), true, nil
	}

	var dataList apiDataProviderList
	if err := json.Unmarshal(raw, &dataList); err == nil && dataList.Data != nil {
		return modelsResultFromAPIProviders(dataList.Data), true, nil
	}
	return ModelsResult{}, false, nil
}

func modelsResultFromProviders(input []opencodesdk.Provider) ModelsResult {
	converted := make([]apiProvider, 0, len(input))
	for _, p := range input {
		models := make(map[string]apiModel, len(p.Models))
		for id, m := range p.Models {
			models[id] = apiModel{Name: m.Name}
		}
		converted = append(converted, apiProvider{ID: p.ID, Name: p.Name, Models: models})
	}
	return modelsResultFromAPIProviders(converted)
}

func modelsResultFromAPIProviders(input []apiProvider) ModelsResult {
	providers := make([]ProviderSummary, 0, len(input))
	var models []ModelSummary
	for _, p := range input {
		providers = append(providers, ProviderSummary{ID: p.ID, Name: p.Name, Models: len(p.Models)})
		for id, m := range p.Models {
			models = append(models, ModelSummary{ProviderID: p.ID, ID: id, Name: m.Name})
		}
	}
	return sortModelsResult(ModelsResult{Providers: providers, Models: models})
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
	return c.syncPost(ctx, fmt.Sprintf("session/%s/message", sessionID), c.dir(loc), body)
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

// Permissions returns all pending permission requests as a flat JSON array.
// Results are not scoped server-side; the caller (summarizeRequests) filters
// by sessionID when needed.
func (c *Client) Permissions(ctx context.Context, loc Location, _ string) (json.RawMessage, error) {
	return c.instanceGet(ctx, "permission", c.dir(loc))
}

func (c *Client) PermissionReply(ctx context.Context, loc Location, _, requestID, reply, message string) (json.RawMessage, error) {
	if requestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	type body struct {
		Reply   string `json:"reply"`
		Message string `json:"message,omitempty"`
	}
	return c.instancePost(ctx, fmt.Sprintf("permission/%s/reply", requestID), c.dir(loc), body{Reply: reply, Message: message})
}

// Questions returns all pending question requests as a flat JSON array.
func (c *Client) Questions(ctx context.Context, loc Location, _ string) (json.RawMessage, error) {
	return c.instanceGet(ctx, "question", c.dir(loc))
}

func (c *Client) QuestionReply(ctx context.Context, loc Location, _, requestID string, reject bool, answers [][]string) (json.RawMessage, error) {
	if requestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	if reject {
		return c.instancePost(ctx, fmt.Sprintf("question/%s/reject", requestID), c.dir(loc), nil)
	}
	type replyBody struct {
		Answers [][]string `json:"answers"`
	}
	return c.instancePost(ctx, fmt.Sprintf("question/%s/reply", requestID), c.dir(loc), replyBody{Answers: answers})
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
	return c.doPost(ctx, c.httpClient, "/"+path, dir, body)
}

// syncPost makes a POST request to an instance route using the sync HTTP client (longer timeout).
func (c *Client) syncPost(ctx context.Context, path, dir string, body any) (json.RawMessage, error) {
	return c.doPost(ctx, c.syncHTTPClient, "/"+path, dir, body)
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

func (c *Client) doPost(ctx context.Context, hc *http.Client, path, dir string, body any) (json.RawMessage, error) {
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
	return c.doRawWith(hc, req)
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
	return c.doRawWith(c.httpClient, req)
}

// HTTPError is returned by client methods when the server responds with a
// non-2xx status code.
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("opencode API %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func (c *Client) doRawWith(hc *http.Client, req *http.Request) (json.RawMessage, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := data
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return nil, &HTTPError{
			Method:     req.Method,
			Path:       req.URL.Path,
			StatusCode: resp.StatusCode,
			Body:       string(snippet),
		}
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
		reqSnippet = string(data[:snippetLen]) + "..."
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
	snippet := slices.Concat(
		slices.Clip(data[:snippetLen]),
		[]byte("..."),
	)
	t.logger.DebugContext(r.Context(), "opencode API response",
		"method", r.Method,
		"url", r.URL.String(),
		"status", resp.StatusCode,
		"body", string(snippet),
	)
	return resp, nil
}
