// Package grafana registers MCP tools to build, verify, and save Grafana dashboards.
//
//nolint:modernize // False positives on stringPtr which is not new(string)
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/cog/variants"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/gauge"
	"github.com/grafana/grafana-foundation-sdk/go/prometheus"
	"github.com/grafana/grafana-foundation-sdk/go/stat"
	"github.com/grafana/grafana-foundation-sdk/go/table"
	"github.com/grafana/grafana-foundation-sdk/go/timeseries"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// Session specs and models.

type DashboardSession struct {
	DashboardID string         `json:"dashboard_id"`
	Title       string         `json:"title"`
	UID         string         `json:"uid,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	TimeFrom    string         `json:"time_from,omitempty"`
	TimeTo      string         `json:"time_to,omitempty"`
	Variables   []VariableSpec `json:"variables,omitempty"`
	Rows        []*RowEntry    `json:"rows,omitempty"`
	Panels      []*PanelEntry  `json:"panels,omitempty"`
	NextY       uint32         `json:"next_y"`
	CreatedAt   time.Time      `json:"created_at"`
	TouchedAt   time.Time      `json:"touched_at"`
}

type VariableSpec struct {
	Name           string `json:"name"`
	Type           string `json:"type"` // "query", "custom", "datasource", etc.
	Query          string `json:"query,omitempty"`
	DatasourceUID  string `json:"datasource_uid,omitempty"`
	DatasourceType string `json:"datasource_type,omitempty"`
}

type RowEntry struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	Collapsed bool          `json:"collapsed"`
	Panels    []*PanelEntry `json:"panels,omitempty"`
}

type PanelEntry struct {
	ID          string                `json:"id"`
	Title       string                `json:"title"`
	Description string                `json:"description,omitempty"`
	Type        string                `json:"type"` // "timeseries", "stat", "gauge", "table", etc.
	GridPos     dashboard.GridPos     `json:"grid_pos"`
	Unit        string                `json:"unit,omitempty"`
	Decimals    *float64              `json:"decimals,omitempty"`
	Queries     []QueryEntry          `json:"queries,omitempty"`
	Thresholds  []dashboard.Threshold `json:"thresholds,omitempty"`
}

type QueryEntry struct {
	RefID          string `json:"ref_id"`
	DatasourceUID  string `json:"datasource_uid"`
	DatasourceType string `json:"datasource_type"`
	Expr           string `json:"expr"`
	LegendFormat   string `json:"legend_format,omitempty"`
}

func (s *DashboardSession) findPanel(panelID string) (*PanelEntry, *RowEntry, int) {
	for i, p := range s.Panels {
		if p.ID == panelID {
			return p, nil, i
		}
	}
	for _, r := range s.Rows {
		for i, p := range r.Panels {
			if p.ID == panelID {
				return p, r, i
			}
		}
	}
	return nil, nil, -1
}

// SessionManager manages active dashboard builder sessions.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*DashboardSession
	dir      string
}

func NewSessionManager(dir string) *SessionManager {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create session dir %q: %v\n", dir, err)
	}
	m := &SessionManager{
		sessions: make(map[string]*DashboardSession),
		dir:      dir,
	}
	m.loadAll()
	return m
}

func (m *SessionManager) Add(s *DashboardSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.DashboardID] = s
	m.save(s)
}

func (m *SessionManager) Get(id string) (*DashboardSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	s.TouchedAt = time.Now()
	m.save(s)
	return s, nil
}

func (m *SessionManager) List() []*DashboardSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]*DashboardSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		res = append(res, s)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].TouchedAt.After(res[j].TouchedAt)
	})
	return res
}

func (m *SessionManager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	_ = os.Remove(filepath.Join(m.dir, id+".json"))
}

func (m *SessionManager) save(s *DashboardSession) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(m.dir, s.DashboardID+".json"), data, 0o600)
}

func (m *SessionManager) loadAll() {
	files, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".json" {
			data, err := os.ReadFile(filepath.Join(m.dir, f.Name()))
			if err != nil {
				continue
			}
			var s DashboardSession
			if err := json.Unmarshal(data, &s); err == nil {
				m.sessions[s.DashboardID] = &s
			}
		}
	}
}

func (m *SessionManager) StartCleanupLoop(ctx context.Context, ttl time.Duration) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			now := time.Now()
			for id, s := range m.sessions {
				if now.Sub(s.TouchedAt) > ttl {
					delete(m.sessions, id)
					_ = os.Remove(filepath.Join(m.dir, id+".json"))
				}
			}
			m.mu.Unlock()
		}
	}
}

// GrafanaClient calls Grafana API endpoints.
type GrafanaClient struct {
	URL        string
	Token      string
	User       string
	Password   string
	httpClient *http.Client
}

func NewGrafanaClient(urlStr, token, user, password string) *GrafanaClient {
	return &GrafanaClient{
		URL:      urlStr,
		Token:    token,
		User:     user,
		Password: password,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (c *GrafanaClient) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if c.URL == "" {
		return nil, fmt.Errorf("grafana base URL is not configured")
	}
	baseURL := strings.TrimSuffix(c.URL, "/")
	reqURL := baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	} else if c.User != "" || c.Password != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.httpClient.Do(req)
}

func (c *GrafanaClient) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *GrafanaClient) getRaw(ctx context.Context, path string) (string, error) {
	resp, err := c.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return string(bodyBytes), nil
}

type DatasourceInfo struct {
	UID  string `json:"uid"`
	Type string `json:"type"`
	Name string `json:"name"`
}

func (c *GrafanaClient) GetDatasourceByUID(ctx context.Context, uid string) (*DatasourceInfo, error) {
	var info DatasourceInfo
	err := c.getJSON(ctx, "/api/datasources/uid/"+uid, &info)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *GrafanaClient) ResolveDatasource(ctx context.Context, name string) (*DatasourceInfo, error) {
	var info DatasourceInfo
	err := c.getJSON(ctx, "/api/datasources/name/"+url.PathEscape(name), &info)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

type PromLabelValuesResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

func (c *GrafanaClient) SearchMetrics(ctx context.Context, dsUID, match string) ([]string, error) {
	v := url.Values{}
	if match != "" {
		v.Add("match[]", match)
	}
	path := fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/label/__name__/values?%s", dsUID, v.Encode())
	var resp PromLabelValuesResponse
	err := c.getJSON(ctx, path, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *GrafanaClient) LookupLabels(ctx context.Context, dsUID, match string) ([]string, error) {
	v := url.Values{}
	if match != "" {
		v.Add("match[]", match)
	}
	path := fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/labels?%s", dsUID, v.Encode())
	var resp PromLabelValuesResponse
	err := c.getJSON(ctx, path, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *GrafanaClient) LookupLabelValues(ctx context.Context, dsUID, label string) ([]string, error) {
	path := fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/label/%s/values", dsUID, url.PathEscape(label))
	var resp PromLabelValuesResponse
	err := c.getJSON(ctx, path, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *GrafanaClient) LookupMetricMetadata(ctx context.Context, dsUID, metric string) (string, error) {
	v := url.Values{}
	v.Set("metric", metric)
	path := fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/metadata?%s", dsUID, v.Encode())
	return c.getRaw(ctx, path)
}

func (c *GrafanaClient) VerifyPrometheusQuery(ctx context.Context, dsUID, query, queryType string) (string, error) {
	v := url.Values{}
	v.Set("query", query)
	var path string
	if queryType == "instant" {
		v.Set("time", fmt.Sprintf("%d", time.Now().Unix()))
		path = fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/query?%s", dsUID, v.Encode())
	} else {
		now := time.Now()
		start := now.Add(-1 * time.Hour).Unix()
		end := now.Unix()
		v.Set("start", fmt.Sprintf("%d", start))
		v.Set("end", fmt.Sprintf("%d", end))
		v.Set("step", "15s")
		path = fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/query_range?%s", dsUID, v.Encode())
	}
	return c.getRaw(ctx, path)
}

func (c *GrafanaClient) VerifyLokiQuery(ctx context.Context, dsUID, query, queryType string) (string, error) {
	v := url.Values{}
	v.Set("query", query)
	var path string
	if queryType == "instant" {
		v.Set("time", fmt.Sprintf("%d", time.Now().UnixNano()))
		path = fmt.Sprintf("/api/datasources/proxy/uid/%s/loki/api/v1/query?%s", dsUID, v.Encode())
	} else {
		now := time.Now()
		start := now.Add(-1 * time.Hour).UnixNano()
		end := now.UnixNano()
		v.Set("start", fmt.Sprintf("%d", start))
		v.Set("end", fmt.Sprintf("%d", end))
		v.Set("step", "15s")
		path = fmt.Sprintf("/api/datasources/proxy/uid/%s/loki/api/v1/query_range?%s", dsUID, v.Encode())
	}
	return c.getRaw(ctx, path)
}

func (c *GrafanaClient) VerifyQuery(ctx context.Context, dsUID, query, queryType string) (string, error) {
	info, err := c.GetDatasourceByUID(ctx, dsUID)
	if err != nil {
		return "", fmt.Errorf("resolving datasource by UID: %w", err)
	}
	switch info.Type {
	case "prometheus":
		return c.VerifyPrometheusQuery(ctx, dsUID, query, queryType)
	case "loki":
		return c.VerifyLokiQuery(ctx, dsUID, query, queryType)
	default:
		return "", fmt.Errorf("unsupported datasource type: %s", info.Type)
	}
}

type SaveDashboardReq struct {
	Dashboard any    `json:"dashboard"`
	FolderUID string `json:"folderUid,omitempty"`
	Overwrite bool   `json:"overwrite"`
}

type SaveDashboardRes struct {
	ID      int64  `json:"id"`
	UID     string `json:"uid"`
	URL     string `json:"url"`
	Status  string `json:"status"`
	Version int64  `json:"version"`
}

func (c *GrafanaClient) SaveDashboard(ctx context.Context, dashboardJSON []byte, folderUID string) (*SaveDashboardRes, error) {
	var dbRaw any
	if err := json.Unmarshal(dashboardJSON, &dbRaw); err != nil {
		return nil, fmt.Errorf("parsing dashboard JSON: %w", err)
	}
	payload := SaveDashboardReq{
		Dashboard: dbRaw,
		FolderUID: folderUID,
		Overwrite: true,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.doRequest(ctx, "POST", "/api/dashboards/db", bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	var saveRes SaveDashboardRes
	if err := json.Unmarshal(bodyBytes, &saveRes); err != nil {
		return nil, err
	}
	return &saveRes, nil
}

// Tool implementation.

func Register(s *mcp.Server, sm *SessionManager, gc *GrafanaClient) {
	// 3.1 Construction Tools
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_dashboard",
		Description: "Initializes a new dashboard building session.",
	}, addDashboardHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "list_dashboard_sessions",
		Description: "Returns active dashboard_ids with their titles and timestamps.",
		Flags:       mcputil.ReadOnly,
	}, listSessionsHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_param",
		Description: "Adds a template variable/parameter to the dashboard (e.g. cluster, namespace).",
	}, addParamHandler(sm, gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "set_time_range",
		Description: "Sets the default time range for the dashboard (e.g. now-6h to now).",
	}, setTimeRangeHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_row",
		Description: "Adds a standard Grafana row for grouping panels.",
	}, addRowHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_panel",
		Description: "Adds a panel to the dashboard. For gauge and stat panels, a base threshold is automatically inserted.",
	}, addPanelHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "update_panel",
		Description: "Updates properties of an existing panel without rebuilding.",
	}, updatePanelHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "delete_panel",
		Description: "Removes a panel from the ongoing dashboard session.",
		Flags:       mcputil.Destructive,
	}, deletePanelHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_query",
		Description: "Attaches a query to an existing panel.",
	}, addQueryHandler(sm, gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "add_threshold",
		Description: "Adds a color threshold to stat/gauge panels. Base threshold is automatically created on panel creation.",
	}, addThresholdHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "get_dashboard_state",
		Description: "Returns the current in-progress structure of the dashboard.",
		Flags:       mcputil.ReadOnly,
	}, getDashboardStateHandler(sm))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "export_dashboard",
		Description: "Finalizes and compiles the dashboard. By default, this only validates the dashboard can be built. Use 'save' to push directly to Grafana, or 'output_path' to write the JSON to a local file.",
	}, exportDashboardHandler(sm, gc))

	// 3.2 Discovery & Verification Tools
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "resolve_datasource",
		Description: "Resolves a datasource name to its UID and type.",
		Flags:       mcputil.ReadOnly,
	}, resolveDatasourceHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "verify_query",
		Description: "Validates a query against the datasource.",
		Flags:       mcputil.ReadOnly,
	}, verifyQueryHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "search_metrics",
		Description: "Finds metric names matching a pattern.",
		Flags:       mcputil.ReadOnly,
	}, searchMetricsHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "lookup_labels",
		Description: "Fetches labels for a given selector/metric.",
		Flags:       mcputil.ReadOnly,
	}, lookupLabelsHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "lookup_label_values",
		Description: "Fetches available values for a specific label.",
		Flags:       mcputil.ReadOnly,
	}, lookupLabelValuesHandler(gc))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "lookup_metric_metadata",
		Description: "Returns metric type (counter/gauge/histogram) and help string.",
		Flags:       mcputil.ReadOnly,
	}, lookupMetricMetadataHandler(gc))
}

// Handler implementations

type AddDashboardReq struct {
	Name string   `json:"name" jsonschema:"The title of the dashboard"`
	UID  string   `json:"uid,omitempty" jsonschema:"Optional unique ID for the dashboard"`
	Tags []string `json:"tags,omitempty" jsonschema:"Optional tags for the dashboard"`
}

type AddDashboardRes struct {
	DashboardID string `json:"dashboard_id"`
}

func addDashboardHandler(sm *SessionManager) mcp.ToolHandlerFor[AddDashboardReq, AddDashboardRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddDashboardReq) (*mcp.CallToolResult, AddDashboardRes, error) {
		if args.Name == "" {
			return nil, AddDashboardRes{}, fmt.Errorf("name is required")
		}
		id := uuid.New().String()
		s := &DashboardSession{
			DashboardID: id,
			Title:       args.Name,
			UID:         args.UID,
			Tags:        args.Tags,
			CreatedAt:   time.Now(),
			TouchedAt:   time.Now(),
		}
		sm.Add(s)
		return nil, AddDashboardRes{DashboardID: id}, nil
	}
}

type ListSessionsRes struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
	DashboardID string    `json:"dashboard_id"`
	Title       string    `json:"title"`
	TouchedAt   time.Time `json:"touched_at"`
}

func listSessionsHandler(sm *SessionManager) mcp.ToolHandlerFor[struct{}, ListSessionsRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListSessionsRes, error) {
		sessions := sm.List()
		res := ListSessionsRes{
			Sessions: make([]SessionInfo, len(sessions)),
		}
		for i, s := range sessions {
			res.Sessions[i] = SessionInfo{
				DashboardID: s.DashboardID,
				Title:       s.Title,
				TouchedAt:   s.TouchedAt,
			}
		}
		return nil, res, nil
	}
}

type AddParamReq struct {
	DashboardID    string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Name           string `json:"name" jsonschema:"The name of the variable"`
	Type           string `json:"type" jsonschema:"The type of the variable (e.g. query, custom, datasource)"`
	Query          string `json:"query,omitempty" jsonschema:"The query expression or values"`
	DatasourceUID  string `json:"datasource_uid,omitempty" jsonschema:"Optional datasource UID"`
	DatasourceType string `json:"datasource_type,omitempty" jsonschema:"Optional datasource type"`
}

func addParamHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[AddParamReq, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args AddParamReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		dsType := args.DatasourceType
		if dsType == "" && args.DatasourceUID != "" {
			dsType = "prometheus"
			if gc != nil {
				info, err := gc.GetDatasourceByUID(ctx, args.DatasourceUID)
				if err == nil && info != nil {
					dsType = info.Type
				}
			}
		}
		s.Variables = append(s.Variables, VariableSpec{
			Name:           args.Name,
			Type:           args.Type,
			Query:          args.Query,
			DatasourceUID:  args.DatasourceUID,
			DatasourceType: dsType,
		})
		sm.Add(s)
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type SetTimeRangeReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	From        string `json:"from" jsonschema:"The start time (e.g. now-6h)"`
	To          string `json:"to" jsonschema:"The end time (e.g. now)"`
}

func setTimeRangeHandler(sm *SessionManager) mcp.ToolHandlerFor[SetTimeRangeReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args SetTimeRangeReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		s.TimeFrom = args.From
		s.TimeTo = args.To
		sm.Add(s)
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type AddRowReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Title       string `json:"title" jsonschema:"The title of the row"`
	Collapsed   bool   `json:"collapsed,omitempty" jsonschema:"Whether the row is collapsed"`
}

type AddRowRes struct {
	RowID string `json:"row_id"`
}

func addRowHandler(sm *SessionManager) mcp.ToolHandlerFor[AddRowReq, AddRowRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddRowReq) (*mcp.CallToolResult, AddRowRes, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, AddRowRes{}, err
		}
		rowID := uuid.New().String()
		s.Rows = append(s.Rows, &RowEntry{
			ID:        rowID,
			Title:     args.Title,
			Collapsed: args.Collapsed,
		})
		s.NextY++
		sm.Add(s)
		return nil, AddRowRes{RowID: rowID}, nil
	}
}

type AddPanelReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Title       string `json:"title" jsonschema:"The title of the panel"`
	Type        string `json:"type" jsonschema:"The panel type (e.g. timeseries, stat, gauge, table)"`
	RowID       string `json:"row_id,omitempty" jsonschema:"Optional row ID to group the panel under"`
	W           *int   `json:"w,omitempty" jsonschema:"Optional width (1-24)"`
	H           *int   `json:"h,omitempty" jsonschema:"Optional height"`
	X           *int   `json:"x,omitempty" jsonschema:"Optional X position (0-23)"`
	Y           *int   `json:"y,omitempty" jsonschema:"Optional Y position"`
}

type AddPanelRes struct {
	PanelID string            `json:"panel_id"`
	GridPos dashboard.GridPos `json:"grid_pos"`
}

func addPanelHandler(sm *SessionManager) mcp.ToolHandlerFor[AddPanelReq, AddPanelRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddPanelReq) (*mcp.CallToolResult, AddPanelRes, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, AddPanelRes{}, err
		}

		w := uint32(24)
		if args.W != nil {
			w = uint32(*args.W)
		}
		h := uint32(8)
		if args.H != nil {
			h = uint32(*args.H)
		}
		x := uint32(0)
		if args.X != nil {
			x = uint32(*args.X)
		}
		y := s.NextY
		if args.Y != nil {
			y = uint32(*args.Y)
		}

		gridPos := dashboard.GridPos{
			W: w,
			H: h,
			X: x,
			Y: y,
		}

		if args.RowID == "" {
			if args.Y == nil && args.H == nil {
				s.NextY += h
			} else if args.Y != nil && (*args.Y+int(h)) > int(s.NextY) {
				s.NextY = uint32(*args.Y) + h
			}
		}

		panelID := uuid.New().String()
		panel := &PanelEntry{
			ID:      panelID,
			Title:   args.Title,
			Type:    args.Type,
			GridPos: gridPos,
		}

		if args.Type == "stat" || args.Type == "gauge" {
			panel.Thresholds = []dashboard.Threshold{
				{
					Value: nil,
					Color: "green",
				},
			}
		}

		if args.RowID != "" {
			found := false
			for _, r := range s.Rows {
				if r.ID == args.RowID {
					r.Panels = append(r.Panels, panel)
					found = true
					break
				}
			}
			if !found {
				return nil, AddPanelRes{}, fmt.Errorf("row_id %s not found in dashboard", args.RowID)
			}
		} else {
			s.Panels = append(s.Panels, panel)
		}

		sm.Add(s)
		return nil, AddPanelRes{PanelID: panelID, GridPos: gridPos}, nil
	}
}

type UpdatePanelReq struct {
	DashboardID string   `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID     string   `json:"panel_id" jsonschema:"The ID of the panel"`
	Title       string   `json:"title,omitempty" jsonschema:"Optional new title"`
	Description string   `json:"description,omitempty" jsonschema:"Optional new description"`
	Unit        string   `json:"unit,omitempty" jsonschema:"Optional unit (e.g. short, percent, bytes)"`
	Decimals    *float64 `json:"decimals,omitempty" jsonschema:"Optional decimal precision"`
}

