// Package identity resolves the caller's account identity from the mycelium
// profile header.
//
// Identity is keyed on the account id (Profile.accId, a stable unique UUID),
// NOT the email — the email is mutable, while accId is the canonical account
// primary key mycelium propagates. The principal owner's email is still read,
// but only for human-facing traceability (written next to the user's config),
// never as the isolation key. Mirrors picoclaw-openai-proxy/server.js's decode
// (base64 -> zstd -> JSON), but on the profile's accId/owners fields.
package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/klauspost/compress/zstd"
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
}

// Resolver decodes the mycelium profile header into an Identity.
//
// It is an interface so the parallel Go mycelium SDK can be dropped in later
// without touching callers; FallbackResolver is the self-contained default so
// this feature is not blocked on the SDK.
type Resolver interface {
	// Resolve returns the caller identity and true, or a zero value and false
	// when the header is absent/undecodable or carries no accId (callers map
	// false to HTTP 401).
	Resolve(profileHeader string) (Identity, bool)
}

// owner mirrors the subset of a mycelium Profile owner we read (camelCase, per
// the Profile's `#[serde(rename_all = "camelCase")]`).
type owner struct {
	Email       string `json:"email"`
	IsPrincipal bool   `json:"isPrincipal"`
}

type profile struct {
	AccID  string  `json:"accId"`
	Owners []owner `json:"owners"`
}

// FallbackResolver decodes the profile with no external SDK dependency.
type FallbackResolver struct {
	dec *zstd.Decoder
}

// NewFallbackResolver builds a reusable resolver. The zstd decoder is safe for
// concurrent use across requests.
func NewFallbackResolver() (*FallbackResolver, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &FallbackResolver{dec: dec}, nil
}

// Resolve implements Resolver.
func (r *FallbackResolver) Resolve(header string) (Identity, bool) {
	if header == "" {
		return Identity{}, false
	}
	compressed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return Identity{}, false
	}
	jsonBytes, err := r.dec.DecodeAll(compressed, nil)
	if err != nil {
		return Identity{}, false
	}
	var p profile
	if err := json.Unmarshal(jsonBytes, &p); err != nil {
		return Identity{}, false
	}
	if p.AccID == "" {
		return Identity{}, false // no account id => cannot isolate
	}
	return Identity{AccID: p.AccID, Email: principalEmail(p.Owners)}, true
}

// principalEmail returns the principal owner's email (or the first owner's), or
// "" — parity with server.js's owners.find(isPrincipal) || owners[0].
func principalEmail(owners []owner) string {
	var chosen *owner
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
