package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// Register adds opencode handoff tools to an MCP server.
func Register(s *mcp.Server, client *Client, mgr *Manager) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_health",
		Description: "Check connectivity to the configured opencode HTTP server. Call this if other handoff tools return connection or authentication errors.",
		Flags:       mcputil.ReadOnly,
	}, healthHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_agents",
		Description: "List opencode agents available for a directory/workspace.",
		Flags:       mcputil.ReadOnly,
	}, agentsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_models",
		Description: "List opencode providers and optionally models. Supports substring, glob (e.g. 'openai/gpt-*-mini'), or regex filtering and a result limit to avoid cluttering context with large provider catalogs (e.g. OpenRouter).",
		Flags:       mcputil.ReadOnly,
	}, modelsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_sessions",
		Description: "List opencode sessions visible in a directory/workspace.",
		Flags:       mcputil.ReadOnly,
	}, sessionsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_fire",
		Description: "Background handoff: create or reuse an opencode session, submit a prompt, and return immediately. Use handoff_check with the returned session_id to poll progress.",
	}, fireHandler(mgr))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_check",
		Description: "Poll progress for a session_id returned by handoff_fire, or inspect sessions for pending permissions/questions.",
		Flags:       mcputil.ReadOnly,
	}, checkHandler(client, mgr))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_permission_reply",
		Description: "Reply to an opencode permission request for a session. Use handoff_check to see pending permissions.",
	}, permissionReplyHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_question_reply",
		Description: "Reply to or reject an opencode clarification question for a session. Use handoff_check to see pending questions.",
	}, questionReplyHandler(client))
}

type locationParams struct {
	Directory string `json:"directory,omitempty" jsonschema:"Project directory for opencode location scoping."`
	Workspace string `json:"workspace,omitempty" jsonschema:"Optional opencode workspace identifier."`
}

func (p locationParams) location() Location {
	return Location(p)
}

func healthHandler(client *Client) mcp.ToolHandlerFor[struct{}, HealthResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, HealthResult, error) {
		data, err := client.Health(ctx)
		if err != nil {
			return nil, HealthResult{OK: false, BaseURL: client.BaseURL(), Message: err.Error()}, nil
		}
		return nil, HealthResult{OK: true, BaseURL: client.BaseURL(), Data: data, Message: "opencode server is reachable"}, nil
	}
}

func agentsHandler(client *Client) mcp.ToolHandlerFor[locationParams, AgentsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args locationParams) (*mcp.CallToolResult, AgentsResult, error) {
		agents, err := client.Agents(ctx, args.location())
		if err != nil {
			return nil, AgentsResult{}, err
		}
		return nil, AgentsResult{Agents: agents}, nil
	}
}

type modelsParams struct {
	locationParams
	IncludeModels bool   `json:"include_models,omitempty" jsonschema:"If true, includes the list of individual models. Defaults to false."`
	Filter        string `json:"filter,omitempty" jsonschema:"Optional substring, glob, or regex to filter models (e.g., 'xai/', 'openai/gpt-*-mini'). Implies include_models=true."`
	Limit         int    `json:"limit,omitempty" jsonschema:"Maximum number of models to return when include_models is true. Defaults to 50; pass -1 for no limit. Zero is treated as the default."`
}

func modelsHandler(client *Client) mcp.ToolHandlerFor[modelsParams, ModelsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args modelsParams) (*mcp.CallToolResult, ModelsResult, error) {
		res, err := client.ProvidersAndModels(ctx, args.location())
		if err != nil {
			return nil, ModelsResult{}, err
		}

		includeModels := args.IncludeModels || args.Filter != ""
		if !includeModels {
			res.Models = nil
			return nil, res, nil
		}

		if args.Filter != "" {
			var filtered []ModelSummary
			for _, m := range res.Models {
				if matchModel(args.Filter, m) {
					filtered = append(filtered, m)
				}
			}
			res.Models = filtered
		}

		limit := 50
		if args.Limit != 0 {
			limit = args.Limit
		}

		if limit > 0 && len(res.Models) > limit {
			res.Models = res.Models[:limit]
		}

		return nil, res, nil
	}
}

// matchModel reports whether m matches filter using substring, then glob
// (path.Match), then regex — in that order. Regex is only attempted when
// neither substring nor glob matches, so a glob like "openai/gpt-*-mini" is
// never re-interpreted as a regex. Substring matching means a bare "xai/"
// matches all xai models without needing the "xai/*" glob form.
func matchModel(filter string, m ModelSummary) bool {
	if filter == "" {
		return true
	}

	fullID := m.ProviderID + "/" + m.ID

	if strings.Contains(fullID, filter) || strings.Contains(m.ID, filter) || strings.Contains(m.Name, filter) {
		return true
	}

	if matched, _ := path.Match(filter, fullID); matched {
		return true
	}
	if matched, _ := path.Match(filter, m.ID); matched {
		return true
	}

	if re, err := regexp.Compile(filter); err == nil {
		if re.MatchString(fullID) || re.MatchString(m.ID) || re.MatchString(m.Name) {
			return true
		}
	}

	return false
}

