// Package opencode registers MCP tools that delegate work to an opencode server.
package opencode

import (
	"encoding/json"
	"log/slog"
	"time"
)

// Config contains connection settings for an opencode HTTP server.
type Config struct {
	BaseURL          string
	Username         string
	Password         string
	DefaultDirectory string
	// SyncTimeout is the timeout for blocking prompt calls (session message POST).
	// If zero, the general request timeout is used.
	SyncTimeout time.Duration
	// APILogger, when non-nil, logs every HTTP request and response body at
	// debug level. Useful for debugging opencode API calls.
	APILogger *slog.Logger
}

type Location struct {
	Directory string `json:"directory,omitempty" jsonschema:"Project directory to run opencode in. Overrides the server default directory."`
	Workspace string `json:"workspace,omitempty" jsonschema:"Optional opencode workspace identifier."`
}

type HealthResult struct {
	OK      bool            `json:"ok"`
	BaseURL string          `json:"base_url"`
	Data    json.RawMessage `json:"-"`
	Message string          `json:"message,omitempty"`
}

type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Mode        string `json:"mode,omitempty"`
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
}

type CreateSessionRequest struct {
	Title      string `json:"title,omitempty"`
	ParentID   string `json:"parentID,omitempty"`
	ProviderID string `json:"providerID,omitempty"`
	ModelID    string `json:"modelID,omitempty"`
}

type PromptRequest struct {
	Prompt PromptPayload `json:"prompt"`
	Agent  string        `json:"agent,omitempty"`
}

type PromptPayload struct {
	Text string `json:"text"`
}

type CreateSessionResult struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
}

type HandoffFireResult struct {
	SessionID          string           `json:"session_id"`
	PromptMessageID    string           `json:"prompt_message_id,omitempty"`
	Message            string           `json:"message,omitempty"`
	PendingPermissions []RequestSummary `json:"pending_permissions,omitempty"`
	PendingQuestions   []RequestSummary `json:"pending_questions,omitempty"`
	Errors             []string         `json:"errors,omitempty"`
}

type HandoffCheckResult struct {
	SessionID          string           `json:"session_id"`
	Status             string           `json:"status,omitempty"`
	FinalText          string           `json:"final_text,omitempty"`
	PendingPermissions []RequestSummary `json:"pending_permissions,omitempty"`
	PendingQuestions   []RequestSummary `json:"pending_questions,omitempty"`
	JobError           string           `json:"job_error,omitempty"`
	Errors             []string         `json:"errors,omitempty"`
	Messages           []MessageSummary `json:"messages,omitempty"`
}

type MessageSummary struct {
	ID   string `json:"id,omitempty"`
	Role string `json:"role,omitempty"`
	Text string `json:"text,omitempty"`
}

type RequestSummary struct {
	Kind    string `json:"kind,omitempty"`
	Title   string `json:"title,omitempty"`
	Text    string `json:"text,omitempty"`
	Preview string `json:"preview,omitempty"`
}

type PermissionReplyResult struct {
	OK                 bool             `json:"ok"`
	Data               json.RawMessage  `json:"-"`
	PendingPermissions []RequestSummary `json:"pending_permissions,omitempty"`
	PendingQuestions   []RequestSummary `json:"pending_questions,omitempty"`
	Errors             []string         `json:"errors,omitempty"`
}

type QuestionReplyResult struct {
	OK                 bool             `json:"ok"`
	Data               json.RawMessage  `json:"-"`
	PendingPermissions []RequestSummary `json:"pending_permissions,omitempty"`
	PendingQuestions   []RequestSummary `json:"pending_questions,omitempty"`
	Errors             []string         `json:"errors,omitempty"`
}

type RequestsResult struct {
	Requests []RequestSummary `json:"requests"`
	Count    int              `json:"count"`
}
