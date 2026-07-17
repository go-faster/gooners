package gitlab

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListArgsListOptions(t *testing.T) {
	for _, tt := range []struct {
		name        string
		args        ListArgs
		wantPage    int64
		wantPerPage int64
	}{
		{"zero value defaults", ListArgs{}, 1, defaultPerPage},
		{"explicit", ListArgs{Page: 3, PerPage: 50}, 3, 50},
		{"negative page floors to first", ListArgs{Page: -1}, 1, defaultPerPage},
		{"per page clamped to server maximum", ListArgs{PerPage: 5000}, 1, maxPerPage},
		{"per page at maximum is kept", ListArgs{PerPage: maxPerPage}, 1, maxPerPage},
		{"negative per page defaults", ListArgs{PerPage: -10}, 1, defaultPerPage},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.args.listOptions()
			require.Equal(t, tt.wantPage, got.Page)
			require.Equal(t, tt.wantPerPage, got.PerPage)
		})
	}
}

func TestTruncate(t *testing.T) {
	for _, tt := range []struct {
		name      string
		in        string
		limit     int
		want      string
		wantTrunc bool
	}{
		{"under limit", "hello", 10, "hello", false},
		{"exactly at limit", "hello", 5, "hello", false},
		{"over limit", "hello world", 5, "hello", true},
		{"empty", "", 5, "", false},
		// Runes, not bytes: cutting mid-rune would corrupt the output.
		{"multibyte counted as runes", "привет", 6, "привет", false},
		{"multibyte cut on rune boundary", "привет", 3, "при", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := truncate(tt.in, tt.limit)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.wantTrunc, truncated)
		})
	}
}

func TestSplitCSV(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want []string
	}{
		{"single", "bug", []string{"bug"}},
		{"multiple", "bug,ux", []string{"bug", "ux"}},
		{"spaces trimmed", "bug, ux , p1", []string{"bug", "ux", "p1"}},
		{"blanks dropped", "bug,,ux,", []string{"bug", "ux"}},
		{"empty", "", []string{}},
		{"only separators", ",,,", []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, splitCSV(tt.in))
		})
	}
}
