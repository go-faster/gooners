package opencode

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

func parseAgents(body json.RawMessage) ([]Agent, error) {
	var byName map[string]json.RawMessage
	if err := json.Unmarshal(body, &byName); err == nil {
		agents := make([]Agent, 0, len(byName))
		for name, raw := range byName {
			agents = append(agents, parseAgent(name, raw))
		}
		sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
		return agents, nil
	}

	var list []json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	agents := make([]Agent, 0, len(list))
	for _, raw := range list {
		agents = append(agents, parseAgent("", raw))
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents, nil
}

func parseAgent(name string, raw json.RawMessage) Agent {
	var obj struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Mode        string          `json:"mode"`
		Model       json.RawMessage `json:"model"`
		Permission  json.RawMessage `json:"permission"`
	}
	_ = json.Unmarshal(raw, &obj)
	if name == "" {
		name = obj.Name
	}
	return Agent{
		Name:        name,
		Description: obj.Description,
		Mode:        obj.Mode,
		Model:       compactJSON(obj.Model),
		Permission:  compactJSON(obj.Permission),
		Raw:         raw,
	}
}

func summarizeProviders(body json.RawMessage) []ProviderSummary {
	var byID map[string]json.RawMessage
	if err := json.Unmarshal(body, &byID); err == nil {
		out := make([]ProviderSummary, 0, len(byID))
		for id, raw := range byID {
			out = append(out, parseProvider(id, raw))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	var list []json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil {
		return []ProviderSummary{}
	}
	out := make([]ProviderSummary, 0, len(list))
	for _, raw := range list {
		out = append(out, parseProvider("", raw))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func parseProvider(id string, raw json.RawMessage) ProviderSummary {
	var obj struct {
		ID     string          `json:"id"`
		Name   string          `json:"name"`
		Models json.RawMessage `json:"models"`
	}
	_ = json.Unmarshal(raw, &obj)
	if id == "" {
		id = obj.ID
	}
	return ProviderSummary{ID: id, Name: obj.Name, Models: countJSONItems(obj.Models)}
}

func summarizeModels(body json.RawMessage) []ModelSummary {
	var byProvider map[string]json.RawMessage
	if err := json.Unmarshal(body, &byProvider); err == nil {
		out := []ModelSummary{}
		for providerID, raw := range byProvider {
			out = append(out, summarizeProviderModels(providerID, raw)...)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].ProviderID == out[j].ProviderID {
				return out[i].ID < out[j].ID
			}
			return out[i].ProviderID < out[j].ProviderID
		})
		return out
	}
	return summarizeProviderModels("", body)
}

func summarizeProviderModels(providerID string, raw json.RawMessage) []ModelSummary {
	var byID map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byID); err == nil {
		out := make([]ModelSummary, 0, len(byID))
		for id, modelRaw := range byID {
			out = append(out, parseModel(providerID, id, modelRaw))
		}
		return out
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return []ModelSummary{}
	}
	out := make([]ModelSummary, 0, len(list))
	for _, modelRaw := range list {
		out = append(out, parseModel(providerID, "", modelRaw))
	}
	return out
}

func parseModel(providerID, id string, raw json.RawMessage) ModelSummary {
	var obj struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ProviderID string `json:"providerID"`
	}
	_ = json.Unmarshal(raw, &obj)
	if id == "" {
		id = obj.ID
	}
	if providerID == "" {
		providerID = obj.ProviderID
	}
	return ModelSummary{ProviderID: providerID, ID: id, Name: obj.Name}
}

func parseSessions(body json.RawMessage) ([]Session, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(body, &list); err == nil {
		return parseSessionList(list), nil
	}
	var wrapper struct {
		Sessions []json.RawMessage `json:"sessions"`
		Items    []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	items := wrapper.Sessions
	if len(items) == 0 {
		items = wrapper.Items
	}
	return parseSessionList(items), nil
}

func parseSessionList(list []json.RawMessage) []Session {
	sessions := make([]Session, 0, len(list))
	for _, raw := range list {
		if session, ok := parseSession(raw); ok {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

func parseSession(raw json.RawMessage) (Session, bool) {
	var obj struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		ParentID string `json:"parentID"`
		Time     struct {
			Created int64 `json:"created"`
			Updated int64 `json:"updated"`
		} `json:"time"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.ID == "" {
		return Session{}, false
	}
	return Session{
		ID:        obj.ID,
		Title:     obj.Title,
		ParentID:  obj.ParentID,
		CreatedAt: obj.Time.Created,
		UpdatedAt: obj.Time.Updated,
		Raw:       raw,
	}, true
}

func firstText(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	texts := collectText(nil, v)
	if len(texts) == 0 {
		return ""
	}
	return strings.Join(texts, "\n")
}

func summarizeMessages(raw json.RawMessage, limit int) []MessageSummary {
	if limit <= 0 {
		limit = 6
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []MessageSummary{}
	}
	items := collectObjects(nil, v)
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	out := make([]MessageSummary, 0, len(items))
	for _, item := range items {
		text := strings.Join(collectText(nil, item), "\n")
		if text == "" && item["role"] == nil {
			continue
		}
		out = append(out, MessageSummary{
			ID:   stringField(item, "id"),
			Role: stringField(item, "role"),
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
			ID:        firstStringField(item, "id", "requestID", "request_id"),
			SessionID: itemSessionID,
			Kind:      kind,
			Title:     firstStringField(item, "title", "tool", "type", "action"),
			Text:      text,
			Preview:   preview(text),
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
		if x["id"] != nil || x["role"] != nil || x["requestID"] != nil || x["messageID"] != nil {
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

func preview(s string) string {
	return truncateFields(s, 240)
}

func compactText(s string) string {
	return truncateFields(s, 1200)
}

func truncateFields(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func countJSONItems(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err == nil {
		return len(list)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		return len(m)
	}
	return 0
}
