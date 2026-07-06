package alertmanager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	alertops "github.com/prometheus/alertmanager/api/v2/client/alert"
	silenceops "github.com/prometheus/alertmanager/api/v2/client/silence"
	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/prometheus/alertmanager/pkg/labels"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// SilenceSummary is a compact, context-friendly view of a Gettable silence.
type SilenceSummary struct {
	ID        string          `json:"id"`
	State     string          `json:"state"` // expired, active, pending
	Matchers  []MatcherResult `json:"matchers"`
	StartsAt  string          `json:"starts_at"`
	EndsAt    string          `json:"ends_at"`
	CreatedBy string          `json:"created_by,omitempty"`
	Comment   string          `json:"comment,omitempty"`
	UpdatedAt string          `json:"updated_at,omitempty"`
}

// modelMatchType maps the AM API's IsEqual/IsRegex bool pair onto the
// labels.MatchType enum, so Raw formatting can reuse labels.Matcher.String()
// (which applies Alertmanager's actual matcher escaping rules) instead of
// hand-rolled formatting.
func modelMatchType(isEqual, isRegex bool) labels.MatchType {
	switch {
	case isEqual && !isRegex:
		return labels.MatchEqual
	case !isEqual && !isRegex:
		return labels.MatchNotEqual
	case isEqual && isRegex:
		return labels.MatchRegexp
	default:
		return labels.MatchNotRegexp
	}
}

func modelMatchersToResults(ms models.Matchers) []MatcherResult {
	out := make([]MatcherResult, 0, len(ms))
	for _, m := range ms {
		if m == nil {
			continue
		}
		mr := MatcherResult{}
		if m.Name != nil {
			mr.Name = *m.Name
		}
		if m.Value != nil {
			mr.Value = *m.Value
		}
		if m.IsRegex != nil {
			mr.IsRegex = *m.IsRegex
		}
		if m.IsEqual != nil {
			mr.IsEqual = *m.IsEqual
		}
		if lm, err := labels.NewMatcher(modelMatchType(mr.IsEqual, mr.IsRegex), mr.Name, mr.Value); err == nil {
			mr.Raw = lm.String()
		}
		out = append(out, mr)
	}
	return out
}

func silenceSummaryFromModel(g *models.GettableSilence) SilenceSummary {
	s := SilenceSummary{
		Matchers: modelMatchersToResults(g.Matchers),
	}
	if g.ID != nil {
		s.ID = *g.ID
	}
	if g.StartsAt != nil {
		s.StartsAt = time.Time(*g.StartsAt).Format(time.RFC3339)
	}
	if g.EndsAt != nil {
		s.EndsAt = time.Time(*g.EndsAt).Format(time.RFC3339)
	}
	if g.CreatedBy != nil {
		s.CreatedBy = *g.CreatedBy
	}
	if g.Comment != nil {
		s.Comment = *g.Comment
	}
	if g.UpdatedAt != nil {
		s.UpdatedAt = time.Time(*g.UpdatedAt).Format(time.RFC3339)
	}
	if g.Status != nil && g.Status.State != nil {
		s.State = *g.Status.State
	}
	return s
}

type ListSilencesReq struct {
	Filter string `json:"filter,omitempty" jsonschema:"Alertmanager matcher expression to filter silences"`
}

type ListSilencesRes struct {
	Silences []SilenceSummary `json:"silences"`
	Count    int              `json:"count"`
}

func listSilencesHandler(c *Client) mcp.ToolHandlerFor[ListSilencesReq, ListSilencesRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ListSilencesReq) (*mcp.CallToolResult, ListSilencesRes, error) {
		filter, err := parseFilterOption(args.Filter)
		if err != nil {
			return nil, ListSilencesRes{}, err
		}

		res, err := c.am.Silence.GetSilences(silenceops.NewGetSilencesParams().WithContext(ctx).WithFilter(filter))
		if err != nil {
			return nil, ListSilencesRes{}, fmt.Errorf("get silences: %w", err)
		}

		silences := make([]SilenceSummary, 0, len(res.Payload))
		for _, sil := range res.Payload {
			silences = append(silences, silenceSummaryFromModel(sil))
		}
		return nil, ListSilencesRes{Silences: silences, Count: len(silences)}, nil
	}
}

type GetSilenceReq struct {
	ID string `json:"id" jsonschema:"The silence UUID"`
}

