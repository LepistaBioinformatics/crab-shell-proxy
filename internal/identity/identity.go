// Package identity resolves the caller's account identity from the mycelium
// profile header.
//
// Identity is keyed on the account id (Profile.accId, a stable unique UUID),
// NOT the email — the email is mutable, while accId is the canonical account
// primary key mycelium propagates. The principal owner's email is still read,
// but only for human-facing traceability (written next to the user's config),
// never as the isolation key. Decoding is delegated to the official mycelium Go
// SDK (github.com/LepistaBioinformatics/mycelium-sdk-go), which owns the wire
// contract (base64 -> zstd -> JSON, and the licensedResources records/urls
// enum); the resolved *mycelium.Profile is carried on Identity so handlers can
// apply the SDK's fluent authorization filters.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	mycelium "github.com/LepistaBioinformatics/mycelium-sdk-go"
	"github.com/google/uuid"
)

// ProfileHeader is the header mycelium injects with the compressed profile.
const ProfileHeader = "x-mycelium-profile"

// ServiceNameHeader is the header mycelium injects naming the matched service
// (e.g. "picoclaw-alpha"); the proxy resolves the agent from it.
const ServiceNameHeader = "x-mycelium-service-name"

// Identity is the resolved caller identity.
type Identity struct {
	// AccID is Profile.accId — the stable, unique account id (the isolation key).
	AccID string
	// Email is the principal owner's email — for traceability only, may be "".
	Email string
	// Profile is the full decoded mycelium profile, carried so handlers can run
	// the SDK's fluent authorization filters (WithReadAccess/OnAccount/…).
	Profile *mycelium.Profile
}

// Resolver decodes the mycelium profile header into an Identity.
//
// It is an interface so an alternative decoder (or a fake, in tests) can be
// substituted without touching callers; SDKResolver is the default,
// backed by the official mycelium Go SDK.
type Resolver interface {
	// Resolve returns the caller identity and true, or a zero value and false
	// when the header is absent/undecodable or carries no accId (callers map
	// false to HTTP 401).
	Resolve(profileHeader string) (Identity, bool)
}

// SDKResolver decodes the profile header via the official mycelium Go SDK.
type SDKResolver struct{}

// NewSDKResolver builds a resolver. The SDK's zstd decoder is a package-level
// singleton safe for concurrent use, so the resolver holds no state.
func NewSDKResolver() *SDKResolver { return &SDKResolver{} }

// Resolve implements Resolver. Decoding is strict (the gateway always
// compresses); a decode failure or an empty/nil account id yields false, which
// callers map to HTTP 401 — the fail-safe posture (a mis-decode never grants
// access).
func (r *SDKResolver) Resolve(header string) (Identity, bool) {
	p, err := mycelium.DecodeAndDecompressProfileFromBase64(header)
	if err != nil {
		return Identity{}, false
	}
	if p.AccID == uuid.Nil {
		return Identity{}, false // no account id => cannot isolate
	}
	return Identity{
		AccID:   p.AccID.String(),
		Email:   principalEmail(p.Owners),
		Profile: p,
	}, true
}

