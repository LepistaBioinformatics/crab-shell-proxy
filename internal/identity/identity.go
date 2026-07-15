// Package identity resolves the signed-in principal's email from the mycelium
// profile header, and derives a Docker-name-safe hash from it.
//
// This mirrors picoclaw-openai-proxy/server.js's emailFromRequest: the profile
// header is base64(zstd(json(Profile))); the principal owner's email is the
// "who" half of per-user isolation. A client cannot forge it — mycelium builds
// the header server-side from the verified token and fully replaces (not
// appends) it (fetch_and_inject_profile_from_token_to_forward.rs).
package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ProfileHeader is the header mycelium injects with the compressed profile.
const ProfileHeader = "x-mycelium-profile"

// ServiceNameHeader is the header mycelium injects naming the matched service
// (e.g. "picoclaw-alpha"); the proxy resolves the agent from it.
const ServiceNameHeader = "x-mycelium-service-name"

// Resolver decodes the mycelium profile header into the principal email.
//
// It is an interface so the parallel Go mycelium SDK can be dropped in later
// without touching callers; FallbackResolver is the self-contained default so
// this feature is not blocked on the SDK.
type Resolver interface {
	// PrincipalEmail returns the principal owner's email, or "" when the header
	// is absent or undecodable (never an error — callers map "" to HTTP 401).
	PrincipalEmail(profileHeader string) string
}

// owner mirrors the subset of mycelium's Profile.owners entries we read.
type owner struct {
	Email       string `json:"email"`
	IsPrincipal bool   `json:"isPrincipal"`
}

type profile struct {
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

// PrincipalEmail implements Resolver.
func (r *FallbackResolver) PrincipalEmail(header string) string {
	if header == "" {
		return ""
	}
	compressed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return ""
	}
	jsonBytes, err := r.dec.DecodeAll(compressed, nil)
	if err != nil {
		return ""
	}
	var p profile
	if err := json.Unmarshal(jsonBytes, &p); err != nil {
		return ""
	}
	// Prefer the principal owner; fall back to the first owner (parity with
	// server.js: owners.find(isPrincipal) || owners[0]).
	var chosen *owner
	for i := range p.Owners {
		if p.Owners[i].IsPrincipal {
			chosen = &p.Owners[i]
			break
		}
	}
	if chosen == nil && len(p.Owners) > 0 {
		chosen = &p.Owners[0]
	}
	if chosen == nil {
		return ""
	}
	return chosen.Email
}

// SessionKey derives the per-conversation key handed to picoclaw as its
// session_id, mirroring server.js's sessionIdFor = sha256(email::session_id),
// hex, first 32 chars. Both the chat turn and /v1/sessions/history compute this
// identically so history lookups match live conversations. Returns "" if either
// input is empty (callers map that to HTTP 400). Note: email is used as-is (not
// lowercased) for exact server.js parity; the (case-insensitive) container hash
// is UserHash, a separate concern.
func SessionKey(email, sessionID string) string {
	if email == "" || sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(email + "::" + sessionID))
	return hex.EncodeToString(sum[:])[:32]
}

// UserHash maps an email to a stable, Docker-name-safe 16-hex-char token used
// in container names and per-user data dirs. Lowercased first so casing
// variants of the same address collapse to one container.
func UserHash(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])[:16]
}