func updatePanelHandler(sm *SessionManager) mcp.ToolHandlerFor[UpdatePanelReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args UpdatePanelReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		p, _, _ := s.findPanel(args.PanelID)
		if p == nil {
			return nil, mcputil.SuccessResult{OK: false}, fmt.Errorf("panel_id %s not found", args.PanelID)
		}

		if args.Title != "" {
			p.Title = args.Title
		}
		if args.Description != "" {
			p.Description = args.Description
		}
		if args.Unit != "" {
			p.Unit = args.Unit
		}
		if args.Decimals != nil {
			p.Decimals = args.Decimals
		}
		sm.Add(s)
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type DeletePanelReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID     string `json:"panel_id" jsonschema:"The ID of the panel"`
}

func deletePanelHandler(sm *SessionManager) mcp.ToolHandlerFor[DeletePanelReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args DeletePanelReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		p, r, idx := s.findPanel(args.PanelID)
		if p == nil {
			return nil, mcputil.SuccessResult{OK: false}, fmt.Errorf("panel_id %s not found", args.PanelID)
		}

		if r != nil {
			r.Panels = append(r.Panels[:idx], r.Panels[idx+1:]...)
		} else {
			s.Panels = append(s.Panels[:idx], s.Panels[idx+1:]...)
		}
		sm.Add(s)
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type AddQueryReq struct {
	DashboardID    string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID        string `json:"panel_id" jsonschema:"The ID of the panel"`
	DatasourceUID  string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	DatasourceType string `json:"datasource_type,omitempty" jsonschema:"Optional type of the datasource (e.g. prometheus, loki)"`
	Expr           string `json:"expr" jsonschema:"The query expression"`
	LegendFormat   string `json:"legend_format,omitempty" jsonschema:"Optional legend format"`
}

type AddQueryRes struct {
	QueryRef string `json:"query_ref"`
}

func queryRefID(idx int) string {
	var s string
	for idx >= 0 {
		s = string(rune('A'+(idx%26))) + s
		idx = (idx / 26) - 1
	}
	return s
}

func addQueryHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[AddQueryReq, AddQueryRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args AddQueryReq) (*mcp.CallToolResult, AddQueryRes, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, AddQueryRes{}, err
		}
		p, _, _ := s.findPanel(args.PanelID)
		if p == nil {
			return nil, AddQueryRes{}, fmt.Errorf("panel_id %s not found", args.PanelID)
		}

		dsType := args.DatasourceType
		if dsType == "" {
			dsType = "prometheus"
			if gc != nil {
				info, err := gc.GetDatasourceByUID(ctx, args.DatasourceUID)
				if err == nil && info != nil {
					dsType = info.Type
				}
			}
		}

		refID := queryRefID(len(p.Queries))
		p.Queries = append(p.Queries, QueryEntry{
			RefID:          refID,
			DatasourceUID:  args.DatasourceUID,
			DatasourceType: dsType,
			Expr:           args.Expr,
			LegendFormat:   args.LegendFormat,
		})
		sm.Add(s)
		return nil, AddQueryRes{QueryRef: refID}, nil
	}
}

