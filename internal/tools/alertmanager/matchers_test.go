package alertmanager

import (
	"context"
	"testing"

	"github.com/prometheus/alertmanager/pkg/labels"
	"github.com/stretchr/testify/require"
)

func TestParseMatcherQuery_Valid(t *testing.T) {
	tests := []struct {
		name           string
		query          string
		expectedCount  int
		expectedNames  []string
		expectedValues []string
	}{
		{
			name:           "single_equal",
			query:          `alertname="Foo"`,
			expectedCount:  1,
			expectedNames:  []string{"alertname"},
			expectedValues: []string{"Foo"},
		},
		{
			name:           "multiple_matchers",
			query:          `alertname="Foo",service="bar"`,
			expectedCount:  2,
			expectedNames:  []string{"alertname", "service"},
			expectedValues: []string{"Foo", "bar"},
		},
		{
			name:           "with_braces",
			query:          `{alertname="Foo",service="bar"}`,
			expectedCount:  2,
			expectedNames:  []string{"alertname", "service"},
			expectedValues: []string{"Foo", "bar"},
		},
		{
			name:           "regex_matcher",
			query:          `service=~"api.*"`,
			expectedCount:  1,
			expectedNames:  []string{"service"},
			expectedValues: []string{"api.*"},
		},
		{
			name:           "not_equal",
			query:          `env!="prod"`,
			expectedCount:  1,
			expectedNames:  []string{"env"},
			expectedValues: []string{"prod"},
		},
		{
			name:           "not_regex",
			query:          `service!~"test.*"`,
			expectedCount:  1,
			expectedNames:  []string{"service"},
			expectedValues: []string{"test.*"},
		},
		{
			name:           "whitespace",
			query:          "  alertname=\"Foo\" , service=\"bar\"  ",
			expectedCount:  2,
			expectedNames:  []string{"alertname", "service"},
			expectedValues: []string{"Foo", "bar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := parseMatcherQuery(tt.query)
			require.NoError(t, err)
			require.Len(t, ms, tt.expectedCount)

			for i, m := range ms {
				require.Equal(t, tt.expectedNames[i], m.Name)
				require.Equal(t, tt.expectedValues[i], m.Value)
			}
		})
	}
}

