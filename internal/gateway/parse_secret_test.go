package gateway

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsValidSecretName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "empty", input: "", want: false},
		{name: "simple", input: "foo", want: true},
		{name: "uppercase", input: "FOO", want: true},
		{name: "digits", input: "key1", want: true},
		{name: "underscore", input: "my_key", want: true},
		{name: "dot", input: "my.key", want: true},
		{name: "dash", input: "my-key", want: true},
		{name: "all valid chars", input: "A-Z.a_z0-9", want: true},
		{name: "space", input: "my key", want: false},
		{name: "slash", input: "my/key", want: false},
		{name: "colon", input: "a:b", want: false},
		{name: "brace", input: "a{b", want: false},
		{name: "null byte", input: "a\x00b", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isValidSecretName(tt.input))
		})
	}
}

func TestParseSecretRef(t *testing.T) {
	tests := []struct {
		name     string
		s        string
		i        int
		wantName string
		wantEnd  int
	}{
		// valid — exact format
		{name: "basic", s: "{secret:foo}", wantName: "foo", wantEnd: 12},
		// valid — whitespace variants
		{name: "space around colon", s: "{ secret : foo }", wantName: "foo", wantEnd: 16},
		{name: "tabs", s: "{\tsecret\t:\tbar\t}", wantName: "bar", wantEnd: 16},
		{name: "mixed whitespace", s: "{  secret  :  baz  }", wantName: "baz", wantEnd: 20},
		// valid — offset in larger string
		{name: "offset", s: "xx{secret:k}yy", i: 2, wantName: "k", wantEnd: 12},
		// valid — name with all allowed punctuation
		{name: "name with dot dash underscore", s: "{secret:my-key.v1_x}", wantName: "my-key.v1_x", wantEnd: 20},
		// not starting with '{'
		{name: "no brace", s: "secret:foo}", wantName: "", wantEnd: 0},
		// i past end
		{name: "i at end", s: "{secret:x}", i: 10, wantName: "", wantEnd: 0},
		// wrong keyword
		{name: "wrong keyword", s: "{env:foo}", wantName: "", wantEnd: 0},
		{name: "keyword prefix only", s: "{sec:foo}", wantName: "", wantEnd: 0},
		// no closing brace
		{name: "unclosed", s: "{secret:foo", wantName: "", wantEnd: 0},
		// empty name
		{name: "empty name", s: "{secret:}", wantName: "", wantEnd: 0},
		{name: "whitespace-only name", s: "{secret:   }", wantName: "", wantEnd: 0},
		// invalid name chars
		{name: "name with slash", s: "{secret:a/b}", wantName: "", wantEnd: 0},
		{name: "name with space", s: "{secret:a b}", wantName: "", wantEnd: 0},
		// no colon
		{name: "no colon", s: "{secret foo}", wantName: "", wantEnd: 0},
		// empty string
		{name: "empty string", s: "", wantName: "", wantEnd: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotEnd := parseSecretRef(tt.s, tt.i)
			require.Equal(t, tt.wantName, gotName, "name")
			require.Equal(t, tt.wantEnd, gotEnd, "end")
		})
	}
}

func TestExtractSecretRefs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "none", input: "no secrets here", want: nil},
		{name: "single", input: "{secret:foo}", want: []string{"foo"}},
		{name: "two", input: "{secret:a} and {secret:b}", want: []string{"a", "b"}},
		{name: "duplicate", input: "{secret:x}{secret:x}", want: []string{"x", "x"}},
		{name: "embedded in text", input: "Bearer {secret:tok}", want: []string{"tok"}},
		{name: "whitespace in ref", input: "{ secret : k }", want: []string{"k"}},
		{name: "invalid ref ignored", input: "{env:X} and {secret:ok}", want: []string{"ok"}},
		{name: "adjacent refs", input: "{secret:a}{secret:b}{secret:c}", want: []string{"a", "b", "c"}},
		{name: "unclosed brace ignored", input: "{secret:nope and {secret:yes}", want: []string{"yes"}},
		{name: "empty", input: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := slices.Collect(extractSecretRefs(tt.input))
			require.Equal(t, tt.want, got)
		})
	}
}

func TestInterpolate_Table(t *testing.T) {
	resolver := func(secrets map[string]string) SecretResolver {
		cfgs := make([]SecretConfig, 0, len(secrets))
		for k, v := range secrets {
			cfgs = append(cfgs, SecretConfig{Name: k, Value: v})
		}
		r, err := NewSecretResolver(cfgs)
		if err != nil {
			t.Helper()
			t.Fatalf("NewSecretResolver: %v", err)
		}
		return r
	}

	tests := []struct {
		name    string
		input   string
		secrets map[string]string
		want    string
		wantErr bool
	}{
		{
			name:    "no refs passthrough",
			input:   "plain string",
			secrets: nil,
			want:    "plain string",
		},
		{
			name:    "single substitution",
			input:   "{secret:k}",
			secrets: map[string]string{"k": "val"},
			want:    "val",
		},
		{
			name:    "prefix and suffix preserved",
			input:   "Bearer {secret:tok}",
			secrets: map[string]string{"tok": "abc123"},
			want:    "Bearer abc123",
		},
		{
			name:    "multiple substitutions",
			input:   "{secret:a}:{secret:b}",
			secrets: map[string]string{"a": "user", "b": "pass"},
			want:    "user:pass",
		},
		{
			name:    "same secret twice",
			input:   "{secret:x}/{secret:x}",
			secrets: map[string]string{"x": "tok"},
			want:    "tok/tok",
		},
		{
			name:    "whitespace in ref",
			input:   "{ secret : k }",
			secrets: map[string]string{"k": "v"},
			want:    "v",
		},
		{
			name:    "invalid ref left verbatim",
			input:   "{env:X}",
			secrets: nil,
			want:    "{env:X}",
		},
		{
			name:    "unclosed brace left verbatim",
			input:   "{secret:nope",
			secrets: map[string]string{"nope": "v"},
			want:    "{secret:nope",
		},
		{
			// A secret with an empty literal Value has no configured source,
			// so resolution fails (the 'default' branch in Resolve).
			name:    "empty value is missing source",
			input:   "a{secret:e}b",
			secrets: map[string]string{"e": ""},
			wantErr: true,
		},
		{
			name:    "missing secret returns error",
			input:   "{secret:missing}",
			secrets: nil,
			wantErr: true,
		},
		{
			name:    "nil resolver with no refs",
			input:   "plain",
			secrets: nil, // signals nil resolver in the test below
			want:    "plain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r SecretResolver
			if tt.name == "nil resolver with no refs" {
				r = nil
			} else {
				r = resolver(tt.secrets)
			}
			got, err := Interpolate(t.Context(), tt.input, r)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestInterpolate_NilResolverRejectsSecretRef(t *testing.T) {
	_, err := Interpolate(t.Context(), "{secret:x}", nil)
	require.Error(t, err)
}
