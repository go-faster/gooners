package opencode

import (
	"encoding/json"
	"strings"
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

func TestCollectTextTraversesToolBlocks(t *testing.T) {
	t.Parallel()
	// firstText returns the last collected text (final answer after tool use).
	raw := json.RawMessage(`{"data":[{"role":"assistant","tool_result":{"output":{"text":"tool output"}},"tool_use":{"input":{"text":"tool input"}}}]}`)
	got := firstText(raw)
	require.NotEmpty(t, got)
}

func TestSummaryOutputOmitsDuplicateAndRawFields(t *testing.T) {
	t.Parallel()
	msg := MessageSummary{ID: "msg_1", Role: "assistant", Text: strings.Repeat("a", 1201)}
	data, err := json.Marshal(struct {
		Health HealthResult          `json:"health"`
		Perm   PermissionReplyResult `json:"perm"`
		Msg    MessageSummary        `json:"msg"`
	}{
		Health: HealthResult{OK: true, Data: json.RawMessage(`{"raw":true}`)},
		Perm:   PermissionReplyResult{OK: true, Data: json.RawMessage(`{"raw":true}`)},
		Msg:    msg,
	})
	require.NoError(t, err)
	encoded := string(data)
	require.NotContains(t, encoded, "preview")
	require.NotContains(t, encoded, "raw")
}

func TestIsSessionFinishedJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"v2 finished", `[{"info":{"role":"assistant","finish":"stop"}}]`, true},
		{"v2 no finish", `[{"info":{"role":"assistant"}}]`, false},
		{"v2 empty finish", `[{"info":{"role":"assistant","finish":""}}]`, false},
		{"flat finished", `[{"role":"assistant","finish":"stop"}]`, true},
		{"flat no finish", `[{"role":"assistant"}]`, false},
		{"flat empty finish", `[{"role":"assistant","finish":""}]`, false},
		{"empty array", `[]`, false},
		{"not assistant v2", `[{"info":{"role":"user","finish":"stop"}}]`, false},
		{"not assistant flat", `[{"role":"user","finish":"stop"}]`, false},
		{"last assistant wins v2", `[{"info":{"role":"assistant","finish":"stop"}},{"info":{"role":"assistant"}}]`, false},
		{"last assistant wins flat", `[{"role":"assistant","finish":"stop"},{"role":"assistant"}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, isSessionFinishedJSON(json.RawMessage(tc.input)))
		})
	}
}

func TestFirstTextReturnsLastText(t *testing.T) {
	t.Parallel()
	// firstText returns the last text (final answer), not all texts joined.
	got := firstText(json.RawMessage(`{"data":[{"role":"assistant","content":[{"type":"text","text":"first"},{"text":"second"}]}]}`))
	require.Equal(t, "second", got)
}

func TestIntermediatePartsExcludedFromSummary(t *testing.T) {
	t.Parallel()
	// Parts without a role (intermediate model steps) must not appear in message summaries.
	// final_text should be the last text, not everything concatenated.
	raw := json.RawMessage(`[
		{"id":"msg_1","role":"user","content":[{"text":"hello"}]},
		{"id":"prt_1","text":"internal step"},
		{"id":"msg_2","role":"assistant","content":[{"text":"Hello."}]}
	]`)
	require.Equal(t, "Hello.", firstText(raw))
	msgs := summarizeMessages(raw, 10)
	require.Len(t, msgs, 2)
	require.Equal(t, "user", msgs[0].Role)
	require.Equal(t, "assistant", msgs[1].Role)
}

func TestMessageSummaryUsesNestedInfoRole(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
		"data":[
			{"info":{"id":"msg_1","role":"user"},"parts":[{"text":"hello"}]},
			{"id":"prt_1","text":"internal step"},
			{"info":{"id":"msg_2","role":"assistant"},"parts":[{"text":"done"}]}
		]
	}`)

	msgs := summarizeMessages(raw, 10)
	require.Len(t, msgs, 2)
	require.Equal(t, MessageSummary{ID: "msg_1", Role: "user", Text: "hello"}, msgs[0])
	require.Equal(t, MessageSummary{ID: "msg_2", Role: "assistant", Text: "done"}, msgs[1])
}
