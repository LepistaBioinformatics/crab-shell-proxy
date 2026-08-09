package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProbeURLRejectsWhatMustNotBeProbed(t *testing.T) {
	bad := map[string]string{
		"empty":       "",
		"plain http":  "http://api.example.com/v1",
		"no scheme":   "api.example.com/v1",
		"file":        "file:///etc/passwd",
		"no host":     "https:///v1",
		"with query":  "https://api.example.com/v1?key=leak",
		"with anchor": "https://api.example.com/v1#x",
	}
	for name, raw := range bad {
		if _, err := probeURL(raw); err == nil {
			t.Errorf("%s (%q): accepted, want rejected", name, raw)
		}
	}

	// The completions path is appended, and a trailing slash does not double it:
	// this URL has to be the one picoclaw builds, or a green probe means nothing.
	got, err := probeURL("https://api.openai.com/v1/")
	if err != nil {
		t.Fatalf("probeURL: %v", err)
	}
	if got != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("probeURL = %q, want picoclaw's endpoint", got)
	}
}

func TestBlockedAddrCoversTheDeploymentsOwnNetwork(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // the proxy itself
		"10.1.2.3", "172.17.0.2", "192.168.1.10", // docker + private networks
		"169.254.169.254", // cloud metadata
		"fd00::1",         // unique-local
		"fe80::1",         // link-local
		"0.0.0.0",         // unspecified
	}
	for _, raw := range blocked {
		if !blockedAddr(net.ParseIP(raw)) {
			t.Errorf("%s: allowed, want blocked", raw)
		}
	}
	// The forms that smuggle a v4 address inside a v6 one. net.IP's own
	// predicates do not unwrap these, so every one of them passed the guard until
	// CodeQL's SSRF alert sent me back to check.
	smuggled := []string{
		"2002:7f00:0001::1",   // 6to4 wrapping 127.0.0.1
		"2002:0a00:0001::1",   // 6to4 wrapping 10.0.0.1
		"64:ff9b::7f00:1",     // NAT64 wrapping 127.0.0.1
		"64:ff9b::a9fe:a9fe",  // NAT64 wrapping the metadata address
		"2001:0:4136:e378::1", // Teredo
	}
	for _, raw := range smuggled {
		if !blockedAddr(net.ParseIP(raw)) {
			t.Errorf("%s: allowed — a v4 address inside a v6 one is still that address", raw)
		}
	}
	// Carrier-grade NAT is the host's own network on some deployments, and
	// IsPrivate reports only RFC 1918.
	if !blockedAddr(net.ParseIP("100.64.0.1")) {
		t.Error("100.64.0.1: allowed, want blocked (CGNAT)")
	}

	// A real provider address must still be reachable, or the guard has simply
	// turned the feature off.
	for _, raw := range []string{"1.1.1.1", "104.18.6.192", "2606:4700::1111"} {
		if blockedAddr(net.ParseIP(raw)) {
			t.Errorf("%s: blocked, want allowed", raw)
		}
	}
	if !blockedAddr(nil) {
		t.Error("an unparseable address must be blocked, not allowed by default")
	}
}

// The guard runs at dial time on the address actually connected to. This proves
// it end to end rather than by inspection: a hostname that resolves to loopback
// is exactly what a rebinding attack produces.
func TestProbeRefusesToDialInward(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	outcome, err := runProbe(ctx, probeDraft{
		Provider: "openai",
		Model:    "gpt-5.4",
		// localhost resolves to 127.0.0.1 — inside the deployment.
		APIBase: "https://localhost/v1",
		APIKey:  "sk-test",
	})
	if err != nil {
		t.Fatalf("runProbe returned a hard error, want a failed OUTCOME: %v", err)
	}
	if outcome.OK {
		t.Fatal("probing localhost succeeded — the address guard is not engaged")
	}
	if outcome.Detail != "blocked_target" {
		t.Errorf("Detail = %q, want blocked_target", outcome.Detail)
	}
}