type AddThresholdReq struct {
	DashboardID string  `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	PanelID     string  `json:"panel_id" jsonschema:"The ID of the panel"`
	Value       float64 `json:"value" jsonschema:"The threshold value"`
	Color       string  `json:"color" jsonschema:"The color for the threshold"`
}

func addThresholdHandler(sm *SessionManager) mcp.ToolHandlerFor[AddThresholdReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddThresholdReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		p, _, _ := s.findPanel(args.PanelID)
		if p == nil {
			return nil, mcputil.SuccessResult{OK: false}, fmt.Errorf("panel_id %s not found", args.PanelID)
		}

		val := args.Value
		p.Thresholds = append(p.Thresholds, dashboard.Threshold{
			Value: &val,
			Color: args.Color,
		})
		sm.Add(s)
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

func getDashboardStateHandler(sm *SessionManager) mcp.ToolHandlerFor[GetDashboardStateReq, *DashboardSession] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args GetDashboardStateReq) (*mcp.CallToolResult, *DashboardSession, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, nil, err
		}
		return nil, s, nil
	}
}

type GetDashboardStateReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
}

type ExportDashboardReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Save        bool   `json:"save,omitempty" jsonschema:"If true, saves the dashboard directly to Grafana API"`
	FolderUID   string `json:"folder_uid,omitempty" jsonschema:"Optional folder UID to save the dashboard under"`
	OutputPath  string `json:"output_path,omitempty" jsonschema:"Optional file path to save the dashboard JSON to. If not absolute, it will be relative to the server's working directory."`
}

type ExportDashboardRes struct {
	Saved      bool   `json:"saved"`
	UID        string `json:"uid"`
	URL        string `json:"url,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
}

