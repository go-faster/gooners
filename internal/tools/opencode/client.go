package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "http://localhost:4096"

// Client calls opencode v2 HTTP API endpoints.
type Client struct {
	baseURL          string
	username         string
	password         string
	defaultDirectory string
	httpClient       *http.Client
}

// NewClient creates an opencode API client.
func NewClient(cfg Config, timeout time.Duration) (*Client, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse opencode URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("opencode URL must include scheme and host: %q", baseURL)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &Client{
		baseURL:          strings.TrimRight(u.String(), "/"),
		username:         cfg.Username,
		password:         cfg.Password,
		defaultDirectory: cfg.DefaultDirectory,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}, nil
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) Health(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/api/health", Location{}, nil)
}

func (c *Client) Agents(ctx context.Context, loc Location) ([]Agent, error) {
	body, err := c.raw(ctx, http.MethodGet, "/api/agent", loc, nil)
	if err != nil {
		return nil, err
	}
	return parseAgents(body)
}

func (c *Client) Providers(ctx context.Context, loc Location) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/api/provider", loc, nil)
}

func (c *Client) Models(ctx context.Context, loc Location) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/api/model", loc, nil)
}

func (c *Client) Sessions(ctx context.Context, req SessionsRequest) (SessionsResult, error) {
	path := "/api/session" + sessionsQuery(req)
	body, err := c.raw(ctx, http.MethodGet, path, req.Location, nil)
	if err != nil {
		return SessionsResult{}, err
	}
	sessions, err := parseSessions(body)
	if err != nil {
		return SessionsResult{}, err
	}
	return SessionsResult{Sessions: sessions}, nil
}

func (c *Client) CreateSession(ctx context.Context, loc Location, req CreateSessionRequest) (Session, error) {
	body, err := c.raw(ctx, http.MethodPost, "/api/session", loc, req)
	if err != nil {
		return Session{}, err
	}
	sessions, err := parseSessions(body)
	if err == nil && len(sessions) > 0 {
		return sessions[0], nil
	}
	if one, ok := parseSession(body); ok {
		return one, nil
	}
	return Session{}, fmt.Errorf("opencode create session response does not contain a session id")
}

func (c *Client) Prompt(ctx context.Context, loc Location, sessionID string, req PromptRequest) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	return c.raw(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/prompt", loc, req)
}

func (c *Client) Wait(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	return c.raw(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/wait", loc, nil)
}

func (c *Client) Context(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	return c.raw(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID)+"/context", loc, nil)
}

func (c *Client) Messages(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	return c.raw(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID)+"/message", loc, nil)
}

func (c *Client) Permissions(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if sessionID != "" {
		return c.raw(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID)+"/permission", loc, nil)
	}
	return c.raw(ctx, http.MethodGet, "/api/permission/request", loc, nil)
}

func (c *Client) PermissionReply(ctx context.Context, loc Location, sessionID, requestID string, payload any) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if requestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	path := "/api/session/" + url.PathEscape(sessionID) + "/permission/" + url.PathEscape(requestID) + "/reply"
	return c.raw(ctx, http.MethodPost, path, loc, payload)
}

func (c *Client) Questions(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if sessionID != "" {
		return c.raw(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID)+"/question", loc, nil)
	}
	return c.raw(ctx, http.MethodGet, "/api/question/request", loc, nil)
}

func (c *Client) QuestionReply(ctx context.Context, loc Location, sessionID, requestID string, reject bool, payload any) (json.RawMessage, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if requestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	action := "reply"
	if reject {
		action = "reject"
	}
	path := "/api/session/" + url.PathEscape(sessionID) + "/question/" + url.PathEscape(requestID) + "/" + action
	return c.raw(ctx, http.MethodPost, path, loc, payload)
}

func (c *Client) raw(ctx context.Context, method, path string, loc Location, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.password != "" || c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	if err := c.applyLocation(req, loc); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call opencode %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read opencode response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpError(resp.StatusCode, method, path, data)
	}
	return unwrapData(data), nil
}

func (c *Client) applyLocation(req *http.Request, loc Location) error {
	directory := loc.Directory
	if directory == "" {
		directory = c.defaultDirectory
	}
	if directory != "" {
		if invalidHeaderValue(directory) {
			return fmt.Errorf("directory contains invalid header characters")
		}
		req.Header.Set("x-opencode-directory", directory)
	}
	if loc.Workspace != "" {
		if invalidHeaderValue(loc.Workspace) {
			return fmt.Errorf("workspace contains invalid header characters")
		}
		req.Header.Set("x-opencode-workspace", loc.Workspace)
	}
	return nil
}

func invalidHeaderValue(s string) bool {
	return strings.ContainsAny(s, "\x00\r\n")
}

func unwrapData(data []byte) json.RawMessage {
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Data) > 0 && string(wrapper.Data) != "null" {
		return wrapper.Data
	}
	return json.RawMessage(data)
}

func httpError(status int, method, path string, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(status)
	}
	suggestion := ""
	switch status {
	case http.StatusUnauthorized:
		suggestion = "; check OPENCODE_USERNAME/OPENCODE_PASSWORD"
	case http.StatusForbidden:
		suggestion = "; credentials were accepted but are not authorized for this opencode operation"
	case http.StatusNotFound:
		if method == http.MethodPost && path == "/api/session" {
			suggestion = "; this opencode server does not expose v2 POST /api/session yet"
		}
	case http.StatusConflict:
		suggestion = "; check whether the session id already exists or the session is currently busy"
	case http.StatusServiceUnavailable:
		suggestion = "; ensure opencode serve is running and ready"
	}
	return fmt.Errorf("opencode %s %s returned HTTP %d%s: %s", method, path, status, suggestion, msg)
}

func sessionsQuery(req SessionsRequest) string {
	values := url.Values{}
	if req.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", req.Limit))
	}
	if req.Order != "" {
		values.Set("order", req.Order)
	}
	if req.Search != "" {
		values.Set("search", req.Search)
	}
	if req.Cursor != "" {
		values.Set("cursor", req.Cursor)
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}
