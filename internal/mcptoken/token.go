// Package mcptoken mints and verifies the bearer token a picoclaw container uses
// to reach the proxy's native memory-graph MCP endpoint.
//
// The endpoint needs an authentication path of its own because a container has no
// x-mycelium-profile header: it is not a mycelium caller. And the token has to sit
// in plaintext in the workspace's config.json, because picoclaw offers no env
// indirection for tools.mcp.servers (context.md E-3) — env_file is stdio-only.
//
// So the token is made STATELESS and DETERMINISTIC: it carries its own scope and a
// MAC over it, meaning the proxy stores nothing, the same workspace always yields
// the same token (so config.json does not churn and no spurious restart is
// triggered), and rotation is rotating the secret.
//
// The package is separate from memgraph because this is authentication, and
// separate from mcpserver because internal/docker needs Mint and must not import
// an HTTP server.
package mcptoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// delimiter separates the four scope fields inside the signed payload.
const delimiter = "/"

// scopeFields is how many fields a payload must have. Verify requires exactly
// this, so a payload with a valid MAC but the wrong shape is still refused.
const scopeFields = 4

var (
	// ErrNoSecret means the proxy has no CRAB_MCP_TOKEN_SECRET. Minting without one
	// would produce a token anybody could compute, so it is refused outright rather
	// than defaulted.
	ErrNoSecret = errors.New("mcptoken: no signing secret configured")

	// ErrInvalidScope means a scope field is empty or contains the delimiter.
	ErrInvalidScope = errors.New("mcptoken: invalid scope field")
)

// Mint returns the token for one workspace.
//
// A scope field containing the delimiter is REFUSED, and this is the reason the
// function can return an error at all. Joining four fields with "/" is not an
// injective encoding on its own: role="a/b", userAccID="c" and role="a",
// userAccID="b/c" produce byte-identical payloads, so one member's perfectly
// legitimate token would verify as another member's scope. That is a
// canonicalisation collision, not a forgery, so the MAC cannot catch it — it is
// computed over the already-ambiguous bytes.
//
// identity.SanitizeID maps "/" to "-" and very likely makes this unreachable in
// practice, but that is a guarantee owned by a different package. Refusing here is
// what makes "no caller can address another member's graph" a property of this
// encoding rather than a property of whoever happened to call it.
func Mint(secret string, sc memgraph.Scope) (string, error) {
	if secret == "" {
		return "", ErrNoSecret
	}
	payload, err := encodeScope(sc)
	if err != nil {
		return "", err
	}
	return b64(payload) + "." + b64(mac(secret, payload)), nil
}

// Verify checks the MAC and returns the scope it authorises.
//
// The MAC is checked BEFORE the payload is interpreted for anything, including its
// own field count: an attacker-supplied payload is untrusted bytes until the MAC
// says otherwise. A failure returns the zero Scope, never a partially-populated
// one — a caller that ignored the bool would otherwise get a usable-looking scope.
func Verify(secret, token string) (memgraph.Scope, bool) {
	if secret == "" {
		return memgraph.Scope{}, false
	}
	// SplitN with 2, then reject a remainder containing another separator: a token
	// with two dots is malformed, not "the first two segments of a longer token".
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return memgraph.Scope{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return memgraph.Scope{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return memgraph.Scope{}, false
	}
	if !hmac.Equal(sig, mac(secret, string(payload))) {
		return memgraph.Scope{}, false
	}
	sc, err := decodeScope(string(payload))
	if err != nil {
		return memgraph.Scope{}, false
	}
	return sc, true
}

func encodeScope(sc memgraph.Scope) (string, error) {
	fields := []struct {
		name  string
		value string
	}{
		{"tenantID", sc.TenantID},
		{"subsAccID", sc.SubsAccID},
		{"role", sc.Role},
		{"userAccID", sc.UserAccID},
	}
	parts := make([]string, 0, scopeFields)
	for _, f := range fields {
		if f.value == "" {
			return "", fmt.Errorf("%w: %s is empty", ErrInvalidScope, f.name)
		}
		if strings.Contains(f.value, delimiter) {
			return "", fmt.Errorf("%w: %s contains %q", ErrInvalidScope, f.name, delimiter)
		}
		parts = append(parts, f.value)
	}
	return strings.Join(parts, delimiter), nil
}

func decodeScope(payload string) (memgraph.Scope, error) {
	parts := strings.Split(payload, delimiter)
	if len(parts) != scopeFields {
		return memgraph.Scope{}, fmt.Errorf("%w: got %d fields, want %d",
			ErrInvalidScope, len(parts), scopeFields)
	}
	for i, p := range parts {
		if p == "" {
			return memgraph.Scope{}, fmt.Errorf("%w: field %d is empty", ErrInvalidScope, i)
		}
	}
	return memgraph.Scope{
		TenantID:  parts[0],
		SubsAccID: parts[1],
		Role:      parts[2],
		UserAccID: parts[3],
	}, nil
}

func mac(secret, payload string) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	return h.Sum(nil)
}

func b64[T string | []byte](v T) string {
	return base64.RawURLEncoding.EncodeToString([]byte(v))
}