func stringPtr(s string) *string {
	return &s
}

func exportDashboardHandler(sm *SessionManager, gc *GrafanaClient) mcp.ToolHandlerFor[ExportDashboardReq, ExportDashboardRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ExportDashboardReq) (*mcp.CallToolResult, ExportDashboardRes, error) {
		s, err := sm.Get(args.DashboardID)
		if err != nil {
			return nil, ExportDashboardRes{}, err
		}

		dbBuilder := dashboard.NewDashboardBuilder(s.Title)
		if s.UID != "" {
			dbBuilder.Uid(s.UID)
		}
		if len(s.Tags) > 0 {
			dbBuilder.Tags(s.Tags)
		}
		if s.TimeFrom != "" || s.TimeTo != "" {
			from := s.TimeFrom
			if from == "" {
				from = "now-6h"
			}
			to := s.TimeTo
			if to == "" {
				to = "now"
			}
			dbBuilder.Time(from, to)
		}

		// Variables
		for _, v := range s.Variables {
			switch v.Type {
			case "query":
				vb := dashboard.NewQueryVariableBuilder(v.Name)
				if v.DatasourceUID != "" {
					dsType := v.DatasourceType
					if dsType == "" {
						dsType = "prometheus"
					}
					vb.Datasource(common.DataSourceRef{
						Uid:  stringPtr(v.DatasourceUID),
						Type: stringPtr(dsType),
					})
				}
				if v.Query != "" {
					vb.Query(dashboard.StringOrMap{String: stringPtr(v.Query)})
				}
				vb.Refresh(dashboard.VariableRefresh(1))
				dbBuilder.WithVariable(vb)

			case "custom":
				vb := dashboard.NewCustomVariableBuilder(v.Name)
				if v.Query != "" {
					vb.Values(dashboard.StringOrMap{String: stringPtr(v.Query)})
				}
				dbBuilder.WithVariable(vb)

			case "datasource":
				vb := dashboard.NewDatasourceVariableBuilder(v.Name)
				if v.Query != "" {
					vb.Type(v.Query)
				}
				dbBuilder.WithVariable(vb)
			}
		}

		// Add rows
		for _, r := range s.Rows {
			rowBuilder := dashboard.NewRowBuilder(r.Title).Collapsed(r.Collapsed)
			for _, p := range r.Panels {
				pBuilder := buildPanel(p)
				rowBuilder.WithPanel(pBuilder)
			}
			dbBuilder.WithRow(rowBuilder)
		}

		// Add top-level panels
		for _, p := range s.Panels {
			pBuilder := buildPanel(p)
			dbBuilder.WithPanel(pBuilder)
		}

		dashboardObj, err := dbBuilder.Build()
		if err != nil {
			return nil, ExportDashboardRes{}, fmt.Errorf("building dashboard: %w", err)
		}

		dashboardJSON, err := json.MarshalIndent(dashboardObj, "", "  ")
		if err != nil {
			return nil, ExportDashboardRes{}, fmt.Errorf("marshaling dashboard: %w", err)
		}

		res := ExportDashboardRes{
			UID:           s.UID,
		}

		if args.OutputPath != "" {
			if err := os.WriteFile(args.OutputPath, dashboardJSON, 0o644); err != nil {
				return nil, ExportDashboardRes{}, fmt.Errorf("writing dashboard to file: %w", err)
			}
			res.OutputPath = args.OutputPath
		}

		if args.Save {
			if gc == nil {
				return nil, ExportDashboardRes{}, fmt.Errorf("grafana client not configured, cannot save dashboard")
			}
			saveRes, err := gc.SaveDashboard(ctx, dashboardJSON, args.FolderUID)
			if err != nil {
				return nil, ExportDashboardRes{}, fmt.Errorf("saving dashboard to Grafana: %w", err)
			}
			res.Saved = true
			res.UID = saveRes.UID
			res.URL = saveRes.URL
		}

		return nil, res, nil
	}
}