func getSilenceHandler(c *Client) mcp.ToolHandlerFor[GetSilenceReq, SilenceSummary] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args GetSilenceReq) (*mcp.CallToolResult, SilenceSummary, error) {
		res, err := c.am.Silence.GetSilence(silenceops.NewGetSilenceParams().WithContext(ctx).WithSilenceID(strfmt.UUID(args.ID)))
		if err != nil {
			return nil, SilenceSummary{}, fmt.Errorf("get silence: %w", err)
		}
		return nil, silenceSummaryFromModel(res.Payload), nil
	}
}

type PreviewSilenceReq struct {
	Matchers string `json:"matchers" jsonschema:"Alertmanager matcher expression describing the silence scope, e.g. alertname=\"HighErrorRate\",service=\"checkout\""`
}

type PreviewSilenceRes struct {
	Matchers []MatcherResult `json:"matchers"`
	Alerts   []AlertSummary  `json:"matching_alerts"`
	Count    int             `json:"count"`
}

func previewSilenceHandler(c *Client) mcp.ToolHandlerFor[PreviewSilenceReq, PreviewSilenceRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args PreviewSilenceReq) (*mcp.CallToolResult, PreviewSilenceRes, error) {
		ms, err := parseMatcherQuery(args.Matchers)
		if err != nil {
			return nil, PreviewSilenceRes{}, fmt.Errorf("invalid matchers: %w", err)
		}

		res, err := c.am.Alert.GetAlerts(alertops.NewGetAlertsParams().WithContext(ctx).WithFilter(matchersToFilter(ms)))
		if err != nil {
			return nil, PreviewSilenceRes{}, fmt.Errorf("preview matching alerts: %w", err)
		}

		alerts := make([]AlertSummary, 0, len(res.Payload))
		for _, a := range res.Payload {
			alerts = append(alerts, alertSummaryFromModel(a))
		}
		return nil, PreviewSilenceRes{Matchers: toMatcherResults(ms), Alerts: alerts, Count: len(alerts)}, nil
	}
}

type CreateSilenceReq struct {
	Matchers  string `json:"matchers" jsonschema:"Alertmanager matcher expression describing the silence scope, e.g. alertname=\"HighErrorRate\",service=\"checkout\". Must include at least one non-wildcard matcher."`
	StartsAt  string `json:"starts_at,omitempty" jsonschema:"RFC3339 start time; defaults to now"`
	EndsAt    string `json:"ends_at,omitempty" jsonschema:"RFC3339 end time; provide this or duration"`
	Duration  string `json:"duration,omitempty" jsonschema:"Duration from starts_at, e.g. 30m, 2h; provide this or ends_at"`
	CreatedBy string `json:"created_by" jsonschema:"Who/what is creating this silence (required)"`
	Comment   string `json:"comment" jsonschema:"Why this silence is being created (required)"`
}

type CreateSilenceRes struct {
	ID             string          `json:"id"`
	Matchers       []MatcherResult `json:"matchers"`
	StartsAt       string          `json:"starts_at"`
	EndsAt         string          `json:"ends_at"`
	MatchingAlerts int             `json:"matching_alerts"`
}

func ptrDateTime(t time.Time) *strfmt.DateTime {
	d := strfmt.DateTime(t)
	return &d
}

