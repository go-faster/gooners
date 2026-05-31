// Package session provides SSH configuration parsing, host key handling,
// authentication methods, and connection pooling for remote machine access.
package session

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Machine struct {
	Name string `json:"name"`
	User string `json:"user"`
}

// parseSSHConfig parses a single ssh_config source from r.
// It returns Machine entries for every non-wildcard Host alias found,
// along with the User active for that Host block (empty if not set in the block).
//
// Include directives are *not* followed by this function. If onInclude is
// non-nil, it is called with the raw values from each Include line so the
// caller can perform path resolution, globbing and recursion if desired.
func parseSSHConfig(r io.Reader, onInclude func([]string)) []Machine {
	var machines []Machine
	var currentHosts []string
	currentUser := ""

	flush := func() {
		for _, h := range currentHosts {
			if h == "" || h == "*" || strings.ContainsAny(h, "*?[]") {
				continue
			}
			machines = append(machines, Machine{Name: h, User: currentUser})
		}
		currentHosts = nil
		currentUser = ""
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if before, _, found := strings.Cut(line, "#"); found {
			line = strings.TrimSpace(before)
		}
		if line == "" {
			continue
		}

		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '='
		})
		if len(fields) == 0 {
			continue
		}
		kw := strings.ToLower(fields[0])

		switch kw {
		case "host":
			flush()
			if len(fields) > 1 {
				currentHosts = fields[1:]
			}
		case "user":
			if len(fields) > 1 && len(currentHosts) > 0 {
				currentUser = fields[1]
			}
		case "include":
			// Flush the current Host block before descending into the Include.
			// After the callback returns, currentHosts/currentUser will be reset.
			// Any directives between this Include and the next "Host" line will
			// therefore be ignored (they have no active Host block).
			//
			// This matches OpenSSH ssh_config semantics: directives are only
			// meaningful inside a Host block, and Include acts as a textual
			// splice point for additional config.
			flush()
			if onInclude != nil && len(fields) > 1 {
				onInclude(fields[1:])
			}
		}
	}
	_ = scanner.Err()
	flush()
	// scanner.Err is ignored; this is a best-effort parser for user ssh config.
	return machines
}

// ListMachines returns the Host aliases (non-pattern) defined in the user's
// ~/.ssh/config and /etc/ssh/ssh_config (and their Includes). Only name and
// explicit User are returned; no HostName or other parameters.
//
// This is a best-effort operation: missing files, permission errors, and
// malformed Include directives are silently ignored.
func ListMachines() []Machine {
	home := homeDir()
	return listMachinesFromRoots([]string{
		filepath.Join(home, ".ssh", "config"),
		"/etc/ssh/ssh_config",
	}, home)
}

// listMachinesFromRoots walks the provided root config files (plus any Includes they reference),
// using the given home directory for ~ expansion and relative path resolution.
func listMachinesFromRoots(roots []string, home string) []Machine {
	var machines []Machine
	seen := make(map[string]struct{})

	var load func(string, int)
	load = func(path string, depth int) {
		if depth > 10 {
			return
		}
		f, err := os.Open(path) //nolint:gosec // path from ssh config Include or user-provided roots, trusted input
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()

		baseDir := filepath.Dir(path)

		parsed := parseSSHConfig(f, func(incs []string) {
			for _, inc := range incs {
				incPath := inc
				if rest, ok := strings.CutPrefix(inc, "~/"); ok {
					incPath = filepath.Join(home, rest)
				} else if !filepath.IsAbs(inc) {
					incPath = filepath.Join(baseDir, inc)
				}
				matches, _ := filepath.Glob(incPath)
				if len(matches) == 0 {
					matches = []string{incPath}
				}
				for _, m := range matches {
					load(m, depth+1)
				}
			}
		})

		for _, m := range parsed {
			if _, ok := seen[m.Name]; ok {
				continue
			}
			seen[m.Name] = struct{}{}
			machines = append(machines, m)
		}
	}

	for _, root := range roots {
		load(root, 0)
	}

	return machines
}
