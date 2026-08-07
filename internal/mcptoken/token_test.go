package mcptoken

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

const secret = "test-signing-secret"

func scope() memgraph.Scope {
	return memgraph.Scope{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	tok, err := Mint(secret, scope())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, ok := Verify(secret, tok)
	if !ok {
		t.Fatalf("Verify rejected a token we just minted")
	}
	if got != scope() {
		t.Errorf("Verify = %+v, want %+v", got, scope())
	}
}

// The token must be stable, because it is written into config.json on every
// ensure. A token that varied would rewrite the file every time and raise a
// restart notice forever (FR-4.3).
func TestMintIsDeterministic(t *testing.T) {
	t.Parallel()
	first, err := Mint(secret, scope())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Mint(secret, scope())
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if again != first {
			t.Fatalf("Mint is not deterministic: %q then %q", first, again)
		}
	}
}

func TestDistinctScopesGetDistinctTokens(t *testing.T) {
	t.Parallel()
	seen := map[string]memgraph.Scope{}
	for _, sc := range []memgraph.Scope{
		{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"},
		{TenantID: "t2", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"},
		{TenantID: "t1", SubsAccID: "s2", Role: "alpha", UserAccID: "u1"},
		{TenantID: "t1", SubsAccID: "s1", Role: "beta", UserAccID: "u1"},
		{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u2"},
	} {
		tok, err := Mint(secret, sc)
		if err != nil {
			t.Fatalf("Mint(%+v): %v", sc, err)
		}
		if prev, dup := seen[tok]; dup {
			t.Fatalf("scopes %+v and %+v mint the same token", sc, prev)
		}
		seen[tok] = sc
	}
}

// The collision this guards against is not a forgery: both tokens are legitimately
// minted and carry valid MACs. A "/"-joined payload is simply not injective, so
// without the refusal, member A's own token would authorise member B's scope.
func TestMintRefusesADelimiterSoScopesCannotCollide(t *testing.T) {
	t.Parallel()
	a := memgraph.Scope{TenantID: "t1", SubsAccID: "s1", Role: "a/b", UserAccID: "c"}
	b := memgraph.Scope{TenantID: "t1", SubsAccID: "s1", Role: "a", UserAccID: "b/c"}

	tokA, errA := Mint(secret, a)
	tokB, errB := Mint(secret, b)

	if errA == nil || errB == nil {
		t.Fatalf("Mint accepted a scope field containing %q: %v / %v — these two scopes would collide", delimiter, errA, errB)
	}
	if !errors.Is(errA, ErrInvalidScope) || !errors.Is(errB, ErrInvalidScope) {
		t.Errorf("errors = %v / %v, want ErrInvalidScope", errA, errB)
	}
	if tokA != "" || tokB != "" {
		t.Errorf("Mint returned a token alongside an error: %q / %q", tokA, tokB)
	}
}

func TestMintRefusesAnEmptyField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sc   memgraph.Scope
	}{
		{"empty tenant", memgraph.Scope{SubsAccID: "s", Role: "r", UserAccID: "u"}},
		{"empty subscription", memgraph.Scope{TenantID: "t", Role: "r", UserAccID: "u"}},
		{"empty role", memgraph.Scope{TenantID: "t", SubsAccID: "s", UserAccID: "u"}},
		{"empty user", memgraph.Scope{TenantID: "t", SubsAccID: "s", Role: "r"}},
		{"all empty", memgraph.Scope{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Mint(secret, c.sc); !errors.Is(err, ErrInvalidScope) {
				t.Errorf("Mint(%+v) err = %v, want ErrInvalidScope", c.sc, err)
			}
		})
	}
}

func TestMintRefusesAnEmptySecret(t *testing.T) {
	t.Parallel()
	tok, err := Mint("", scope())
	if !errors.Is(err, ErrNoSecret) {
		t.Errorf("Mint with no secret err = %v, want ErrNoSecret", err)
	}
	if tok != "" {
		t.Errorf("Mint returned %q with no secret", tok)
	}
}

func TestVerifyRejectsMalformedAndForgedTokens(t *testing.T) {
	t.Parallel()
	valid, err := Mint(secret, scope())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	payload := strings.SplitN(valid, ".", 2)[0]

	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"no separator", payload},
		{"two separators", valid + ".extra"},
		{"empty payload", "." + strings.SplitN(valid, ".", 2)[1]},
		{"empty signature", payload + "."},
		{"non-base64 payload", "!!!." + strings.SplitN(valid, ".", 2)[1]},
		{"non-base64 signature", payload + ".!!!"},
		{"signature truncated", valid[:len(valid)-4]},
		{"payload swapped for another scope", func() string {
			other, _ := Mint(secret, memgraph.Scope{TenantID: "t9", SubsAccID: "s9", Role: "r9", UserAccID: "u9"})
			return strings.SplitN(other, ".", 2)[0] + "." + strings.SplitN(valid, ".", 2)[1]
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Verify(secret, c.token)
			if ok {
				t.Errorf("Verify(%q) accepted it", c.token)
			}
			if got != (memgraph.Scope{}) {
				t.Errorf("Verify returned %+v alongside false; want the zero Scope", got)
			}
		})
	}
}

