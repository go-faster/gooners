package opencode

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// RegisterOptions controls opencode handoff tool behavior.
type RegisterOptions struct {
	WaitTimeout time.Duration
}

// Register adds opencode handoff tools to an MCP server.
func Register(s *mcp.Server, client *Client, opts RegisterOptions) {
	if opts.WaitTimeout <= 0 {
		opts.WaitTimeout = 10 * time.Minute
	}

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
		Description: "List opencode providers and models available for a directory/workspace.",
		Flags:       mcputil.ReadOnly,
	}, modelsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_sessions",
		Description: "List opencode sessions visible in a directory/workspace.",
		Flags:       mcputil.ReadOnly,
	}, sessionsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_run",
		Description: "Blocking handoff: create an opencode session, submit a prompt to an agent, wait for completion, and return a compact result. Requires opencode v2 POST /api/session.",
	}, runHandler(client, opts.WaitTimeout))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_fire",
		Description: "Background handoff: create or reuse an opencode session, submit a prompt, and return immediately. Use handoff_check with the returned session_id to poll progress.",
	}, fireHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_check",
		Description: "Poll progress for a session_id returned by handoff_fire, or inspect blocked handoff_run/handoff_wait sessions for pending permissions/questions.",
		Flags:       mcputil.ReadOnly,
	}, checkHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_wait",
		Description: "Wait for an existing opencode session to become idle. If it times out or is blocked, returns compact session state and next-step hints instead of discarding progress.",
	}, waitHandler(client, opts.WaitTimeout))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_permissions",
		Description: "List pending opencode permission requests globally or for one session.",
		Flags:       mcputil.ReadOnly,
	}, permissionsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_permission_reply",
		Description: "Reply to an opencode permission request for a session.",
	}, permissionReplyHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_questions",
		Description: "List pending opencode clarification questions.",
		Flags:       mcputil.ReadOnly,
	}, questionsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_question_reply",
		Description: "Reply to or reject an opencode clarification question for a session.",
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

func modelsHandler(client *Client) mcp.ToolHandlerFor[locationParams, ModelsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args locationParams) (*mcp.CallToolResult, ModelsResult, error) {
		loc := args.location()
		providers, err := client.Providers(ctx, loc)
		if err != nil {
			return nil, ModelsResult{}, err
		}
		models, err := client.Models(ctx, loc)
		if err != nil {
			return nil, ModelsResult{}, err
		}
		return nil, ModelsResult{Providers: summarizeProviders(providers), Models: summarizeModels(models)}, nil
	}
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

type runParams struct {
	locationParams
	Prompt          string `json:"prompt" jsonschema:"Task to delegate to opencode."`
	Title           string `json:"title,omitempty" jsonschema:"Optional session title."`
	Agent           string `json:"agent,omitempty" jsonschema:"Optional opencode agent name."`
	ProviderID      string `json:"provider_id,omitempty" jsonschema:"Optional model provider id."`
	ModelID         string `json:"model_id,omitempty" jsonschema:"Optional model id."`
	Variant         string `json:"variant,omitempty" jsonschema:"Optional model variant."`
	Delivery        string `json:"delivery,omitempty" jsonschema:"Prompt delivery mode, usually queue."`
	ParentSessionID string `json:"parent_session_id,omitempty" jsonschema:"Optional parent session id."`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty" jsonschema:"Wait timeout in seconds."`
	Resume          *bool  `json:"resume,omitempty" jsonschema:"Whether opencode should resume interrupted prompt execution."`
	Verbose         bool   `json:"verbose,omitempty" jsonschema:"Include raw messages/context returned by opencode."`
}

type fireParams struct {
	runParams
	SessionID string `json:"session_id,omitempty" jsonschema:"Existing session id to reuse; omitted means create a new session."`
}

func runHandler(client *Client, defaultWait time.Duration) mcp.ToolHandlerFor[runParams, HandoffRunResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args runParams) (*mcp.CallToolResult, HandoffRunResult, error) {
		if args.Prompt == "" {
			return nil, HandoffRunResult{}, fmt.Errorf("prompt is required")
		}
		loc := args.location()
		session, err := client.CreateSession(ctx, loc, CreateSessionRequest{Title: args.Title, ParentID: args.ParentSessionID, Agent: args.Agent})
		if err != nil {
			return nil, HandoffRunResult{}, err
		}
		prompt, err := client.Prompt(ctx, loc, session.ID, promptRequest(args))
		if err != nil {
			return nil, HandoffRunResult{}, err
		}
		waitCtx, cancel := waitContext(ctx, args.TimeoutSeconds, defaultWait)
		defer cancel()
		if _, err := client.Wait(waitCtx, loc, session.ID); err != nil {
			check, checkErr := checkSession(ctx, client, loc, session.ID, args.Verbose)
			if checkErr != nil {
				return nil, HandoffRunResult{}, fmt.Errorf("wait for session %q: %w", session.ID, err)
			}
			return nil, runResultFromCheck(session.ID, "blocked_or_running", extractMessageID(prompt), check, fmt.Sprintf("session did not finish cleanly: %v; call handoff_wait with session_id to retry, or handoff_permissions if the session is blocked", err)), nil
		}
		check, err := checkSession(ctx, client, loc, session.ID, args.Verbose)
		if err != nil {
			return nil, HandoffRunResult{}, err
		}
		return nil, runResultFromCheck(session.ID, "completed", extractMessageID(prompt), check, ""), nil
	}
}