func createSilenceHandler(c *Client) mcp.ToolHandlerFor[CreateSilenceReq, CreateSilenceRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args CreateSilenceReq) (*mcp.CallToolResult, CreateSilenceRes, error) {
		// Validate matchers
		ms, err := parseMatcherQuery(args.Matchers)
		if err != nil {
			return nil, CreateSilenceRes{}, fmt.Errorf("invalid matchers: %w", err)
		}

		// Check at least one matcher
		if len(ms) == 0 {
			return nil, CreateSilenceRes{}, fmt.Errorf("at least one matcher is required")
		}

		// Check not catch-all only
		if isCatchAllOnly(ms) {
			return nil, CreateSilenceRes{}, fmt.Errorf("matchers must include at least one non-wildcard matcher; refusing a catch-all silence")
		}

		// Validate created_by
		if strings.TrimSpace(args.CreatedBy) == "" {
			return nil, CreateSilenceRes{}, fmt.Errorf("created_by is required")
		}

		// Validate comment
		if strings.TrimSpace(args.Comment) == "" {
			return nil, CreateSilenceRes{}, fmt.Errorf("comment is required")
		}

		// Parse start time
		var startsAt time.Time
		if args.StartsAt == "" {
			startsAt = time.Now().UTC()
		} else {
			t, err := time.Parse(time.RFC3339, args.StartsAt)
			if err != nil {
				return nil, CreateSilenceRes{}, fmt.Errorf("invalid starts_at: %w", err)
			}
			startsAt = t
		}

		// Parse end time
		var endsAt time.Time
		if args.EndsAt != "" && args.Duration != "" {
			return nil, CreateSilenceRes{}, fmt.Errorf("exactly one of ends_at or duration must be set")
		}
		if args.EndsAt == "" && args.Duration == "" {
			return nil, CreateSilenceRes{}, fmt.Errorf("exactly one of ends_at or duration must be set")
		}

		if args.EndsAt != "" {
			t, err := time.Parse(time.RFC3339, args.EndsAt)
			if err != nil {
				return nil, CreateSilenceRes{}, fmt.Errorf("invalid ends_at: %w", err)
			}
			endsAt = t
		} else {
			d, err := time.ParseDuration(args.Duration)
			if err != nil {
				return nil, CreateSilenceRes{}, fmt.Errorf("invalid duration: %w", err)
			}
			endsAt = startsAt.Add(d)
		}

		// Validate end > start
		if !endsAt.After(startsAt) {
			return nil, CreateSilenceRes{}, fmt.Errorf("ends_at must be after starts_at")
		}

		// Check max duration
		if maxDur := c.cfg.MaxSilenceDuration; endsAt.Sub(startsAt) > maxDur {
			return nil, CreateSilenceRes{}, fmt.Errorf("silence duration %s exceeds the configured maximum of %s", endsAt.Sub(startsAt), maxDur)
		}

		// Preview matching alerts
		previewRes, err := c.am.Alert.GetAlerts(alertops.NewGetAlertsParams().WithContext(ctx).WithFilter(matchersToFilter(ms)))
		if err != nil {
			return nil, CreateSilenceRes{}, fmt.Errorf("preview matching alerts: %w", err)
		}

		matchingCount := 0
		if previewRes.Payload != nil {
			matchingCount = len(previewRes.Payload)
		}

		// Create the silence
		sil := &models.PostableSilence{
			Silence: models.Silence{
				Matchers:  matchersToModels(ms),
				StartsAt:  ptrDateTime(startsAt),
				EndsAt:    ptrDateTime(endsAt),
				CreatedBy: &args.CreatedBy,
				Comment:   &args.Comment,
			},
		}

		res, err := c.am.Silence.PostSilences(silenceops.NewPostSilencesParams().WithContext(ctx).WithSilence(sil))
		if err != nil {
			return nil, CreateSilenceRes{}, fmt.Errorf("create silence: %w", err)
		}

		silenceID := ""
		if res.Payload != nil {
			silenceID = res.Payload.SilenceID
		}

		return nil, CreateSilenceRes{
			ID:             silenceID,
			Matchers:       toMatcherResults(ms),
			StartsAt:       startsAt.Format(time.RFC3339),
			EndsAt:         endsAt.Format(time.RFC3339),
			MatchingAlerts: matchingCount,
		}, nil
	}
}

type ExpireSilenceReq struct {
	ID string `json:"id" jsonschema:"The silence UUID to expire immediately"`
}

func expireSilenceHandler(c *Client) mcp.ToolHandlerFor[ExpireSilenceReq, mcputil.SuccessResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ExpireSilenceReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		_, err := c.am.Silence.DeleteSilence(silenceops.NewDeleteSilenceParams().WithContext(ctx).WithSilenceID(strfmt.UUID(args.ID)))
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, fmt.Errorf("expire silence: %w", err)
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

func registerSilenceTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "list_silences",
		Description: "Lists current Alertmanager silences, optionally filtered by a matcher query.",
		Flags:       mcputil.ReadOnly,
	}, listSilencesHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "get_silence",
		Description: "Retrieves a single Alertmanager silence by ID.",
		Flags:       mcputil.ReadOnly,
	}, getSilenceHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "preview_silence",
		Description: "Shows which currently-firing alerts a matcher set would silence, without creating a silence.",
		Flags:       mcputil.ReadOnly,
	}, previewSilenceHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "create_silence",
		Description: "Creates an Alertmanager silence after validating: at least one non-wildcard matcher, required created_by/comment, and a duration within the configured maximum. Returns how many currently-firing alerts would be silenced. Call preview_silence first to check blast radius before committing.",
		Flags:       mcputil.Destructive,
	}, createSilenceHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "expire_silence",
		Description: "Immediately expires (deletes) an Alertmanager silence by ID.",
		Flags:       mcputil.Destructive,
	}, expireSilenceHandler(c))
}