// Redirects are followed because picoclaw follows them. An endpoint that
// redirects to its real API host — a bare provider domain, the ordinary case —
// must not be reported as broken when the container would use it fine.
func TestProbeFollowsARedirectTheContainerWouldFollow(t *testing.T) {
	hop := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}

	if err := checkProbeRedirect(hop("https://integrate.api.nvidia.com/v1/chat/completions"),
		[]*http.Request{hop("https://nvidia.com/v1/chat/completions")}); err != nil {
		t.Errorf("an https hop must be followed: %v", err)
	}

	// A downgrade would put the member's key on the wire in plaintext.
	if err := checkProbeRedirect(hop("http://api.example.com/v1/chat/completions"),
		[]*http.Request{hop("https://example.com/v1/chat/completions")}); !errors.Is(err, errInsecureRedirect) {
		t.Errorf("http hop = %v, want it refused", err)
	}

	// An unbounded chain is still a chain to refuse.
	var chain []*http.Request
	for i := 0; i < maxProbeRedirects; i++ {
		chain = append(chain, hop("https://example.com/v1"))
	}
	if err := checkProbeRedirect(hop("https://example.com/v1"), chain); !errors.Is(err, errTooManyRedirects) {
		t.Errorf("long chain = %v, want it refused", err)
	}
}

// The model listing is a second outbound request built from the same member
// input, so it has to pass the same door. It did not: it was string
// concatenation, valid only because its one caller happened to have validated
// already.
func TestTheModelListingURLGoesThroughTheSameValidator(t *testing.T) {
	got, err := probeSiblingURL("https://api.openai.com/v1", "/models")
	if err != nil {
		t.Fatalf("probeSiblingURL: %v", err)
	}
	if got != "https://api.openai.com/v1/models" {
		t.Errorf("probeSiblingURL = %q", got)
	}
	for _, bad := range []string{"http://api.example.com/v1", "", "ftp://x/v1"} {
		if _, err := probeSiblingURL(bad, "/models"); err == nil {
			t.Errorf("%q: accepted for the listing but refused for the completion", bad)
		}
	}
}

func TestValidateProbeDraftGatesTheProviderFamily(t *testing.T) {
	base := probeDraft{Provider: "openai", Model: "gpt-5.4", APIBase: "https://api.openai.com/v1", APIKey: "sk"}
	if err := validateProbeDraft(base); err != nil {
		t.Fatalf("an OpenAI-compatible draft must pass: %v", err)
	}
	// These reach picoclaw through a DIFFERENT client, so this probe's request
	// shape says nothing about whether the container could use them.
	for _, p := range []string{"azure", "bedrock", "gemini", "anthropic-messages", "github-copilot", "ollama"} {
		d := base
		d.Provider = p
		if err := validateProbeDraft(d); err == nil {
			t.Errorf("provider %q: accepted, want refused", p)
		}
	}
	missing := base
	missing.APIKey = ""
	if err := validateProbeDraft(missing); err == nil {
		t.Error("a draft with no key must be refused before any request is made")
	}
}