func fireHandler(client *Client) mcp.ToolHandlerFor[fireParams, HandoffFireResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args fireParams) (*mcp.CallToolResult, HandoffFireResult, error) {
		if args.Prompt == "" {
			return nil, HandoffFireResult{}, fmt.Errorf("prompt is required")
		}
		loc := args.location()
		sessionID := args.SessionID
		if sessionID == "" {
			session, err := client.CreateSession(ctx, loc, CreateSessionRequest{Title: args.Title, ParentID: args.ParentSessionID, Agent: args.Agent})
			if err != nil {
				return nil, HandoffFireResult{}, err
			}
			sessionID = session.ID
		}
		prompt, err := client.Prompt(ctx, loc, sessionID, promptRequest(args.runParams))
		if err != nil {
			return nil, HandoffFireResult{}, err
		}
		return nil, HandoffFireResult{SessionID: sessionID, PromptMessageID: extractMessageID(prompt), Message: "prompt submitted; use handoff_check or handoff_wait with this session_id"}, nil
	}
}

type sessionParams struct {
	locationParams
	SessionID      string `json:"session_id" jsonschema:"opencode session id."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Wait timeout in seconds."`
	Verbose        bool   `json:"verbose,omitempty" jsonschema:"Include raw messages/context returned by opencode."`
}

type checkParams struct {
	locationParams
	SessionID string `json:"session_id" jsonschema:"opencode session id."`
	Verbose   bool   `json:"verbose,omitempty" jsonschema:"Include raw messages/context returned by opencode."`
}

type requestListParams struct {
	locationParams
	SessionID string `json:"session_id,omitempty" jsonschema:"Optional opencode session id. Omit to list global pending requests when supported by opencode."`
}

func checkHandler(client *Client) mcp.ToolHandlerFor[checkParams, HandoffCheckResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args checkParams) (*mcp.CallToolResult, HandoffCheckResult, error) {
		res, err := checkSession(ctx, client, args.location(), args.SessionID, args.Verbose)
		if err != nil {
			return nil, HandoffCheckResult{}, err
		}
		return nil, res, nil
	}
}

func waitHandler(client *Client, defaultWait time.Duration) mcp.ToolHandlerFor[sessionParams, HandoffRunResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args sessionParams) (*mcp.CallToolResult, HandoffRunResult, error) {
		loc := args.location()
		waitCtx, cancel := waitContext(ctx, args.TimeoutSeconds, defaultWait)
		defer cancel()
		if _, err := client.Wait(waitCtx, loc, args.SessionID); err != nil {
			check, checkErr := checkSession(ctx, client, loc, args.SessionID, args.Verbose)
			if checkErr != nil {
				return nil, HandoffRunResult{}, fmt.Errorf("wait for session %q: %w", args.SessionID, err)
			}
			msg := fmt.Sprintf("session did not finish cleanly: %v; call handoff_wait with session_id to retry, or handoff_permissions if the session is blocked", err)
			return nil, runResultFromCheck(args.SessionID, "blocked_or_running", "", check, msg), nil
		}
		check, err := checkSession(ctx, client, loc, args.SessionID, args.Verbose)
		if err != nil {
			return nil, HandoffRunResult{}, err
		}
		return nil, runResultFromCheck(args.SessionID, "completed", "", check, ""), nil
	}
}

func permissionsHandler(client *Client) mcp.ToolHandlerFor[requestListParams, RequestsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args requestListParams) (*mcp.CallToolResult, RequestsResult, error) {
		res, err := client.Permissions(ctx, args.location(), args.SessionID)
		if err != nil {
			return nil, RequestsResult{}, err
		}
		requests := summarizeRequests(res, "permission", args.SessionID)
		return nil, RequestsResult{Requests: requests, Count: len(requests)}, nil
	}
}

type permissionReplyParams struct {
	locationParams
	SessionID string `json:"session_id" jsonschema:"opencode session id."`
	RequestID string `json:"request_id" jsonschema:"permission request id."`
	Reply     string `json:"reply" jsonschema:"permission reply value, for example once, always, reject, or deny depending on opencode API."`
	Message   string `json:"message,omitempty" jsonschema:"Optional explanation."`
}