func TestParseMatcherQuery_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "empty_string",
			query: "",
		},
		{
			name:  "whitespace_only",
			query: "   ",
		},
		{
			name:  "invalid_syntax",
			query: "alertname",
		},
		{
			name:  "unclosed_quote",
			query: `alertname="Foo`,
		},
		{
			name:  "invalid_operator",
			query: `alertname ~ "Foo"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseMatcherQuery(tt.query)
			require.Error(t, err)
		})
	}
}

func TestToMatcherResults(t *testing.T) {
	tests := []struct {
		name     string
		matchers []*labels.Matcher
		check    func(t *testing.T, results []MatcherResult)
	}{
		{
			name: "equal_matcher",
			matchers: []*labels.Matcher{
				{Name: "alertname", Value: "Foo", Type: labels.MatchEqual},
			},
			check: func(t *testing.T, results []MatcherResult) {
				require.Len(t, results, 1)
				require.Equal(t, "alertname", results[0].Name)
				require.Equal(t, "Foo", results[0].Value)
				require.True(t, results[0].IsEqual)
				require.False(t, results[0].IsRegex)
			},
		},
		{
			name: "regex_matcher",
			matchers: []*labels.Matcher{
				{Name: "service", Value: "api.*", Type: labels.MatchRegexp},
			},
			check: func(t *testing.T, results []MatcherResult) {
				require.Len(t, results, 1)
				require.Equal(t, "service", results[0].Name)
				require.Equal(t, "api.*", results[0].Value)
				require.True(t, results[0].IsEqual)
				require.True(t, results[0].IsRegex)
			},
		},
		{
			name: "not_equal_matcher",
			matchers: []*labels.Matcher{
				{Name: "env", Value: "test", Type: labels.MatchNotEqual},
			},
			check: func(t *testing.T, results []MatcherResult) {
				require.Len(t, results, 1)
				require.Equal(t, "env", results[0].Name)
				require.Equal(t, "test", results[0].Value)
				require.False(t, results[0].IsEqual)
				require.False(t, results[0].IsRegex)
			},
		},
		{
			name: "not_regex_matcher",
			matchers: []*labels.Matcher{
				{Name: "service", Value: "test.*", Type: labels.MatchNotRegexp},
			},
			check: func(t *testing.T, results []MatcherResult) {
				require.Len(t, results, 1)
				require.Equal(t, "service", results[0].Name)
				require.Equal(t, "test.*", results[0].Value)
				require.False(t, results[0].IsEqual)
				require.True(t, results[0].IsRegex)
			},
		},
		{
			name: "raw_field",
			matchers: []*labels.Matcher{
				{Name: "alertname", Value: "Foo", Type: labels.MatchEqual},
			},
			check: func(t *testing.T, results []MatcherResult) {
				require.Len(t, results, 1)
				require.NotEmpty(t, results[0].Raw)
				require.Equal(t, `alertname="Foo"`, results[0].Raw)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := toMatcherResults(tt.matchers)
			tt.check(t, results)
		})
	}
}

func TestMatchersToFilter(t *testing.T) {
	matchers := []*labels.Matcher{
		{Name: "alertname", Value: "Foo", Type: labels.MatchEqual},
		{Name: "service", Value: "bar", Type: labels.MatchEqual},
	}

	filters := matchersToFilter(matchers)
	require.Len(t, filters, 2)
	require.Equal(t, `alertname="Foo"`, filters[0])
	require.Equal(t, `service="bar"`, filters[1])
}

func TestMatchersToModels(t *testing.T) {
	matchers := []*labels.Matcher{
		{Name: "alertname", Value: "Foo", Type: labels.MatchEqual},
		{Name: "service", Value: "api.*", Type: labels.MatchRegexp},
	}

	models := matchersToModels(matchers)
	require.Len(t, models, 2)

	// First matcher: equal, not regex
	require.NotNil(t, models[0])
	require.Equal(t, "alertname", *models[0].Name)
	require.Equal(t, "Foo", *models[0].Value)
	require.True(t, *models[0].IsEqual)
	require.False(t, *models[0].IsRegex)

	// Second matcher: equal, regex
	require.NotNil(t, models[1])
	require.Equal(t, "service", *models[1].Name)
	require.Equal(t, "api.*", *models[1].Value)
	require.True(t, *models[1].IsEqual)
	require.True(t, *models[1].IsRegex)
}

func TestIsCatchAllOnly(t *testing.T) {
	tests := []struct {
		name       string
		matchers   []*labels.Matcher
		isCatchAll bool
	}{
		{
			name:       "empty_matchers",
			matchers:   []*labels.Matcher{},
			isCatchAll: false,
		},
		{
			name: "single_catchall_.*",
			matchers: []*labels.Matcher{
				{Name: "service", Value: ".*", Type: labels.MatchRegexp},
			},
			isCatchAll: true,
		},
		{
			name: "single_catchall_.+",
			matchers: []*labels.Matcher{
				{Name: "service", Value: ".+", Type: labels.MatchRegexp},
			},
			isCatchAll: true,
		},
		{
			name: "multiple_catchalls",
			matchers: []*labels.Matcher{
				{Name: "service", Value: ".*", Type: labels.MatchRegexp},
				{Name: "team", Value: ".*", Type: labels.MatchRegexp},
			},
			isCatchAll: true,
		},
		{
			name: "catchall_with_specific",
			matchers: []*labels.Matcher{
				{Name: "service", Value: ".*", Type: labels.MatchRegexp},
				{Name: "team", Value: "sre", Type: labels.MatchEqual},
			},
			isCatchAll: false,
		},
		{
			name: "equal_specific",
			matchers: []*labels.Matcher{
				{Name: "service", Value: "checkout", Type: labels.MatchEqual},
			},
			isCatchAll: false,
		},
		{
			name: "non_catchall_regex",
			matchers: []*labels.Matcher{
				{Name: "service", Value: "api-.*", Type: labels.MatchRegexp},
			},
			isCatchAll: false,
		},
		{
			name: "not_equal_matcher",
			matchers: []*labels.Matcher{
				{Name: "env", Value: ".*", Type: labels.MatchNotRegexp},
			},
			isCatchAll: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isCatchAllOnly(tt.matchers)
			require.Equal(t, tt.isCatchAll, result)
		})
	}
}

func TestValidateMatcherQueryHandler_Valid(t *testing.T) {
	handler := validateMatcherQueryHandler()

	tests := []struct {
		name     string
		query    string
		wantErr  bool
		checkRes func(t *testing.T, res ValidateMatcherQueryRes)
	}{
		{
			name:    "single_matcher",
			query:   `alertname="HighErrorRate"`,
			wantErr: false,
			checkRes: func(t *testing.T, res ValidateMatcherQueryRes) {
				require.True(t, res.Valid)
				require.Empty(t, res.Error)
				require.Len(t, res.Matchers, 1)
				require.Equal(t, "alertname", res.Matchers[0].Name)
				require.Equal(t, "HighErrorRate", res.Matchers[0].Value)
			},
		},
		{
			name:    "multiple_matchers",
			query:   `alertname="HighErrorRate",service="checkout"`,
			wantErr: false,
			checkRes: func(t *testing.T, res ValidateMatcherQueryRes) {
				require.True(t, res.Valid)
				require.Empty(t, res.Error)
				require.Len(t, res.Matchers, 2)
				require.Equal(t, "alertname", res.Matchers[0].Name)
				require.Equal(t, "service", res.Matchers[1].Name)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, res, err := handler(context.Background(), nil, ValidateMatcherQueryReq{Query: tt.query})
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			tt.checkRes(t, res)
		})
	}
}

func TestValidateMatcherQueryHandler_Invalid(t *testing.T) {
	handler := validateMatcherQueryHandler()

	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "empty_query",
			query: "",
		},
		{
			name:  "invalid_syntax",
			query: "alertname",
		},
		{
			name:  "unclosed_quote",
			query: `alertname="Foo`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, res, _ := handler(context.Background(), nil, ValidateMatcherQueryReq{Query: tt.query})
			require.False(t, res.Valid)
			require.NotEmpty(t, res.Error)
		})
	}
}
