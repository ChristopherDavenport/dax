package acp

import "encoding/json"

// The methods dax answers and sends (ACP v1).
const (
	MethodInitialize        = "initialize"
	MethodAuthenticate      = "authenticate"
	MethodSessionNew        = "session/new"
	MethodSessionLoad       = "session/load"
	MethodSessionPrompt     = "session/prompt"
	MethodSessionCancel     = "session/cancel"
	MethodSessionSetMode    = "session/set_mode"
	MethodSessionUpdate     = "session/update"
	MethodRequestPermission = "session/request_permission"
)

// InitializeRequest is the editor's initialize.
type InitializeRequest struct {
	ProtocolVersion int `json:"protocolVersion"`
}

// InitializeResponse is the agent's answer: what it can do.
type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

// AgentCapabilities are what dax offers.
type AgentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities PromptCapabilities `json:"promptCapabilities"`
	MCPCapabilities    MCPCapabilities    `json:"mcpCapabilities"`
}

// PromptCapabilities are the content a prompt may carry beside text.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// MCPCapabilities are the MCP transports beside stdio dax takes.
type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// AuthMethod is a way to authenticate; dax offers none.
type AuthMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EnvVariable is one variable of an MCP server's environment, or one
// header.
type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServer is one of the editor's MCP servers: a stdio one (no Type),
// or an http or sse one.
type MCPServer struct {
	Type    string        `json:"type,omitempty"`
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []EnvVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []EnvVariable `json:"headers,omitempty"`
}

// NewSessionRequest is session/new.
type NewSessionRequest struct {
	CWD        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// NewSessionResponse is session/new's answer.
type NewSessionResponse struct {
	SessionID string        `json:"sessionId"`
	Modes     *SessionModes `json:"modes,omitempty"`
}

// LoadSessionRequest is session/load.
type LoadSessionRequest struct {
	SessionID  string      `json:"sessionId"`
	CWD        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// LoadSessionResponse is session/load's answer, once the conversation
// was replayed as updates.
type LoadSessionResponse struct {
	Modes *SessionModes `json:"modes,omitempty"`
}

// SessionModes are the modes a session offers and the one in force.
type SessionModes struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// SessionMode is one mode.
type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SetModeRequest is session/set_mode.
type SetModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// PromptRequest is session/prompt.
type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// Stop reasons of a prompt's turn.
const (
	StopEndTurn         = "end_turn"
	StopMaxTokens       = "max_tokens"
	StopMaxTurnRequests = "max_turn_requests"
	StopRefusal         = "refusal"
	StopCancelled       = "cancelled"
)

// PromptResponse is session/prompt's answer.
type PromptResponse struct {
	StopReason string `json:"stopReason"`
}

// CancelNotification is session/cancel.
type CancelNotification struct {
	SessionID string `json:"sessionId"`
}

// ContentBlock is a piece of a prompt or a message: text, an image, a
// link to a resource or one embedded.
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	URI      string            `json:"uri,omitempty"`
	Name     string            `json:"name,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource is a resource's contents a prompt carries.
type EmbeddedResource struct {
	URI      string `json:"uri"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Text is a text content block.
func Text(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

// SessionNotification is session/update.
type SessionNotification struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// The kinds of session update dax sends.
const (
	UpdateUserMessage       = "user_message_chunk"
	UpdateAgentMessage      = "agent_message_chunk"
	UpdateAgentThought      = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentMode       = "current_mode_update"
)

// Tool call statuses.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// SessionUpdate is one update; SessionUpdate says which, and which of
// the other fields it carries.
type SessionUpdate struct {
	SessionUpdate string `json:"sessionUpdate,omitempty"`
	// A message chunk's.
	Content *ContentBlock `json:"content,omitempty"`
	// A tool call's, or its update's.
	ToolCallID string            `json:"toolCallId,omitempty"`
	Title      string            `json:"title,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Status     string            `json:"status,omitempty"`
	RawInput   json.RawMessage   `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage   `json:"rawOutput,omitempty"`
	ToolOutput []ToolCallContent `json:"-"`
	// available_commands_update's.
	AvailableCommands []AvailableCommand `json:"availableCommands,omitempty"`
	// current_mode_update's.
	CurrentModeID string `json:"currentModeId,omitempty"`
}

// MarshalJSON writes a tool call's content as "content", which a
// message chunk's single block also is.
func (u SessionUpdate) MarshalJSON() ([]byte, error) {
	type plain SessionUpdate
	if u.SessionUpdate == UpdateAvailableCommands {
		// The list is the update: an empty one is sent as one.
		cmds := u.AvailableCommands
		if cmds == nil {
			cmds = []AvailableCommand{}
		}
		return json.Marshal(struct {
			plain
			AvailableCommands []AvailableCommand `json:"availableCommands"`
		}{plain: plain(u), AvailableCommands: cmds})
	}
	if u.ToolOutput == nil {
		return json.Marshal(plain(u))
	}
	return json.Marshal(struct {
		plain
		Content []ToolCallContent `json:"content"`
	}{plain: plain(u), Content: u.ToolOutput})
}

// ToolCallContent is what a tool call produced: content.
type ToolCallContent struct {
	Type    string       `json:"type"`
	Content ContentBlock `json:"content"`
}

// AvailableCommand is a slash command the editor offers.
type AvailableCommand struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Input       *CommandInput `json:"input,omitempty"`
}

// CommandInput says a command takes text after it.
type CommandInput struct {
	Hint string `json:"hint"`
}

// RequestPermissionRequest asks the editor about a call.
type RequestPermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  SessionUpdate      `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// Permission option kinds.
const (
	AllowOnce    = "allow_once"
	AllowAlways  = "allow_always"
	RejectOnce   = "reject_once"
	RejectAlways = "reject_always"
)

// PermissionOption is one answer the editor offers.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// RequestPermissionResponse is the editor's answer.
type RequestPermissionResponse struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// PermissionOutcome is selected, with the option, or cancelled.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}