func permissionReplyHandler(client *Client) mcp.ToolHandlerFor[permissionReplyParams, PermissionReplyResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args permissionReplyParams) (*mcp.CallToolResult, PermissionReplyResult, error) {
		res, err := client.PermissionReply(ctx, args.location(), args.SessionID, args.RequestID, args.Reply, args.Message)
		if err != nil {
			return nil, PermissionReplyResult{}, err
		}
		return nil, PermissionReplyResult{OK: true, Data: res}, nil
	}
}

func questionsHandler(client *Client) mcp.ToolHandlerFor[requestListParams, RequestsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args requestListParams) (*mcp.CallToolResult, RequestsResult, error) {
		res, err := client.Questions(ctx, args.location(), args.SessionID)
		if err != nil {
			return nil, RequestsResult{}, err
		}
		requests := summarizeRequests(res, "question", args.SessionID)
		return nil, RequestsResult{Requests: requests, Count: len(requests)}, nil
	}
}

type questionReplyParams struct {
	locationParams
	SessionID string     `json:"session_id" jsonschema:"opencode session id."`
	RequestID string     `json:"request_id" jsonschema:"question request id."`
	Answers   [][]string `json:"answers,omitempty" jsonschema:"Answer selections: each inner array is selected labels for one question."`
	Reject    bool       `json:"reject,omitempty" jsonschema:"Reject the question instead of answering it."`
}

func questionReplyHandler(client *Client) mcp.ToolHandlerFor[questionReplyParams, QuestionReplyResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args questionReplyParams) (*mcp.CallToolResult, QuestionReplyResult, error) {
		res, err := client.QuestionReply(ctx, args.location(), args.SessionID, args.RequestID, args.Reject, args.Answers)
		if err != nil {
			return nil, QuestionReplyResult{}, err
		}
		return nil, QuestionReplyResult{OK: true, Data: res}, nil
	}
}

func promptRequest(args runParams) PromptRequest {
	req := PromptRequest{
		Prompt:   PromptPayload{Text: args.Prompt},
		Delivery: args.Delivery,
		Resume:   args.Resume,
		Agent:    args.Agent,
	}
	if args.ProviderID != "" || args.ModelID != "" || args.Variant != "" {
		req.Model = &ModelRef{ProviderID: args.ProviderID, ModelID: args.ModelID, Variant: args.Variant}
	}
	return req
}

func waitContext(ctx context.Context, timeoutSeconds int, defaultWait time.Duration) (context.Context, context.CancelFunc) {
	timeout := defaultWait
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func checkSession(ctx context.Context, client *Client, loc Location, sessionID string, verbose bool) (HandoffCheckResult, error) {
	if sessionID == "" {
		return HandoffCheckResult{}, fmt.Errorf("session_id is required")
	}
	res := HandoffCheckResult{SessionID: sessionID}
	msg, msgErr := client.Messages(ctx, loc, sessionID)
	if msgErr == nil {
		res.Messages = summarizeMessages(msg, 6)
		res.MessagesReturned = len(res.Messages)
		res.FinalText = firstText(msg)
		if verbose {
			res.RawMessages = msg
		}
	}
	ctxData, ctxErr := client.Context(ctx, loc, sessionID)
	if ctxErr == nil {
		if verbose {
			res.RawContext = ctxData
		}
		if res.FinalText == "" {
			res.FinalText = firstText(ctxData)
		}
	}
	perms, _ := client.Permissions(ctx, loc, sessionID)
	res.PendingPermissions = summarizeRequests(perms, "permission", sessionID)
	res.PendingPermissionCount = len(res.PendingPermissions)
	questions, _ := client.Questions(ctx, loc, sessionID)
	res.PendingQuestions = summarizeRequests(questions, "question", sessionID)
	res.PendingQuestionCount = len(res.PendingQuestions)
	if msgErr != nil && ctxErr != nil {
		return HandoffCheckResult{}, fmt.Errorf("read session %q messages: %w; context: %w", sessionID, msgErr, ctxErr)
	}
	res.Message = checkMessage(res)
	return res, nil
}

func runResultFromCheck(sessionID, status, promptMessageID string, check HandoffCheckResult, message string) HandoffRunResult {
	if message == "" {
		message = check.Message
	}
	return HandoffRunResult{
		SessionID:              sessionID,
		Status:                 status,
		FinalText:              check.FinalText,
		PromptMessageID:        promptMessageID,
		Messages:               check.Messages,
		PendingPermissions:     check.PendingPermissions,
		PendingQuestions:       check.PendingQuestions,
		PendingPermissionCount: check.PendingPermissionCount,
		PendingQuestionCount:   check.PendingQuestionCount,
		MessagesReturned:       check.MessagesReturned,
		RawMessages:            check.RawMessages,
		RawContext:             check.RawContext,
		Message:                message,
	}
}

func checkMessage(res HandoffCheckResult) string {
	switch {
	case res.PendingPermissionCount > 0:
		return "pending permission request; use handoff_permissions with session_id to review it"
	case res.PendingQuestionCount > 0:
		return "pending question request; use handoff_questions with session_id to review it"
	default:
		return "session state loaded"
	}
}
