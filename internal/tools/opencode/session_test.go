package opencode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateSessionID(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		wantErr   bool
	}{
		{
			name:      "empty",
			sessionID: "",
			wantErr:   false,
		},
		{
			name:      "valid simple",
			sessionID: "ses_12345",
			wantErr:   false,
		},
		{
			name:      "valid with dash",
			sessionID: "ses-12345",
			wantErr:   false,
		},
		{
			name:      "dot",
			sessionID: ".",
			wantErr:   true,
		},
		{
			name:      "double dot",
			sessionID: "..",
			wantErr:   true,
		},
		{
			name:      "forward slash",
			sessionID: "ses/123",
			wantErr:   true,
		},
		{
			name:      "backslash",
			sessionID: "ses\\123",
			wantErr:   true,
		},
		{
			name:      "traversal",
			sessionID: "../ses_123",
			wantErr:   true,
		},
		{
			name:      "nested traversal",
			sessionID: "a/../../etc/passwd",
			wantErr:   true,
		},
		{
			name:      "absolute path",
			sessionID: "/etc/passwd",
			wantErr:   true,
		},
		{
			name:      "hidden file",
			sessionID: ".hidden",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSessionID(tt.sessionID)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