func buildPanel(p *PanelEntry) cog.Builder[dashboard.Panel] {
	var targets []cog.Builder[variants.Dataquery]
	for _, q := range p.Queries {
		dq := prometheus.NewDataqueryBuilder().
			Expr(q.Expr).
			RefId(q.RefID)
		if q.DatasourceUID != "" {
			dsType := q.DatasourceType
			if dsType == "" {
				dsType = "prometheus"
			}
			dq.Datasource(common.DataSourceRef{
				Uid:  stringPtr(q.DatasourceUID),
				Type: stringPtr(dsType),
			})
		}
		if q.LegendFormat != "" {
			dq.LegendFormat(q.LegendFormat)
		}
		targets = append(targets, dq)
	}

	var thresholdsConfig cog.Builder[dashboard.ThresholdsConfig]
	if len(p.Thresholds) > 0 {
		sort.Slice(p.Thresholds, func(i, j int) bool {
			if p.Thresholds[i].Value == nil {
				return true
			}
			if p.Thresholds[j].Value == nil {
				return false
			}
			return *p.Thresholds[i].Value < *p.Thresholds[j].Value
		})
		thresholdsConfig = dashboard.NewThresholdsConfigBuilder().
			Mode(dashboard.ThresholdsModeAbsolute).
			Steps(p.Thresholds)
	}

	switch p.Type {
	case "timeseries":
		pb := timeseries.NewPanelBuilder().
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		return pb

	case "stat":
		pb := stat.NewPanelBuilder().
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		return pb

	case "gauge":
		pb := gauge.NewPanelBuilder().
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		return pb

	case "table":
		pb := table.NewPanelBuilder().
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		return pb

	default:
		pb := dashboard.NewPanelBuilder().
			Type(p.Type).
			Title(p.Title).
			Targets(targets)
		if p.Description != "" {
			pb.Description(p.Description)
		}
		if p.GridPos.H > 0 {
			pb.GridPos(p.GridPos)
		}
		if p.Unit != "" {
			pb.Unit(p.Unit)
		}
		if thresholdsConfig != nil {
			pb.Thresholds(thresholdsConfig)
		}
		if p.Decimals != nil {
			pb.Decimals(*p.Decimals)
		}
		return pb
	}
}

