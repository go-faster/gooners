package opencode

import (
	"encoding/json"
	"strings"
)

func firstText(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	texts := collectText(nil, v)
	if len(texts) == 0 {
		return ""
	}
	// Return the last text — the final assistant response, not intermediate parts.
	return texts[len(texts)-1]
}

func summarizeMessages(raw json.RawMessage, limit int) []MessageSummary {
	if limit <= 0 {
		limit = 6
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []MessageSummary{}
	}
	// Only include role-bearing message objects, not individual parts.
	items := collectObjects(nil, v)
	var roleItems []map[string]any
	for _, item := range items {
		if messageRole(item) != "" {
			roleItems = append(roleItems, item)
		}
	}
	if len(roleItems) > limit {
		roleItems = roleItems[len(roleItems)-limit:]
	}
	out := make([]MessageSummary, 0, len(roleItems))
	for _, item := range roleItems {
		text := strings.Join(collectText(nil, item), "\n")
		out = append(out, MessageSummary{
			ID:   messageID(item),
			Role: messageRole(item),
			Text: compactText(text),
		})
	}
	return out
}

func summarizeRequests(raw json.RawMessage, kind, sessionID string) []RequestSummary {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []RequestSummary{}
	}
	items := collectObjects(nil, v)
	out := make([]RequestSummary, 0, len(items))
	for _, item := range items {
		itemSessionID := firstStringField(item, "sessionID", "session_id")
		if sessionID != "" && itemSessionID != "" && itemSessionID != sessionID {
			continue
		}
		text := strings.Join(collectText(nil, item), "\n")
		out = append(out, RequestSummary{
			Kind:    kind,
			Title:   firstStringField(item, "title", "tool", "type", "action"),
			Text:    text,
			Preview: preview(text),
		})
	}
	return out
}

func extractMessageID(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	for _, obj := range collectObjects(nil, v) {
		if id := firstStringField(obj, "messageID", "message_id", "id"); id != "" {
			return id
		}
	}
	return ""
}

func collectText(out []string, v any) []string {
	switch x := v.(type) {
	case map[string]any:
		if text, ok := x["text"].(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
		for _, key := range []string{"content", "parts", "message", "messages", "data", "tool_result", "tool_use", "input", "output"} {
			if child, ok := x[key]; ok {
				out = collectText(out, child)
			}
		}
	case []any:
		for _, child := range x {
			out = collectText(out, child)
		}
	}
	return out
}

func collectObjects(out []map[string]any, v any) []map[string]any {
	switch x := v.(type) {
	case map[string]any:
		if x["id"] != nil || x["role"] != nil ||
			x["requestID"] != nil || x["messageID"] != nil ||
			x["request_id"] != nil || x["message_id"] != nil ||
			messageRole(x) != "" {
			out = append(out, x)
		}
		for _, key := range []string{"data", "items", "messages", "requests", "permissions", "questions", "content", "parts"} {
			if child, ok := x[key]; ok {
				out = collectObjects(out, child)
			}
		}
	case []any:
		for _, child := range x {
			out = collectObjects(out, child)
		}
	}
	return out
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v := stringField(m, key); v != "" {
			return v
		}
	}
	return ""
}

func messageID(m map[string]any) string {
	if id := stringField(m, "id"); id != "" {
		return id
	}
	if info, ok := m["info"].(map[string]any); ok {
		return stringField(info, "id")
	}
	return ""
}

func messageRole(m map[string]any) string {
	if role := stringField(m, "role"); role != "" {
		return role
	}
	if info, ok := m["info"].(map[string]any); ok {
		return stringField(info, "role")
	}
	return ""
}

func preview(s string) string {
	return truncateFields(s, 240)
}

func compactText(s string) string {
	return truncateFields(s, 1200)
}

func truncateText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

func truncateFields(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
