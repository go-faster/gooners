// Package opencode registers MCP tools that delegate work to an opencode server.
package opencode

import "encoding/json"

// Config contains connection settings for an opencode HTTP server.
type Config struct {
	BaseURL          string
	Username         string
	Password         string
	DefaultDirectory string
}

type Location struct {
	Directory string `json:"directory,omitempty" jsonschema:"Project directory to run opencode in. Overrides the server default directory."`
	Workspace string `json:"workspace,omitempty" jsonschema:"Optional opencode workspace identifier."`
}

type HealthResult struct {
	OK      bool            `json:"ok"`
	BaseURL string          `json:"base_url"`
	Data    json.RawMessage `json:"data,omitempty"`
	Message string          `json:"message,omitempty"`
}

type Agent struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Mode        string          `json:"mode,omitempty"`
	Model       string          `json:"model,omitempty"`
	Permission  string          `json:"permission,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

type AgentsResult struct {
	Agents []Agent `json:"agents"`
}

type ModelsResult struct {
	Providers []ProviderSummary `json:"providers,omitempty"`
	Models    []ModelSummary    `json:"models,omitempty"`
}

type ProviderSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Models int    `json:"models,omitempty"`
}

type ModelSummary struct {
	ProviderID string `json:"provider_id,omitempty"`
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
}

type Session struct {
	ID        string          `json:"id"`
	Title     string          `json:"title,omitempty"`
	ParentID  string          `json:"parent_id,omitempty"`
	CreatedAt int64           `json:"created_at,omitempty"`
	UpdatedAt int64           `json:"updated_at,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type SessionsResult struct {
	Sessions []Session `json:"sessions"`
}

type SessionsRequest struct {
	Location
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of sessions to return."`
	Order  string `json:"order,omitempty" jsonschema:"Optional sort order accepted by opencode, for example asc or desc."`
	Search string `json:"search,omitempty" jsonschema:"Optional search string."`
	Cursor string `json:"cursor,omitempty" jsonschema:"Optional pagination cursor."`
}

type CreateSessionRequest struct {
	ID       string    `json:"id,omitempty"`
	Title    string    `json:"title,omitempty"`
	ParentID string    `json:"parentID,omitempty"`
	Location *Location `json:"location,omitempty"`
	Agent    string    `json:"agent,omitempty"`
}

type PromptRequest struct {
	Prompt   PromptPayload `json:"prompt"`
	Delivery string        `json:"delivery,omitempty"`
	Resume   *bool         `json:"resume,omitempty"`
	Agent    string        `json:"agent,omitempty"`
	Model    *ModelRef     `json:"model,omitempty"`
}

type PromptPayload struct {
	Text string `json:"text"`
}

type ModelRef struct {
	ProviderID string `json:"providerID,omitempty"`
	ModelID    string `json:"modelID,omitempty"`
	Variant    string `json:"variant,omitempty"`
}

type HandoffRunResult struct {
	SessionID              string           `json:"session_id"`
	Status                 string           `json:"status"`
	FinalText              string           `json:"final_text,omitempty"`
	PromptMessageID        string           `json:"prompt_message_id,omitempty"`
	Messages               []MessageSummary `json:"messages,omitempty"`
	PendingPermissions     []RequestSummary `json:"pending_permissions,omitempty"`
	PendingQuestions       []RequestSummary `json:"pending_questions,omitempty"`
	PendingPermissionCount int              `json:"pending_permission_count"`
	PendingQuestionCount   int              `json:"pending_question_count"`
	MessagesReturned       int              `json:"messages_returned"`
	RawMessages            json.RawMessage  `json:"raw_messages,omitempty"`
	RawContext             json.RawMessage  `json:"raw_context,omitempty"`
	Message                string           `json:"message,omitempty"`
}

type HandoffFireResult struct {
	SessionID       string `json:"session_id"`
	PromptMessageID string `json:"prompt_message_id,omitempty"`
	Message         string `json:"message,omitempty"`
}

type HandoffCheckResult struct {
	SessionID              string           `json:"session_id"`
	FinalText              string           `json:"final_text,omitempty"`
	Messages               []MessageSummary `json:"messages,omitempty"`
	PendingPermissions     []RequestSummary `json:"pending_permissions,omitempty"`
	PendingQuestions       []RequestSummary `json:"pending_questions,omitempty"`
	PendingPermissionCount int              `json:"pending_permission_count"`
	PendingQuestionCount   int              `json:"pending_question_count"`
	MessagesReturned       int              `json:"messages_returned"`
	RawMessages            json.RawMessage  `json:"raw_messages,omitempty"`
	RawContext             json.RawMessage  `json:"raw_context,omitempty"`
	Message                string           `json:"message,omitempty"`
}

type MessageSummary struct {
	ID      string `json:"id,omitempty"`
	Role    string `json:"role,omitempty"`
	Text    string `json:"text,omitempty"`
	Preview string `json:"preview,omitempty"`
}

type RequestSummary struct {
	ID        string `json:"id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Title     string `json:"title,omitempty"`
	Text      string `json:"text,omitempty"`
	Preview   string `json:"preview,omitempty"`
}

type PermissionReplyResult struct {
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data,omitempty"`
}

type QuestionReplyResult struct {
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data,omitempty"`
}

type RequestsResult struct {
	Requests []RequestSummary `json:"requests"`
	Count    int              `json:"count"`
}