func sessionsHandler(client *Client) mcp.ToolHandlerFor[SessionsRequest, SessionsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args SessionsRequest) (*mcp.CallToolResult, SessionsResult, error) {
		res, err := client.Sessions(ctx, args)
		if err != nil {
			return nil, SessionsResult{}, err
		}
		return nil, res, nil
	}
}

type fireParams struct {
	locationParams
	Prompt      string `json:"prompt" jsonschema:"Task to delegate to opencode."`
	Title       string `json:"title,omitempty" jsonschema:"Optional session title."`
	Agent       string `json:"agent,omitempty" jsonschema:"Optional opencode agent name."`
	ProviderID  string `json:"provider_id,omitempty" jsonschema:"Optional model provider id."`
	ModelID     string `json:"model_id,omitempty" jsonschema:"Optional model id."`
	Verbose     bool   `json:"verbose,omitempty" jsonschema:"Include raw messages/context returned by opencode."`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Max seconds to wait for completion (0-300). 0 = fire and return immediately."`
	SessionID   string `json:"session_id,omitempty" jsonschema:"Existing session id to reuse; omitted means create a new session."`
}

func fireHandler(mgr *Manager) mcp.ToolHandlerFor[fireParams, HandoffFireResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args fireParams) (*mcp.CallToolResult, HandoffFireResult, error) {
		if args.Prompt == "" {
			return nil, HandoffFireResult{}, fmt.Errorf("prompt is required")
		}
		loc := args.location()

		title := args.Title
		if title != "" {
			title = "[handoff] " + title
		}

		sessionID, err := mgr.Submit(ctx,
			loc,
			args.SessionID,
			CreateSessionRequest{Title: title},
			promptRequest(args),
		)
		if err != nil {
			return nil, HandoffFireResult{}, err
		}
		return nil, HandoffFireResult{SessionID: sessionID, Message: "prompt submitted; use handoff_check with this session_id"}, nil
	}
}

type checkParams struct {
	locationParams
	SessionID   string `json:"session_id" jsonschema:"opencode session id."`
	Verbose     bool   `json:"verbose,omitempty" jsonschema:"Include raw messages/context returned by opencode."`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Max seconds to wait for completion (0-300). 0 = no wait."`
}

func checkHandler(client *Client, mgr *Manager) mcp.ToolHandlerFor[checkParams, HandoffCheckResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args checkParams) (*mcp.CallToolResult, HandoffCheckResult, error) {
		if args.WaitSeconds > 0 {
			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(min(args.WaitSeconds, 300))*time.Second)
			defer cancel()
			_ = client.Wait(waitCtx, args.location(), args.SessionID)
		}

		res, err := doCheck(ctx, client, mgr, args)
		return nil, res, err
	}
}

func doCheck(ctx context.Context, client *Client, mgr *Manager, args checkParams) (HandoffCheckResult, error) {
	loc := args.location()
	job, ok := mgr.Job(args.SessionID)

	res, isFinished, err := checkSession(ctx, client, loc, args.SessionID, args.Verbose)
	if err != nil {
		return HandoffCheckResult{}, err
	}

	// No tracked job (external session or different server instance): report
	// whatever opencode says and surface any pending permissions/questions.
	if !ok {
		res.Status = sessionStatus(isFinished)
		return res, nil
	}

	if job.Status == JobRunning || job.Status == JobUnknown {
		res.Status = sessionStatus(isFinished)
		return res, nil
	}

	// A job may be in error state because the HTTP POST timed out, yet the
	// opencode session may have continued. Trust the message stream over the
	// job error when the session is demonstrably finished.
	if isFinished {
		res.Status = string(JobDone)
		return res, nil
	}

	res.Status = string(job.Status)
	if job.Err != nil {
		res.JobError = job.Err.Error()
	}
	return res, nil
}

// sessionStatus maps the isFinished flag to a status string for sessions that
// are not tracked by this server instance.
func sessionStatus(isFinished bool) string {
	if isFinished {
		return string(JobDone)
	}
	return string(JobRunning)
}

type permissionReplyParams struct {
	locationParams
	SessionID string `json:"session_id" jsonschema:"opencode session id."`
	Reply     string `json:"reply" jsonschema:"permission reply value, for example once, always, reject, or deny depending on opencode API."`
	Message   string `json:"message,omitempty" jsonschema:"Optional explanation."`
}

func permissionReplyHandler(client *Client) mcp.ToolHandlerFor[permissionReplyParams, PermissionReplyResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args permissionReplyParams) (*mcp.CallToolResult, PermissionReplyResult, error) {
		reqs, err := client.SessionPermissionRequests(ctx, args.location(), args.SessionID)
		if err != nil {
			return nil, PermissionReplyResult{}, err
		}
		if len(reqs) == 0 {
			return nil, PermissionReplyResult{}, fmt.Errorf("no pending permission requests found for session %s", args.SessionID)
		}
		requestID := ""
		if idVal, ok := reqs[0]["id"].(string); ok {
			requestID = idVal
		} else if idVal, ok := reqs[0]["requestID"].(string); ok {
			requestID = idVal
		} else if idVal, ok := reqs[0]["request_id"].(string); ok {
			requestID = idVal
		}
		if requestID == "" {
			return nil, PermissionReplyResult{}, fmt.Errorf("could not extract request ID from pending permission")
		}

		res, err := client.PermissionReply(ctx, args.location(), args.SessionID, requestID, args.Reply, args.Message)
		if err != nil {
			return nil, PermissionReplyResult{}, err
		}
		return nil, PermissionReplyResult{OK: true, Data: res}, nil
	}
}

