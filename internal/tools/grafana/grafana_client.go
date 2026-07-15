package grafana

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

	"github.com/go-faster/gooners/internal/effect"
)

// GrafanaClient calls Grafana API endpoints.
type GrafanaClient struct {
	URL      string
	Token    string
	User     string
	Password string

	http effect.Doer
}

// GrafanaClientOptions configures [NewGrafanaClient].
type GrafanaClientOptions struct {
	URL      string
	Token    string
	User     string
	Password string

	// HTTP performs the requests. If nil, it is an [effect.NewHTTPClient] whose
	// egress allowlist is URL alone: a Grafana client can reach the Grafana it
	// was configured with, and nothing else.
	HTTP effect.Doer
}

func (o *GrafanaClientOptions) setDefaults() {
	if o.HTTP == nil {
		o.HTTP = effect.NewHTTPClient(effect.HTTPOptions{
			Policy:  effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(o.URL)},
			Timeout: 15 * time.Second,
		})
	}
}

func NewGrafanaClient(opts GrafanaClientOptions) *GrafanaClient {
	opts.setDefaults()
	return &GrafanaClient{
		URL:      opts.URL,
		Token:    opts.Token,
		User:     opts.User,
		Password: opts.Password,
		http:     opts.HTTP,
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
	return c.http.Do(req)
}

func (c *GrafanaClient) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			bodyBytes = fmt.Appendf(nil, "<can't read body: %s>", err)
		}
		return fmt.Errorf("got HTTP error %d: %s", resp.StatusCode, string(bodyBytes))
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
		return "", fmt.Errorf("got HTTP error %d: %s", resp.StatusCode, string(bodyBytes))
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

func (c *GrafanaClient) LookupLabelValues(ctx context.Context, dsUID, label, match string) ([]string, error) {
	v := url.Values{}
	if match != "" {
		v.Add("match[]", match)
	}
	path := fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/label/%s/values?%s", dsUID, url.PathEscape(label), v.Encode())
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

func (c *GrafanaClient) VerifyPrometheusQuery(ctx context.Context, dsUID, query, queryType string) (*QuerySummary, error) {
	v := url.Values{}
	v.Set("query", query)
	now := time.Now()
	var path string
	if queryType == "instant" {
		v.Set("time", fmt.Sprintf("%d", now.Unix()))
		path = fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/query?%s", dsUID, v.Encode())
	} else {
		start := now.Add(-1 * time.Hour).Unix()
		v.Set("start", fmt.Sprintf("%d", start))
		v.Set("end", fmt.Sprintf("%d", now.Unix()))
		v.Set("step", "15s")
		path = fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/query_range?%s", dsUID, v.Encode())
	}
	body, err := c.getRaw(ctx, path)
	if err != nil {
		return nil, err
	}
	return summarizePrometheusResponse([]byte(body), now)
}

func (c *GrafanaClient) VerifyLokiQuery(ctx context.Context, dsUID, query, queryType string) (*QuerySummary, error) {
	v := url.Values{}
	v.Set("query", query)
	now := time.Now()
	var path string
	if queryType == "instant" {
		v.Set("time", fmt.Sprintf("%d", now.UnixNano()))
		path = fmt.Sprintf("/api/datasources/proxy/uid/%s/loki/api/v1/query?%s", dsUID, v.Encode())
	} else {
		start := now.Add(-1 * time.Hour).UnixNano()
		v.Set("start", fmt.Sprintf("%d", start))
		v.Set("end", fmt.Sprintf("%d", now.UnixNano()))
		v.Set("step", "15s")
		path = fmt.Sprintf("/api/datasources/proxy/uid/%s/loki/api/v1/query_range?%s", dsUID, v.Encode())
	}
	body, err := c.getRaw(ctx, path)
	if err != nil {
		return nil, err
	}
	return summarizeLokiResponse([]byte(body), now)
}

func (c *GrafanaClient) VerifyQuery(ctx context.Context, dsUID, query, queryType string) (*QuerySummary, error) {
	info, err := c.GetDatasourceByUID(ctx, dsUID)
	if err != nil {
		return nil, fmt.Errorf("resolving datasource by UID: %w", err)
	}
	switch info.Type {
	case "prometheus":
		return c.VerifyPrometheusQuery(ctx, dsUID, query, queryType)
	case "loki":
		return c.VerifyLokiQuery(ctx, dsUID, query, queryType)
	default:
		return nil, fmt.Errorf("unsupported datasource type: %s", info.Type)
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

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("got HTTP error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	var saveRes SaveDashboardRes
	if err := json.Unmarshal(bodyBytes, &saveRes); err != nil {
		return nil, err
	}

	return &saveRes, nil
}

func (c *GrafanaClient) GetDashboardByUID(ctx context.Context, uid string) ([]byte, error) {
	var full struct {
		Dashboard json.RawMessage `json:"dashboard"`
	}
	u := &url.URL{Path: "/api/dashboards/uid/"}
	path := u.JoinPath(url.PathEscape(uid)).EscapedPath()
	if err := c.getJSON(ctx, path, &full); err != nil {
		return nil, err
	}
	return full.Dashboard, nil
}
