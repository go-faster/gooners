package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	grafanaResourceMIMEType = "application/json"
	grafanaResourceScheme   = "grafana-dashboard"
)

func registerResources(s *mcp.Server, sm *SessionManager) {
	h := grafanaResourceHandler(sm)
	s.AddResource(&mcp.Resource{
		URI:         "grafana-dashboard://sessions",
		Name:        "dashboard-sessions",
		Title:       "Grafana dashboard sessions",
		Description: "Lists active dashboard builder sessions with IDs, titles, versions, models, and touch times.",
		MIMEType:    grafanaResourceMIMEType,
	}, h)
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "grafana-dashboard://sessions/{dashboard_id}/state",
		Name:        "dashboard-session-state",
		Title:       "Grafana dashboard session state",
		Description: "Returns the editable dashboard session state and computed layout for a dashboard_id.",
		MIMEType:    grafanaResourceMIMEType,
	}, h)
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "grafana-dashboard://sessions/{dashboard_id}/export{?version}",
		Name:        "dashboard-session-export",
		Title:       "Grafana dashboard export JSON",
		Description: "Returns compiled Grafana dashboard JSON for a dashboard_id. Optional version query may be v1 or v2; default is the session version.",
		MIMEType:    grafanaResourceMIMEType,
	}, h)
}

func grafanaResourceHandler(sm *SessionManager) mcp.ResourceHandler {
	return func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		text, err := readGrafanaResource(sm, uri)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{
					URI:      uri,
					MIMEType: grafanaResourceMIMEType,
					Text:     text,
				},
			},
		}, nil
	}
}

func readGrafanaResource(sm *SessionManager, rawURI string) (string, error) {
	u, err := url.Parse(rawURI)
	if err != nil {
		return "", fmt.Errorf("parse resource uri: %w", err)
	}
	if u.Scheme != grafanaResourceScheme {
		return "", fmt.Errorf("unsupported resource scheme %q", u.Scheme)
	}
	if u.Host != "sessions" {
		return "", fmt.Errorf("unsupported resource host %q", u.Host)
	}

	parts := splitResourcePath(u.Path)
	switch {
	case len(parts) == 0:
		return marshalResource(listDashboardSessions(sm))
	case len(parts) == 2 && parts[1] == "state":
		return marshalDashboardState(sm, parts[0])
	case len(parts) == 2 && parts[1] == "export":
		return marshalDashboardExport(sm, parts[0], u.Query().Get("version"))
	default:
		return "", fmt.Errorf("unsupported grafana-dashboard resource path %q", u.Path)
	}
}

func splitResourcePath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func listDashboardSessions(sm *SessionManager) ListSessionsRes {
	sessions := sm.List()
	res := ListSessionsRes{
		Sessions: make([]SessionInfo, len(sessions)),
	}
	for i, s := range sessions {
		res.Sessions[i] = sessionInfo(s)
	}
	return res
}

func marshalDashboardState(sm *SessionManager, dashboardID string) (string, error) {
	s, err := sm.Get(dashboardID)
	if err != nil {
		return "", err
	}
	return marshalResource(DashboardStateRes{
		DashboardSession: s,
		Layout:           renderLayout(s),
	})
}

func marshalDashboardExport(sm *SessionManager, dashboardID, version string) (string, error) {
	s, err := sm.Get(dashboardID)
	if err != nil {
		return "", err
	}
	if version == "" {
		version = s.Version
	}
	version, err = normalizeDashboardVersion(version)
	if err != nil {
		return "", err
	}
	dashboardJSON, err := buildDashboardJSON(s, version)
	if err != nil {
		return "", err
	}
	return string(dashboardJSON), nil
}

func marshalResource(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
