package opencode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// withPartsMessages is a realistic payload from the opencode instance route
// GET /session/:id/message, which returns Schema.Array(SessionV1.WithParts).
//
// Schema (packages/core/src/v1/session.ts):
//
//	WithParts = { info: Info, parts: Part[] }
//	Info      = User | Assistant  (discriminated by "role")
//	Assistant = { role: "assistant", id, finish?, modelID, providerID, cost, tokens, … }
//	Part      = TextPart | ToolPart | …  (discriminated by "type")
const withPartsMessages = `[
  {
    "info": {
      "id": "msg_user_01",
      "role": "user",
      "path": {"cwd": "/project", "root": "/project"}
    },
    "parts": [
      {"type": "text", "id": "prt_01", "sessionID": "ses_1", "messageID": "msg_user_01",
       "text": "refactor the auth module"}
    ]
  },
  {
    "info": {
      "id": "msg_asst_01",
      "role": "assistant",
      "modelID": "claude-sonnet-4-5",
      "providerID": "anthropic",
      "mode": "default",
      "agent": "coder",
      "path": {"cwd": "/project", "root": "/project"},
      "cost": 0.0042,
      "tokens": {"input": 1500, "output": 800, "reasoning": 0, "cache": {"read": 0, "write": 0}},
      "finish": "end_turn"
    },
    "parts": [
      {"type": "text", "id": "prt_02", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "text": "I have refactored the auth module."}
    ]
  }
]`

// withPartsRunning is the same shape but the assistant message has no finish yet.
const withPartsRunning = `[
  {
    "info": {
      "id": "msg_user_01",
      "role": "user",
      "path": {"cwd": "/project", "root": "/project"}
    },
    "parts": [
      {"type": "text", "id": "prt_01", "sessionID": "ses_1", "messageID": "msg_user_01",
       "text": "refactor the auth module"}
    ]
  },
  {
    "info": {
      "id": "msg_asst_01",
      "role": "assistant",
      "modelID": "claude-sonnet-4-5",
      "providerID": "anthropic",
      "mode": "default",
      "agent": "coder",
      "path": {"cwd": "/project", "root": "/project"},
      "cost": 0,
      "tokens": {"input": 500, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}}
    },
    "parts": [
      {"type": "text", "id": "prt_02", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "text": "Working on it…"}
    ]
  }
]`

// TestWithPartsFinishedDetection verifies that isSessionFinishedJSON correctly
// reads the real opencode WithParts format returned by GET /session/:id/message.
func TestWithPartsFinishedDetection(t *testing.T) {
	t.Parallel()
	t.Run("finished", func(t *testing.T) {
		t.Parallel()
		require.True(t, isSessionFinishedJSON(json.RawMessage(withPartsMessages)))
	})
	t.Run("running", func(t *testing.T) {
		t.Parallel()
		require.False(t, isSessionFinishedJSON(json.RawMessage(withPartsRunning)))
	})
}

// TestWithPartsSummarizeMessages verifies that summarizeMessages can extract
// role and text from the real WithParts format.
func TestWithPartsSummarizeMessages(t *testing.T) {
	t.Parallel()
	msgs := summarizeMessages(json.RawMessage(withPartsMessages), 10)
	require.Len(t, msgs, 2)

	require.Equal(t, "user", msgs[0].Role)
	require.Contains(t, msgs[0].Text, "refactor")

	require.Equal(t, "assistant", msgs[1].Role)
	require.Contains(t, msgs[1].Text, "refactored")
}

// TestWithPartsFirstText verifies that firstText returns the assistant's reply
// from the real WithParts format.
func TestWithPartsFirstText(t *testing.T) {
	t.Parallel()
	got := firstText(json.RawMessage(withPartsMessages))
	require.Contains(t, got, "refactored")
}
