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
