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

// SanitizeID maps an account id to a Docker-name-safe token used in container
// names and per-user data dirs. accId is normally a UUID (already safe); this
// only guards against unexpected characters. Docker also requires the first
// character to be alphanumeric.
func SanitizeID(accID string) string {
	s := unsafeName.ReplaceAllString(strings.TrimSpace(accID), "-")
	s = strings.Trim(s, "-._")
	if s == "" {
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