// principalEmail returns the principal owner's email (or the first owner's), or
// "" — parity with server.js's owners.find(isPrincipal) || owners[0].
func principalEmail(owners []mycelium.Owner) string {
	var chosen *mycelium.Owner
	for i := range owners {
		if owners[i].IsPrincipal {
			chosen = &owners[i]
			break
		}
	}
	if chosen == nil && len(owners) > 0 {
		chosen = &owners[0]
	}
	if chosen == nil {
		return ""
	}
	return chosen.Email
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// pathSegment is what SanitizeID's output must be: ONE path segment — no
// separator, not "." or "..", not starting with a character that would make it
// one. Anchored at both ends; a partial match would defeat the purpose.
//
// This is a GUARD, not a second rewrite. Every caller of SanitizeID uses the
// result as a directory or file name (workspaces, effective-secrets, restart
// markers, project workspaces), so "cannot contain a separator" is a property
// the filesystem layout depends on — and until now it held only as a side effect
// of the substitution above, which is documented as making ids *Docker-safe*.
// Loosening that regex to admit "/" for some future purpose would silently turn
// every one of those call sites into a path traversal. This makes that a test
// failure instead.
var pathSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// SanitizeID maps an account id to a Docker-name-safe token used in container
// names and per-user data dirs. accId is normally a UUID (already safe); this
// only guards against unexpected characters. Docker also requires the first
// character to be alphanumeric.
func SanitizeID(accID string) string {
	s := unsafeName.ReplaceAllString(strings.TrimSpace(accID), "-")
	s = strings.Trim(s, "-._")
	// The value leaves ONLY through the branch where a full match succeeded.
	//
	// Today this never fires: the substitution leaves nothing outside
	// [a-zA-Z0-9._-], and the trim guarantees the first character is alphanumeric,
	// so the match is already implied. That is exactly why it is safe to add to a
	// system whose directory names are on disk — the output cannot change, so no
	// existing workspace is orphaned. What it buys is the invariant becoming
	// enforced rather than emergent.
	if s == "" || !pathSegment.MatchString(s) {
		// Deterministic fallback for a pathological id.
		sum := sha256.Sum256([]byte(accID))
		return hex.EncodeToString(sum[:])[:16]
	}
	return s
}

// SessionKey derives the per-conversation key handed to picoclaw as its
// session_id, mirroring server.js's sessionIdFor = sha256(<id>::session_id),
// hex, first 32 chars — keyed on the accId so it is stable across email
// changes. Both the chat turn and /v1/sessions/history compute this identically
// so history lookups match live conversations. Returns "" if either input is
// empty (callers map that to HTTP 400).
func SessionKey(accID, sessionID string) string {
	if accID == "" || sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(accID + "::" + sessionID))
	return hex.EncodeToString(sum[:])[:32]
}

// The two functions below are the SAME FACT written twice — the project prefix
// the proxy stamps onto a session id, and the dispatch pattern picoclaw matches
// it against. They live side by side on purpose: if they ever disagree the rule
// simply never matches, and the symptom is not an error but a project whose
// chats are silently answered by the main agent, in the main workspace.
//
// The chain they have to agree on, verified against picoclaw v0.3.1:
//
//	proxy sends session_id ......... "p.<project>.<key>"
//	pico channel builds chat_id .... "pico:" + session_id      (pkg/channels/pico/pico.go)
//	router builds the match view ... "<chat_type>:" + chat_id, lowercased,
//	                                 chat_type defaulting to "direct"
//	                                 (pkg/routing/route.go, buildDispatchView)
//	rule matches when .............. when.chat equals that, with "*" as the only
//	                                 wildcard (our upstream patch)

// ProjectSeparator joins the project id into a session id.
//
// It is "." rather than "-" because picoclaw's agent-id alphabet is
// [a-z0-9_-], so a project id can never contain a dot — which makes the prefix
// unambiguous. With "-", the projects "my" and "my-proj" would both be matched
// by the pattern "p-my-*", quietly routing one user's conversation into the
// other project's agent and workspace.
const ProjectSeparator = "."

// ProjectSessionID stamps a project onto a conversation's session key. An empty
// projectID returns the key untouched, which is what keeps every non-project
// chat byte-identical to today and unable to match any project's pattern.
func ProjectSessionID(projectID, sessionKey string) string {
	if projectID == "" || sessionKey == "" {
		return sessionKey
	}
	return "p" + ProjectSeparator + projectID + ProjectSeparator + sessionKey
}

// ProjectChatPattern is the agents.dispatch `when.chat` value that matches every
// session ProjectSessionID produces for one project. Lowercase throughout,
// because the router lowercases both sides before comparing.
func ProjectChatPattern(projectID string) string {
	return "direct:pico:" + strings.ToLower(ProjectSessionID(projectID, "*"))
}
