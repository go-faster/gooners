package opencode

import (
	"fmt"
	"path/filepath"
	"strings"
)

// validateSessionID ensures that a session ID does not contain path traversal payloads.
func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return nil
	}
	clean := filepath.Clean(sessionID)
	if clean == "." || clean == ".." || strings.ContainsAny(clean, `/\`) {
		return fmt.Errorf("invalid sessionID: %q", sessionID)
	}
	return nil
}
