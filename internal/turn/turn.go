// Package turn holds the harness-neutral request shape for running one
// conversational turn, shared by the concrete runner (pico) and its consumer
// (httpapi) without an import cycle.
package turn

// Request is everything a turn runner needs to reach a running per-user
// container and run one turn. Picoclaw, the only runner, reads
// Endpoint/AuthToken/SessionID/Content; SessionKey and Model are populated but
// unread, and are kept because they describe the turn rather than the runner.
type Request struct {
	// Endpoint is the harness-specific address of the running container, e.g.
	// ws://<name>:18790/pico/ws for picoclaw.
	Endpoint string
	// AuthToken authenticates to the harness: the pico channel token.
	AuthToken string
	// SessionID scopes the conversation transcript (picoclaw session_id).
	SessionID string
	// SessionKey is the stable per-(user, agent) long-term memory scope. Unused by
	// picoclaw.
	SessionKey string
	// Model is the model label for this turn. Unused by picoclaw, which is pinned
	// server-side.
	Model string
	// Content is the user message for this turn.
	Content string
}

// Progress is a non-content signal emitted while a turn is running: the agent
// narrating a tool call, an internal thought, or a bare typing indicator. It
// NEVER contributes to the assistant's answer -- it exists so the client can
// show what is happening during the long silence before the reply lands.
type Progress struct {
	// Kind is "thought", "tool", "placeholder" or "typing".
	Kind string
	// Text is human-readable narration, already in the user's own language when
	// the agent wrote it. Empty for "typing".
	Text string
	// Tool is the called function's name, for Kind == "tool".
	Tool string
	// State is "start" or "stop", only for Kind == "typing".
	State string
}

// Attachment is a file the harness produced and delivered out-of-band: the reply
// text says it was sent, and this is what says WHERE from.
//
// URL is absolute by the time it reaches the sink -- the runner resolves the
// harness-relative path it received, because only the runner knows its own
// endpoint format. AuthToken is the bearer the harness expects on that URL.
type Attachment struct {
	Type        string
	URL         string
	Filename    string
	ContentType string
	AuthToken   string
}

// Sink receives everything a running turn emits. Every field may be nil, so the
// zero value is a valid no-op sink.
//
// This replaced a bare `onDelta func(string)`: adding progress as a second
// field rather than an optional setter makes the change a COMPILE error in
// every implementation, which is the point -- a runner must not silently keep
// the old behaviour. Attachment was added the same way, for the same reason.
type Sink struct {
	Content    func(string)
	Progress   func(Progress)
	Attachment func(Attachment)
}

// EmitContent calls the content callback when one is set.
func (s Sink) EmitContent(delta string) {
	if s.Content != nil {
		s.Content(delta)
	}
}

// EmitProgress calls the progress callback when one is set.
func (s Sink) EmitProgress(p Progress) {
	if s.Progress != nil {
		s.Progress(p)
	}
}

// EmitAttachment calls the attachment callback when one is set.
func (s Sink) EmitAttachment(a Attachment) {
	if s.Attachment != nil {
		s.Attachment(a)
	}
}
