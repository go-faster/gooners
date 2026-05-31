package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSSHConfig(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Machine
	}{
		{
			name: "simple",
			in:   "Host myserver\n    User deploy\n",
			want: []Machine{{Name: "myserver", User: "deploy"}},
		},
		{
			name: "multiple aliases one block",
			in:   "Host web1 web2 web3\nUser www\n",
			want: []Machine{
				{Name: "web1", User: "www"},
				{Name: "web2", User: "www"},
				{Name: "web3", User: "www"},
			},
		},
		{
			name: "user on second host",
			in:   "Host a\nHost b\n    User bob\n",
			want: []Machine{
				{Name: "a", User: ""},
				{Name: "b", User: "bob"},
			},
		},
		{
			name: "equals syntax",
			in:   "Host=foo\nUser=alice\n",
			want: []Machine{{Name: "foo", User: "alice"}},
		},
		{
			name: "mixed separators",
			in:   "Host bar\nUser=bob\n",
			want: []Machine{{Name: "bar", User: "bob"}},
		},
		{
			name: "comments and blank lines",
			in:   "# comment\n\nHost prod # inline\nUser deploy\n",
			want: []Machine{{Name: "prod", User: "deploy"}},
		},
		{
			name: "case insensitive keywords",
			in:   "host mysrv\nuser foo\n",
			want: []Machine{{Name: "mysrv", User: "foo"}},
		},
		{
			name: "wildcards are skipped",
			in:   "Host *\nUser root\nHost *.example.com\nUser x\nHost h?\nUser y\nHost good\nUser z\n",
			want: []Machine{{Name: "good", User: "z"}},
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "no user",
			in:   "Host onlyname\n",
			want: []Machine{{Name: "onlyname", User: ""}},
		},
		{
			name: "include ignored by pure parser",
			in:   "Host a\nUser u\nInclude other.conf\nHost b\n",
			want: []Machine{
				{Name: "a", User: "u"},
				{Name: "b", User: ""},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSSHConfig(strings.NewReader(tc.in), nil)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %d want %d\n got=%+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d: got %+v want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseSSHConfigWithIncludeCallback(t *testing.T) {
	input := "Host local\nUser me\nInclude ~/.ssh/extra\nHost other\n"
	var seenIncludes []string

	_ = parseSSHConfig(strings.NewReader(input), func(incs []string) {
		seenIncludes = append(seenIncludes, incs...)
	})

	if len(seenIncludes) != 1 || seenIncludes[0] != "~/.ssh/extra" {
		t.Errorf("expected include callback to receive [~/.ssh/extra], got %v", seenIncludes)
	}
}

func FuzzParseSSHConfig(f *testing.F) {
	// Seed corpus with realistic and edge-case configs
	seeds := []string{
		"Host myserver\n    User deploy\n",
		"Host a b c\nUser www\n",
		"Host=prod User=app\n",
		"# comment\nHost good\nUser u\nHost *\nUser root\n",
		"Host h1\nHost h2\n    User shared\n",
		"",
		"Host only\n",
		"Host foo\nUser a\nInclude bar\nHost baz\n",
		"Host github.com\nUser git\n",
		"Host 1 2 3 4 5\nUser x\n",
		strings.Repeat("Host x\nUser y\n", 20),
		"Host *\nHost *.*\nHost h?\nHost good\n",
	}

	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic
		machines := parseSSHConfig(bytes.NewReader(data), nil)

		for _, m := range machines {
			if m.Name == "" {
				t.Errorf("empty machine name from input %q", data)
			}
			if strings.ContainsAny(m.Name, "*?[]") || m.Name == "*" {
				t.Errorf("wildcard name leaked through parser: %q", m.Name)
			}
			// User may be empty or set, both are valid
			_ = m.User
		}
	})
}

func TestListMachines_Integration(t *testing.T) {
	root := t.TempDir()

	// fake home dir for testing ~/ expansion in Includes
	fakeHome := filepath.Join(root, "fakehome")
	sshDir := filepath.Join(fakeHome, ".ssh")
	mustMkdirAll(t, sshDir)

	// file referenced via ~
	tildeInclude := filepath.Join(sshDir, "tilde.conf")
	mustWriteFile(t, tildeInclude, "Host from-tilde\n    User tildeuser\n")

	// main entrypoint config
	mainCfg := filepath.Join(root, "main.conf")
	mainContent := `
# main config with various includes
Include extra.conf
Include includes/*.conf
Include ~/.ssh/tilde.conf

Host direct1
    User alice
`
	mustWriteFile(t, mainCfg, mainContent)

	// relative include
	extra := filepath.Join(root, "extra.conf")
	mustWriteFile(t, extra, `
Host from-extra
    User bob
`)

	// glob target dir
	incDir := filepath.Join(root, "includes")
	mustMkdirAll(t, incDir)
	mustWriteFile(t, filepath.Join(incDir, "part1.conf"), "Host globbed1\n    User g1\n")
	mustWriteFile(t, filepath.Join(incDir, "part2.conf"), "Host globbed2\n    User g2\nHost bad-wildcard*\n    User shouldnotappear\n")

	got := listMachinesFromRoots([]string{mainCfg}, fakeHome)

	byName := map[string]string{}
	for _, m := range got {
		byName[m.Name] = m.User
	}

	want := map[string]string{
		"direct1":    "alice",
		"from-extra": "bob",
		"globbed1":   "g1",
		"globbed2":   "g2",
		"from-tilde": "tildeuser",
	}

	for name, user := range want {
		require.Contains(t, byName, name, "missing machine")
		require.Equal(t, user, byName[name], "wrong user for %s", name)
	}

	// wildcards must never appear
	for _, m := range got {
		require.False(t, strings.ContainsAny(m.Name, "*?[]"), "wildcard machine leaked: %s", m.Name)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o755), "mkdir %s", path)
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "write %s", path)
}