func TestVerifyRejectsAnotherSecretsToken(t *testing.T) {
	t.Parallel()
	tok, err := Mint("secret-one", scope())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, ok := Verify("secret-two", tok); ok {
		t.Error("a token minted under a different secret verified; rotation would not revoke anything")
	}
	// And an empty verifying secret never accepts anything, so an unconfigured
	// proxy cannot be talked into authorising a request.
	if _, ok := Verify("", tok); ok {
		t.Error("Verify with no secret accepted a token")
	}
}

// The MAC must be checked before the payload is interpreted at all. A payload with
// the wrong field count but a VALID MAC exercises the ordering from the other side:
// it proves the field-count check exists and is not the thing gating the MAC.
func TestVerifyChecksTheMACBeforeTrustingThePayloadShape(t *testing.T) {
	t.Parallel()
	// Hand-mint a token over a deliberately malformed payload, signed correctly.
	for _, payload := range []string{
		"only-one-field",
		"t1/s1/alpha",             // three
		"t1/s1/alpha/u1/p1/extra", // six — five is now the project shape
		"t1//alpha/u1",            // right count, empty field
		"t1/s1/alpha/u1/",         // project shape with an empty project
		strings.Repeat("a/", 100), // absurd
	} {
		tok := b64(payload) + "." + b64(mac(secret, payload))
		got, ok := Verify(secret, tok)
		if ok {
			t.Errorf("Verify accepted a correctly-signed but malformed payload %q as %+v", payload, got)
		}
		if got != (memgraph.Scope{}) {
			t.Errorf("Verify returned %+v for payload %q", got, payload)
		}
	}

	// Conversely, a well-formed payload with a bad MAC is refused, so neither check
	// is standing in for the other.
	good := "t1/s1/alpha/u1"
	if _, ok := Verify(secret, b64(good)+"."+b64(mac("wrong-secret", good))); ok {
		t.Error("Verify accepted a well-formed payload with a bad MAC")
	}
}

func TestTokenIsURLSafeAndCarriesNoPadding(t *testing.T) {
	t.Parallel()
	tok, err := Mint(secret, scope())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// The token goes into a JSON header value and an HTTP header; "=" padding and
	// "+"/"/" would survive but invite quoting bugs downstream.
	if strings.ContainsAny(tok, "=+/") {
		t.Errorf("token %q contains characters raw-URL base64 should have avoided", tok)
	}
	for _, part := range strings.Split(tok, ".") {
		if _, err := base64.RawURLEncoding.DecodeString(part); err != nil {
			t.Errorf("token part %q is not raw-URL base64: %v", part, err)
		}
	}
}

// The MAC comparison has to be constant-time. Enforced as a source gate because
// the failure is invisible to a behavioural test: == and hmac.Equal agree on every
// input and differ only in timing.
func TestPackageComparesTheMACInConstantTime(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join(".", "token.go"))
	if err != nil {
		t.Fatalf("read token.go: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "hmac.Equal(") {
		t.Error("token.go does not use hmac.Equal; the MAC comparison must be constant-time")
	}
	if strings.Contains(text, "bytes.Equal(") {
		t.Error("token.go uses bytes.Equal; use hmac.Equal for the MAC")
	}
}

// A five-field payload is the agent-projects shape: same four scope fields plus
// the project. It has to round-trip, and a project-less token has to stay
// BYTE-IDENTICAL to what was minted before the field existed — that is what
// keeps tokens already sitting in running containers verifying.
func TestMintCarriesTheProjectAndStaysCompatibleWithout(t *testing.T) {
	t.Parallel()
	base := memgraph.Scope{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}

	plain, err := Mint(secret, base)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if payload, _ := decodePayloadForTest(plain); payload != "t1/s1/alpha/u1" {
		t.Errorf("project-less payload = %q, want the pre-feature four-field form", payload)
	}
	if got, ok := Verify(secret, plain); !ok || got.Project != "" {
		t.Errorf("Verify(project-less) = %+v, ok=%v", got, ok)
	}

	scoped := base
	scoped.Project = "seedtrial"
	tok, err := Mint(secret, scoped)
	if err != nil {
		t.Fatalf("Mint(project): %v", err)
	}
	got, ok := Verify(secret, tok)
	if !ok {
		t.Fatal("Verify refused a project token")
	}
	if got != scoped {
		t.Errorf("round trip = %+v, want %+v", got, scoped)
	}

	// The two must not collide: a project token and a project-less one for the
	// same workspace address different graphs.
	if tok == plain {
		t.Error("project and project-less tokens are identical")
	}
}

// A project containing the delimiter is refused rather than silently splitting
// into extra fields — the same rule the other scope fields already carry.
func TestMintRejectsProjectContainingDelimiter(t *testing.T) {
	t.Parallel()
	sc := memgraph.Scope{
		TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1",
		Project: "seed/trial",
	}
	if _, err := Mint(secret, sc); !errors.Is(err, ErrInvalidScope) {
		t.Errorf("Mint error = %v, want ErrInvalidScope", err)
	}
}

func decodePayloadForTest(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	return string(raw), true
}