type questionReplyParams struct {
	locationParams
	SessionID string     `json:"session_id" jsonschema:"opencode session id."`
	Answers   [][]string `json:"answers,omitempty" jsonschema:"Answer selections: each inner array is selected labels for one question."`
	Reject    bool       `json:"reject,omitempty" jsonschema:"Reject the question instead of answering it."`
}

func questionReplyHandler(client *Client) mcp.ToolHandlerFor[questionReplyParams, QuestionReplyResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args questionReplyParams) (*mcp.CallToolResult, QuestionReplyResult, error) {
		reqs, err := client.SessionQuestionRequests(ctx, args.location(), args.SessionID)
		if err != nil {
			return nil, QuestionReplyResult{}, err
		}
		if len(reqs) == 0 {
			return nil, QuestionReplyResult{}, fmt.Errorf("no pending question requests found for session %s", args.SessionID)
		}
		requestID := ""
		if idVal, ok := reqs[0]["id"].(string); ok {
			requestID = idVal
		} else if idVal, ok := reqs[0]["requestID"].(string); ok {
			requestID = idVal
		} else if idVal, ok := reqs[0]["request_id"].(string); ok {
			requestID = idVal
		}
		if requestID == "" {
			return nil, QuestionReplyResult{}, fmt.Errorf("could not extract request ID from pending question")
		}

		res, err := client.QuestionReply(ctx, args.location(), args.SessionID, requestID, args.Reject, args.Answers)
		if err != nil {
			return nil, QuestionReplyResult{}, err
		}
		return nil, QuestionReplyResult{OK: true, Data: res}, nil
	}
}

func promptRequest(args fireParams) PromptRequest {
	req := PromptRequest{
		Prompt: PromptPayload{Text: args.Prompt},
		Agent:  args.Agent,
	}
	if args.ProviderID != "" || args.ModelID != "" {
		req.Model = &ModelRef{ProviderID: args.ProviderID, ModelID: args.ModelID}
	}
	return req
}

func checkSession(ctx context.Context, client *Client, loc Location, sessionID string, verbose bool) (HandoffCheckResult, bool, error) {
	if sessionID == "" {
		return HandoffCheckResult{}, false, fmt.Errorf("session_id is required")
	}
	res := HandoffCheckResult{SessionID: sessionID}
	var isFinished bool
	msg, msgErr := client.Messages(ctx, loc, sessionID)
	if msgErr == nil {
		summaries := summarizeMessages(msg, 6)
		if verbose {
			res.Messages = summaries
		}
		res.FinalText = truncateText(firstText(msg), 4000)
		isFinished = isSessionFinishedJSON(msg)
	}
	ctxData, ctxErr := client.Context(ctx, loc, sessionID)
	if ctxErr == nil {
		if res.FinalText == "" {
			res.FinalText = truncateText(firstText(ctxData), 4000)
		}
	}
	fillPendingRequests(ctx, client, loc, &res)
	if msgErr != nil && ctxErr != nil {
		return HandoffCheckResult{}, false, fmt.Errorf("read session %q messages: %w; context: %w", sessionID, msgErr, ctxErr)
	}
	return res, isFinished, nil
}

func fillPendingRequests(ctx context.Context, client *Client, loc Location, res *HandoffCheckResult) {
	perms, err := client.Permissions(ctx, loc, res.SessionID)
	if err == nil {
		res.PendingPermissions = summarizeRequests(perms, "permission", res.SessionID)
	} else {
		res.Errors = append(res.Errors, fmt.Sprintf("get permission requests: %s", err))
	}

	questions, err := client.Questions(ctx, loc, res.SessionID)
	if err == nil {
		res.PendingQuestions = summarizeRequests(questions, "question", res.SessionID)
	} else {
		res.Errors = append(res.Errors, fmt.Sprintf("get questions: %s", err))
	}
}

func isSessionFinishedJSON(raw json.RawMessage) bool {
	// Try v2 format: messages are objects with an "info" wrapper.
	var v2 []struct {
		Info struct {
			Role   string  `json:"role"`
			Finish *string `json:"finish"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &v2); err == nil {
		for _, msg := range slices.Backward(v2) {
			if msg.Info.Role == "assistant" {
				return msg.Info.Finish != nil && *msg.Info.Finish != ""
			}
		}
	}
	// Try flat format (instance route): role and finish are top-level fields.
	var flat []struct {
		Role   string  `json:"role"`
		Finish *string `json:"finish"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil {
		for _, msg := range slices.Backward(flat) {
			if msg.Role == "assistant" {
				return msg.Finish != nil && *msg.Finish != ""
			}
		}
	}
	return false
}