func TestProbeStatusClassNamesTheFixNotTheCode(t *testing.T) {
	cases := map[int]string{
		401: "bad_key",
		403: "bad_key",
		404: "bad_endpoint",
		429: "rate_limited",
		503: "provider_error",
		418: "http_418",
	}
	for code, want := range cases {
		if got := probeStatusClass(code, false); got != want {
			t.Errorf("probeStatusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

// The 404 disambiguation, on the half that needs no network: an unreadable or
// empty list must claim NOTHING, leaving the original "check the address".
// Guessing "your model is wrong" from a gateway that simply would not answer is
// how the misleading message got there in the first place.
func TestAnUnreadableModelListClaimsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// localhost is refused by the dial guard, so the list cannot be read.
	if modelIsUnknown(ctx, probeDraft{
		Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://localhost/v1", APIKey: "sk-test",
	}) {
		t.Error("an unreachable list must not be reported as a wrong model")
	}
}

// The reported case: a member typed a provider's corporate domain, it redirected
// to the marketing site, and the 404 there was reported as a missing version
// path — sending them to inspect the one part of the URL that was correct.
func TestA404AfterLandingSomewhereElseSaysSo(t *testing.T) {
	if got := probeStatusClass(404, true); got != "redirected_elsewhere" {
		t.Errorf("404 after a cross-host redirect = %q, want redirected_elsewhere", got)
	}
	// Only the 404 changes meaning. A 401 from wherever the chain ended is still
	// a credential the endpoint refused.
	if got := probeStatusClass(401, true); got != "bad_key" {
		t.Errorf("401 after a redirect = %q, want bad_key", got)
	}
}

func TestLooksLikeCompletionRejectsSomethingElseAnswering(t *testing.T) {
	if !looksLikeCompletion([]byte(`{"choices":[{"message":{"content":"pong"}}]}`)) {
		t.Error("a real completion must be recognised")
	}
	// A captive portal, a proxy error page, or an endpoint that 200s an error
	// envelope. Calling any of those a success is how a member ends up with a
	// model that "tested fine" and cannot hold a conversation.
	for _, raw := range []string{`{"error":"nope"}`, `<html>hi</html>`, `{"choices":[]}`, ``} {
		if looksLikeCompletion([]byte(raw)) {
			t.Errorf("%q: accepted as a completion", raw)
		}
	}
}

func TestProbeLimiterIsPerAccount(t *testing.T) {
	now := time.Now()
	l := newProbeLimiter()
	l.now = func() time.Time { return now }

	if !l.allow("u1") {
		t.Fatal("first probe must be allowed")
	}
	if l.allow("u1") {
		t.Error("a second immediate probe must be refused")
	}
	// One member spending their allowance must not block another.
	if !l.allow("u2") {
		t.Error("a different account must not be throttled by u1")
	}
	now = now.Add(probeInterval)
	if !l.allow("u1") {
		t.Error("the floor must expire")
	}
}

func TestUserModelProviderOptionsAreStableAndSorted(t *testing.T) {
	a := UserModelProviderOptions()
	b := UserModelProviderOptions()
	if len(a) != len(b) {
		t.Fatal("two calls disagreed — a map iteration is leaking into the API")
	}
	for i := range a {
		if a[i].Provider != b[i].Provider {
			t.Fatalf("order differs at %d: %q vs %q", i, a[i].Provider, b[i].Provider)
		}
		if i > 0 && a[i].Provider < a[i-1].Provider {
			t.Fatalf("not sorted at %d: %q before %q", i, a[i-1].Provider, a[i].Provider)
		}
	}
}

// The endpoint a member would otherwise have to guess. Getting this wrong is not
// a cosmetic failure: an api_base missing its version path reaches a REAL host
// and 404s, which reads as "wrong provider" rather than "wrong path" — the exact
// report this came from.
func TestProviderOptionsCarryTheEndpointFromTheCatalog(t *testing.T) {
	byName := map[string]UserProviderOption{}
	for _, o := range UserModelProviderOptions() {
		byName[o.Provider] = o
	}

	nvidia, ok := byName["nvidia"]
	if !ok {
		t.Fatal("nvidia is registerable and must be offered")
	}
	if nvidia.APIBase != "https://integrate.api.nvidia.com/v1" {
		t.Errorf("nvidia api_base = %q, want the catalog's versioned endpoint", nvidia.APIBase)
	}
	if len(nvidia.Models) == 0 {
		t.Error("nvidia must suggest at least the catalog's model")
	}
	// A model id NVIDIA does not have answers 404, which reads as a wrong URL —
	// a suggestion is worse than none if following it produces that. Their ids
	// are namespaced; the catalog carried a bare one that no longer exists.
	for _, m := range nvidia.Models {
		if !strings.Contains(m, "/") {
			t.Errorf("nvidia suggests %q, but its model ids are namespaced (owner/model)", m)
		}
	}

	// A suggestion must be usable as typed: probeURL is what turns it into the
	// request, so a base the catalog carries has to survive that unchanged.
	for _, o := range byName {
		if o.APIBase == "" {
			continue
		}
		if _, err := probeURL(o.APIBase); err != nil {
			t.Errorf("%s suggests %q, which the probe rejects: %v", o.Provider, o.APIBase, err)
		}
	}
}
