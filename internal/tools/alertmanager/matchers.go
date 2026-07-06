package alertmanager

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/prometheus/alertmanager/pkg/labels"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// MatcherResult is the structured form of a parsed Alertmanager matcher.
type MatcherResult struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"is_regex"`
	IsEqual bool   `json:"is_equal"`
	Raw     string `json:"raw"`
}

// parseMatcherQuery parses an Alertmanager matcher expression such as
// `alertname="Foo",service="bar"` (leading `{`/trailing `}` optional).
func parseMatcherQuery(query string) ([]*labels.Matcher, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("matcher query must not be empty")
	}
	return labels.ParseMatchers(query)
}

func toMatcherResults(ms []*labels.Matcher) []MatcherResult {
	out := make([]MatcherResult, 0, len(ms))
	for _, m := range ms {
		isEqual := m.Type == labels.MatchEqual || m.Type == labels.MatchRegexp
		isRegex := m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp
		out = append(out, MatcherResult{
			Name:    m.Name,
			Value:   m.Value,
			IsRegex: isRegex,
			IsEqual: isEqual,
			Raw:     m.String(),
		})
	}
	return out
}

// matchersToFilter converts parsed matchers into the repeated `filter`
// query-param form used by the Alertmanager v2 API (one matcher per element).
func matchersToFilter(ms []*labels.Matcher) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.String()
	}
	return out
}

// matchersToModels converts parsed matchers into the models.Matchers form
// used by PostableSilence.
func matchersToModels(ms []*labels.Matcher) models.Matchers {
	out := make(models.Matchers, len(ms))
	for i, m := range ms {
		var (
			isEqual = m.Type == labels.MatchEqual || m.Type == labels.MatchRegexp
			isRegex = m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp
			name    = m.Name
			value   = m.Value
		)
		out[i] = &models.Matcher{
			Name:    &name,
			Value:   &value,
			IsRegex: &isRegex,
			IsEqual: &isEqual,
		}
	}
	return out
}

// isCatchAllOnly reports whether the matcher set lacks any positive,
// non-wildcard constraint. Negative matchers and regexes that match arbitrary
// values are too broad to satisfy the create_silence guardrail by themselves.
func isCatchAllOnly(ms []*labels.Matcher) bool {
	if len(ms) == 0 {
		return false
	}
	return !slices.ContainsFunc(ms, isScopedMatcher)
}

func isScopedMatcher(m *labels.Matcher) bool {
	if m == nil {
		return false
	}
	switch m.Type {
	case labels.MatchEqual:
		return m.Value != ""
	case labels.MatchRegexp:
		return !isBroadRegex(m.Value)
	default:
		return false
	}
}

func isBroadRegex(expr string) bool {
	re, err := regexp.Compile("^(?:" + expr + ")$")
	if err != nil {
		return false
	}
	if re.MatchString("") {
		return true
	}
	return re.MatchString("gooners-catch-all-probe") && re.MatchString("gooners-catch-all-probe-2")
}

func validateSilenceMatchers(ms []*labels.Matcher) error {
	if len(ms) == 0 {
		return fmt.Errorf("at least one matcher is required")
	}
	if isCatchAllOnly(ms) {
		return fmt.Errorf("matchers must include at least one non-wildcard matcher; refusing a catch-all silence")
	}
	return nil
}

type ValidateMatcherQueryReq struct {
	Query string `json:"query" jsonschema:"Alertmanager matcher expression, e.g. alertname=\"HighErrorRate\",service=\"checkout\". Leading { and trailing } are optional. Supports =, !=, =~, !~."`
}

type ValidateMatcherQueryRes struct {
	Valid    bool            `json:"valid"`
	Matchers []MatcherResult `json:"matchers,omitempty"`
	Error    string          `json:"error,omitempty"`
}

func validateMatcherQueryHandler() mcp.ToolHandlerFor[ValidateMatcherQueryReq, ValidateMatcherQueryRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args ValidateMatcherQueryReq) (*mcp.CallToolResult, ValidateMatcherQueryRes, error) {
		ms, err := parseMatcherQuery(args.Query)
		if err != nil {
			return nil, ValidateMatcherQueryRes{Valid: false, Error: err.Error()}, nil
		}
		return nil, ValidateMatcherQueryRes{Valid: true, Matchers: toMatcherResults(ms)}, nil
	}
}

func registerMatcherTools(s *mcp.Server) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "validate_matcher_query",
		Description: "Parses an Alertmanager label-matcher query (used by filter params and silence matchers) and returns the individual matchers, or a precise syntax error. Does not contact Alertmanager.",
		Flags:       mcputil.ReadOnly,
	}, validateMatcherQueryHandler())
}