// Discovery & Verification Handler implementations

type ResolveDatasourceReq struct {
	Name string `json:"name" jsonschema:"The name of the datasource"`
}

type ResolveDatasourceRes struct {
	UID  string `json:"uid"`
	Type string `json:"type"`
}

func resolveDatasourceHandler(gc *GrafanaClient) mcp.ToolHandlerFor[ResolveDatasourceReq, ResolveDatasourceRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ResolveDatasourceReq) (*mcp.CallToolResult, ResolveDatasourceRes, error) {
		if gc == nil {
			return nil, ResolveDatasourceRes{}, fmt.Errorf("grafana client not configured")
		}
		info, err := gc.ResolveDatasource(ctx, args.Name)
		if err != nil {
			return nil, ResolveDatasourceRes{}, err
		}
		return nil, ResolveDatasourceRes{UID: info.UID, Type: info.Type}, nil
	}
}

type VerifyQueryReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Query         string `json:"query" jsonschema:"The query expression"`
	QueryType     string `json:"query_type,omitempty" jsonschema:"Query type: instant or range (default range)"`
}

func verifyQueryHandler(gc *GrafanaClient) mcp.ToolHandlerFor[VerifyQueryReq, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args VerifyQueryReq) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if gc == nil {
			return nil, mcputil.CommandResult{}, fmt.Errorf("grafana client not configured")
		}
		qType := args.QueryType
		if qType == "" {
			qType = "range"
		}
		res, err := gc.VerifyQuery(ctx, args.DatasourceUID, args.Query, qType)
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}
		return nil, mcputil.CommandResult{Text: res}, nil
	}
}

type SearchMetricsReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Match         string `json:"match" jsonschema:"The match pattern for metrics"`
}

type SearchMetricsRes struct {
	Metrics []string `json:"metrics"`
}

func searchMetricsHandler(gc *GrafanaClient) mcp.ToolHandlerFor[SearchMetricsReq, SearchMetricsRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args SearchMetricsReq) (*mcp.CallToolResult, SearchMetricsRes, error) {
		if gc == nil {
			return nil, SearchMetricsRes{}, fmt.Errorf("grafana client not configured")
		}
		metrics, err := gc.SearchMetrics(ctx, args.DatasourceUID, args.Match)
		if err != nil {
			return nil, SearchMetricsRes{}, err
		}
		return nil, SearchMetricsRes{Metrics: metrics}, nil
	}
}

type LookupLabelsReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Match         string `json:"match,omitempty" jsonschema:"Optional match selector e.g. {__name__=\"go_goroutines\"}"`
}

type LookupLabelsRes struct {
	Labels []string `json:"labels"`
}

func lookupLabelsHandler(gc *GrafanaClient) mcp.ToolHandlerFor[LookupLabelsReq, LookupLabelsRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args LookupLabelsReq) (*mcp.CallToolResult, LookupLabelsRes, error) {
		if gc == nil {
			return nil, LookupLabelsRes{}, fmt.Errorf("grafana client not configured")
		}
		labels, err := gc.LookupLabels(ctx, args.DatasourceUID, args.Match)
		if err != nil {
			return nil, LookupLabelsRes{}, err
		}
		return nil, LookupLabelsRes{Labels: labels}, nil
	}
}

type LookupLabelValuesReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Label         string `json:"label" jsonschema:"The label name"`
}

type LookupLabelValuesRes struct {
	Values []string `json:"values"`
}

func lookupLabelValuesHandler(gc *GrafanaClient) mcp.ToolHandlerFor[LookupLabelValuesReq, LookupLabelValuesRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args LookupLabelValuesReq) (*mcp.CallToolResult, LookupLabelValuesRes, error) {
		if gc == nil {
			return nil, LookupLabelValuesRes{}, fmt.Errorf("grafana client not configured")
		}
		values, err := gc.LookupLabelValues(ctx, args.DatasourceUID, args.Label)
		if err != nil {
			return nil, LookupLabelValuesRes{}, err
		}
		return nil, LookupLabelValuesRes{Values: values}, nil
	}
}

type LookupMetricMetadataReq struct {
	DatasourceUID string `json:"datasource_uid" jsonschema:"The UID of the datasource"`
	Metric        string `json:"metric" jsonschema:"The metric name"`
}

func lookupMetricMetadataHandler(gc *GrafanaClient) mcp.ToolHandlerFor[LookupMetricMetadataReq, mcputil.CommandResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args LookupMetricMetadataReq) (*mcp.CallToolResult, mcputil.CommandResult, error) {
		if gc == nil {
			return nil, mcputil.CommandResult{}, fmt.Errorf("grafana client not configured")
		}
		res, err := gc.LookupMetricMetadata(ctx, args.DatasourceUID, args.Metric)
		if err != nil {
			return nil, mcputil.CommandResult{}, err
		}
		return nil, mcputil.CommandResult{Text: res}, nil
	}
}
