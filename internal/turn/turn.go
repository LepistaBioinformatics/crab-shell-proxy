// Package turn holds the harness-neutral request shape for running one
// conversational turn, shared by the concrete runners (pico, hermes) and their
// consumer (httpapi) without an import cycle.
package turn

// Request is everything a turn runner needs to reach a running per-user
// container and run one turn. Which fields matter depends on the harness:
// picoclaw uses Endpoint/AuthToken/SessionID/Content; hermes additionally uses
// SessionKey (long-term memory scope) and Model.
type Request struct {
	// Endpoint is the harness-specific address of the running container, e.g.
	// ws://<name>:18790/pico/ws (picoclaw) or http://<name>:8642 (hermes).
	Endpoint string
	// AuthToken authenticates to the harness: the pico channel token (picoclaw)
	// or the API server bearer key (hermes).
	AuthToken string
	// SessionID scopes the conversation transcript (picoclaw session_id;
	// hermes X-Hermes-Session-Id).
	SessionID string
	// SessionKey is the stable per-(user, agent) long-term memory scope
	// (hermes X-Hermes-Session-Key). Unused by picoclaw.
	SessionKey string
	// Model is the model label sent in the request body (hermes). Unused by
	// picoclaw, which is pinned server-side.
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

// Sink receives everything a running turn emits. Both fields may be nil, so the
// zero value is a valid no-op sink.
//
// This replaced a bare `onDelta func(string)`: adding progress as a second
// field rather than an optional setter makes the change a COMPILE error in
// every implementation, which is the point -- the hermes runner must not
// silently keep the old behaviour.
type Sink struct {
	Content  func(string)
	Progress func(Progress)
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
