package alertmanager

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	alertops "github.com/prometheus/alertmanager/api/v2/client/alert"
	alertgroupops "github.com/prometheus/alertmanager/api/v2/client/alertgroup"
	"github.com/prometheus/alertmanager/api/v2/models"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// AlertSummary is a compact, context-friendly view of a Gettable alert.
type AlertSummary struct {
	Fingerprint  string            `json:"fingerprint"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	State        string            `json:"state,omitempty"` // unprocessed, active, suppressed
	SilencedBy   []string          `json:"silenced_by,omitempty"`
	InhibitedBy  []string          `json:"inhibited_by,omitempty"`
	MutedBy      []string          `json:"muted_by,omitempty"`
	StartsAt     string            `json:"starts_at,omitempty"`
	EndsAt       string            `json:"ends_at,omitempty"`
	Receivers    []string          `json:"receivers,omitempty"`
	GeneratorURL string            `json:"generator_url,omitempty"`
}

func alertSummaryFromModel(a *models.GettableAlert) AlertSummary {
	s := AlertSummary{
		Labels:       map[string]string(a.Labels),
		Annotations:  map[string]string(a.Annotations),
		GeneratorURL: string(a.GeneratorURL),
	}
	if a.Fingerprint != nil {
		s.Fingerprint = *a.Fingerprint
	}
	if a.StartsAt != nil {
		s.StartsAt = time.Time(*a.StartsAt).Format(time.RFC3339)
	}
	if a.EndsAt != nil {
		s.EndsAt = time.Time(*a.EndsAt).Format(time.RFC3339)
	}
	if a.Status != nil {
		if a.Status.State != nil {
			s.State = *a.Status.State
		}
		s.SilencedBy = a.Status.SilencedBy
		s.InhibitedBy = a.Status.InhibitedBy
		s.MutedBy = a.Status.MutedBy
	}
	for _, r := range a.Receivers {
		if r != nil && r.Name != nil {
			s.Receivers = append(s.Receivers, *r.Name)
		}
	}
	return s
}

// parseFilterOption parses an optional matcher-query filter string into the
// repeated-filter form expected by the Alertmanager v2 API, or returns nil
// filter when query is empty.
func parseFilterOption(query string) ([]string, error) {
	if query == "" {
		return nil, nil
	}
	ms, err := parseMatcherQuery(query)
	if err != nil {
		return nil, fmt.Errorf("invalid filter: %w", err)
	}
	return matchersToFilter(ms), nil
}

type ListAlertsReq struct {
	Filter      string `json:"filter,omitempty" jsonschema:"Alertmanager matcher expression to filter alerts, e.g. alertname=\"HighErrorRate\",service=\"checkout\""`
	Receiver    string `json:"receiver,omitempty" jsonschema:"Regex matching receiver names to filter by"`
	Active      *bool  `json:"active,omitempty" jsonschema:"Include active alerts (default true)"`
	Silenced    *bool  `json:"silenced,omitempty" jsonschema:"Include silenced alerts (default true)"`
	Inhibited   *bool  `json:"inhibited,omitempty" jsonschema:"Include inhibited alerts (default true)"`
	Unprocessed *bool  `json:"unprocessed,omitempty" jsonschema:"Include unprocessed alerts (default true)"`
}

type ListAlertsRes struct {
	Alerts []AlertSummary `json:"alerts"`
	Count  int            `json:"count"`
}

func listAlertsHandler(c *Client) mcp.ToolHandlerFor[ListAlertsReq, ListAlertsRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ListAlertsReq) (*mcp.CallToolResult, ListAlertsRes, error) {
		filter, err := parseFilterOption(args.Filter)
		if err != nil {
			return nil, ListAlertsRes{}, err
		}

		params := alertops.NewGetAlertsParams().WithContext(ctx).
			WithFilter(filter).
			WithActive(args.Active).
			WithSilenced(args.Silenced).
			WithInhibited(args.Inhibited).
			WithUnprocessed(args.Unprocessed)
		if args.Receiver != "" {
			params = params.WithReceiver(&args.Receiver)
		}

		res, err := c.am.Alert.GetAlerts(params)
		if err != nil {
			return nil, ListAlertsRes{}, fmt.Errorf("get alerts: %w", err)
		}

		alerts := make([]AlertSummary, 0, len(res.Payload))
		for _, a := range res.Payload {
			alerts = append(alerts, alertSummaryFromModel(a))
		}
		return nil, ListAlertsRes{Alerts: alerts, Count: len(alerts)}, nil
	}
}

type AlertGroupSummary struct {
	Labels   map[string]string `json:"labels"`
	Receiver string            `json:"receiver,omitempty"`
	Alerts   []AlertSummary    `json:"alerts"`
}

type ListAlertGroupsReq struct {
	Filter    string `json:"filter,omitempty" jsonschema:"Alertmanager matcher expression to filter alert groups, e.g. alertname=\"HighErrorRate\",service=\"checkout\""`
	Receiver  string `json:"receiver,omitempty" jsonschema:"Regex matching receiver names to filter by"`
	Active    *bool  `json:"active,omitempty" jsonschema:"Include active alerts within groups (default true)"`
	Silenced  *bool  `json:"silenced,omitempty" jsonschema:"Include silenced alerts within groups (default true)"`
	Inhibited *bool  `json:"inhibited,omitempty" jsonschema:"Include inhibited alerts within groups (default true)"`
	Muted     *bool  `json:"muted,omitempty" jsonschema:"Include entire groups where all alerts are muted (default true)"`
}

type ListAlertGroupsRes struct {
	Groups []AlertGroupSummary `json:"groups"`
	Count  int                 `json:"count"`
}

func listAlertGroupsHandler(c *Client) mcp.ToolHandlerFor[ListAlertGroupsReq, ListAlertGroupsRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ListAlertGroupsReq) (*mcp.CallToolResult, ListAlertGroupsRes, error) {
		filter, err := parseFilterOption(args.Filter)
		if err != nil {
			return nil, ListAlertGroupsRes{}, err
		}

		params := alertgroupops.NewGetAlertGroupsParams().WithContext(ctx).
			WithFilter(filter).
			WithActive(args.Active).
			WithSilenced(args.Silenced).
			WithInhibited(args.Inhibited).
			WithMuted(args.Muted)
		if args.Receiver != "" {
			params = params.WithReceiver(&args.Receiver)
		}

		res, err := c.am.Alertgroup.GetAlertGroups(params)
		if err != nil {
			return nil, ListAlertGroupsRes{}, fmt.Errorf("get alert groups: %w", err)
		}

		groups := make([]AlertGroupSummary, 0, len(res.Payload))
		for _, g := range res.Payload {
			gs := AlertGroupSummary{Labels: map[string]string(g.Labels)}
			if g.Receiver != nil && g.Receiver.Name != nil {
				gs.Receiver = *g.Receiver.Name
			}
			for _, a := range g.Alerts {
				gs.Alerts = append(gs.Alerts, alertSummaryFromModel(a))
			}
			groups = append(groups, gs)
		}
		return nil, ListAlertGroupsRes{Groups: groups, Count: len(groups)}, nil
	}
}

func registerAlertTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "list_alerts",
		Description: "Lists current Alertmanager alerts, optionally filtered by a matcher query, receiver, and active/silenced/inhibited/unprocessed state.",
		Flags:       mcputil.ReadOnly,
	}, listAlertsHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "list_alert_groups",
		Description: "Lists current Alertmanager alert groups (as grouped/routed for notification), optionally filtered by a matcher query, receiver, and active/silenced/inhibited/muted state.",
		Flags:       mcputil.ReadOnly,
	}, listAlertGroupsHandler(c))
}
