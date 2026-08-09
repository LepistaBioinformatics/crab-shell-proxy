package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// The connectivity probe behind "test before you save".
//
// It sends ONE minimal completion to the endpoint the member typed:
//
//	POST {api_base}/chat/completions   {"model":…,"messages":[…],"max_tokens":16}
//
// which is byte-for-byte the request picoclaw's own client makes
// (pkg/providers/openai_compat/provider.go, v0.3.1). Matching it is the whole
// point: a probe that used a different shape — GET /models, say, which is what
// picoclaw's launcher UI does — would go green for an endpoint that then refuses
// every actual turn.
//
// It runs HERE and not in the browser for two reasons: the API key must not
// travel to a page that could log it, and no browser reaches an arbitrary
// provider through CORS anyway.

const (
	probeTimeout = 15 * time.Second
	// probeReadLimit bounds what is read back. looksLikeCompletion parses the
	// TRUNCATED body, so a provider that padded a 16-token reply past this would
	// be reported as "not a completion" — 8 KiB is far past any real answer to
	// "ping", and the cap matters more than that edge.
	probeReadLimit = 8 << 10
	// probeInterval throttles one account's probes. A probe is an outbound
	// request the instance pays for and cannot see the target of; without a floor,
	// the endpoint is a request amplifier.
	probeInterval = 2 * time.Second
)

// probeDraft is what a caller asks to be tested — a definition that need not be
// saved, which is the point: the member proves it works BEFORE it can reach
// their workspace.
type probeDraft struct {
	Provider  string
	Model     string
	APIBase   string
	APIKey    string
	ExtraBody json.RawMessage
}

// probeOutcome is what a caller learns. The provider's response body is
// deliberately absent: it can carry an org id, a quota, or the key itself echoed
// in an error, and none of that belongs in a browser tab. Detail is a CLASS.
type probeOutcome struct {
	OK         bool
	StatusCode int
	LatencyMS  int64
	Detail     string
}

func (o probeOutcome) result(now time.Time) registry.TestResult {
	return registry.TestResult{
		OK: o.OK, StatusCode: o.StatusCode, LatencyMS: o.LatencyMS,
		Detail: o.Detail, At: now,
	}
}

// userModelProviders is the set a member may register. It is picoclaw's
// OpenAI-compatible family, read off the protocol switch in
// pkg/providers/factory_provider.go (v0.3.1) — the providers whose turns go
// through openai_compat, and therefore the only ones this probe's request shape
// actually represents.
//
// azure, bedrock, gemini, anthropic-messages and every oauth/CLI provider are
// absent ON PURPOSE. picoclaw builds a different client for each, so a green
// probe would say nothing about whether the container can use them. They remain
// available in the admin inventory, where whoever defines them can read the
// container's logs.
var userModelProviders = map[string]bool{
	"openai": true, "anthropic": true, "openrouter": true, "groq": true,
	"deepseek": true, "zhipu": true, "zai": true, "nvidia": true, "venice": true,
	"nearai": true, "moonshot": true, "shengsuanyun": true, "siliconflow": true,
	"cerebras": true, "vivgrid": true, "volcengine": true, "mistral": true,
	"avian": true, "longcat": true, "modelscope": true, "novita": true,
	"minimax": true, "qwen": true, "litellm": true, "openai-compatible": true,
}

// UserProviderOption is one entry of the provider picker: the name, the endpoint
// that provider actually answers on, and the models the catalog knows about.
//
// The endpoint is carried because a member cannot be expected to know it. The
// catalog was introduced for the admin form precisely to stop free-text typos
// being the normal failure mode; leaving the member's form to free text
// reproduced that failure exactly — a base URL missing its `/v1` reaches a real
// host, 404s, and reads as "wrong provider".
type UserProviderOption struct {
	Provider string   `json:"provider"`
	APIBase  string   `json:"api_base,omitempty"`
	Models   []string `json:"models,omitempty"`
}

// UserModelProviderOptions is the registerable set, sorted, with whatever the
// embedded catalog knows about each. Providers the catalog does not mention are
// still offered — with no suggestion, which is honest — because the allow-list is
// what picoclaw can route, and the catalog is only a convenience over it.
//
// A catalog read failure degrades to bare names rather than failing the request:
// the member can still type an endpoint, which is exactly what they did before
// this existed.
func UserModelProviderOptions() []UserProviderOption {
	byProvider := map[string]*UserProviderOption{}
	names := make([]string, 0, len(userModelProviders))
	for p := range userModelProviders {
		names = append(names, p)
		byProvider[p] = &UserProviderOption{Provider: p}
	}
	sort.Strings(names)

	entries, err := docker.SuggestionCatalog()
	if err == nil {
		for _, e := range entries {
			opt, ok := byProvider[strings.ToLower(strings.TrimSpace(e.Provider))]
			if !ok {
				continue
			}
			// An oauth entry carries no usable endpoint for a member (they cannot
			// complete the flow), so it contributes nothing.
			if e.AuthMethod != "" {
				continue
			}
			if opt.APIBase == "" {
				opt.APIBase = e.APIBase
			}
			if e.Model != "" {
				opt.Models = append(opt.Models, e.Model)
			}
		}
	}

	out := make([]UserProviderOption, 0, len(names))
	for _, n := range names {
		out = append(out, *byProvider[n])
	}
	return out
}

// inputError is a rejection a MEMBER has to act on, so its message is a CODE the
// interface resolves into their language (lib/i18n/errors.ts) rather than
// English prose. The inventory's admin routes keep returning prose: their
// audience reads the proxy's logs, and a code there would be worse.
type inputError struct{ Code string }

func (e inputError) Error() string { return e.Code }

var (
	errProviderNotAllowed = inputError{"provider_not_allowed"}
	errModelRequired      = inputError{"model_required"}
	errAPIKeyRequired     = inputError{"api_key_required"}
)

func validateProbeDraft(d probeDraft) error {
	if !userModelProviders[strings.ToLower(strings.TrimSpace(d.Provider))] {
		return errProviderNotAllowed
	}
	if strings.TrimSpace(d.Model) == "" {
		return errModelRequired
	}
	if strings.TrimSpace(d.APIKey) == "" {
		return errAPIKeyRequired
	}
	if _, err := probeURL(d.APIBase); err != nil {
		return err
	}
	return nil
}

// probeURL validates the endpoint and returns the completions URL.
//
// https only. Not a purity rule: the key travels on this request, and the
// address guard below is worthless against an attacker who can read the
// plaintext anyway. It also rules out the localhost-style providers (ollama,
// lmstudio, vllm) whose endpoint would be inside the proxy's own network — which
// is the address guard's whole concern.
func probeURL(apiBase string) (string, error) {
	raw := strings.TrimSpace(apiBase)
	if raw == "" {
		return "", inputError{"api_base_required"}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", inputError{"api_base_invalid"}
	}
	if u.Scheme != "https" {
		return "", inputError{"api_base_not_https"}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", inputError{"api_base_has_query"}
	}
	return strings.TrimRight(u.String(), "/") + "/chat/completions", nil
}

// blockedAddr reports whether an address is one the proxy must never be talked
// into reaching on a member's behalf: its own loopback, the docker network every
// container and the gateway share, or the cloud metadata service.
func blockedAddr(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 169 && ip4[1] == 254 {
		// Link-local, which is also where 169.254.169.254 lives.
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
}

var errBlockedTarget = errors.New("api_base resolves to an address inside the deployment")

// maxProbeRedirects bounds the chain. Well under Go's default 10, because a real
// provider endpoint redirects once (a bare domain to its API host) or not at all.
const maxProbeRedirects = 5

var (
	errTooManyRedirects = errors.New("too many redirects")
	errInsecureRedirect = errors.New("redirect leaves https")
)

// checkProbeRedirect decides whether to follow one hop.
//
// Redirects ARE followed, because picoclaw follows them: its client sets no
// CheckRedirect at all (pkg/providers/common/common.go, v0.3.1), so Go's default
// applies. Refusing them here made the probe fail for endpoints the container
// handles perfectly — a bare provider domain that redirects to its API host is
// the ordinary case, not an attack — and a probe that disagrees with the real
// request is worse than no probe.
//
// Refusing redirects was never the SSRF boundary anyway. That boundary is the
// dial guard below, which checks the resolved address of EVERY connection this
// client makes, redirect hops included: a permitted host that redirects inward
// is refused when the hop is dialled, not before it.
//
// Two things are still refused: an unbounded chain, and any hop that leaves
// https — the key rides on this request, and a downgrade would put it on the
// wire in plaintext.
//
// The Authorization header is deliberately NOT re-attached across hosts. Go
// drops it on a cross-domain redirect, picoclaw inherits exactly that, and
// re-adding it here would both send the member's key somewhere picoclaw never
// sends it and make the probe pass where the real turn fails.
func checkProbeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxProbeRedirects {
		return errTooManyRedirects
	}
	if req.URL.Scheme != "https" {
		return errInsecureRedirect
	}
	return nil
}

// probeClient is built per probe rather than shared, because the dial guard is
// the security boundary and a shared client's connection pool could hand back a
// connection established for a different (already-validated) host.
//
// The guard runs at DIAL time, on the address actually connected to, rather than
// on a hostname resolved earlier. That ordering is what closes DNS rebinding: a
// name that answers publicly on the first lookup and privately on the second
// still has to pass this check on the connection that carries the request.
func probeClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout:       probeTimeout,
		CheckRedirect: checkProbeRedirect,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if blockedAddr(ip.IP) {
						return nil, errBlockedTarget
					}
				}
				// Dial the address that was CHECKED, not the name: re-resolving
				// here would reopen the window the check just closed.
				var lastErr error
				for _, ip := range ips {
					conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		},
	}
}

// runProbe performs the request and classifies the answer. A transport failure
// is not an error of this function: "the endpoint did not answer" is a RESULT the
// member needs to see, not a 500 for the operator to read in a log.
func runProbe(ctx context.Context, d probeDraft) (probeOutcome, error) {
	endpoint, err := probeURL(d.APIBase)
	if err != nil {
		return probeOutcome{}, err
	}
	body := map[string]any{
		"model":      strings.TrimSpace(d.Model),
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 16,
		"stream":     false,
	}
	// extra_body is merged the way picoclaw merges it — top level of the request
	// — so a provider that needs one to answer at all is testable.
	if len(d.ExtraBody) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(d.ExtraBody, &extra); err != nil {
			return probeOutcome{}, inputError{"extra_body_not_object"}
		}
		for k, v := range extra {
			body[k] = v
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return probeOutcome{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return probeOutcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(d.APIKey))

	start := time.Now()
	res, err := probeClient().Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return probeOutcome{OK: false, LatencyMS: latency, Detail: probeErrClass(err)}, nil
	}
	defer res.Body.Close()
	// Read and discard: the connection is closed either way, and the body is
	// never surfaced. What it is read FOR is the shape check below.
	raw, _ := io.ReadAll(io.LimitReader(res.Body, probeReadLimit))

	// Whether the answer came from somewhere other than the address the member
	// typed. res.Request is the LAST request in the chain, so this is the host
	// that actually replied.
	redirected := res.Request != nil && res.Request.URL != nil && res.Request.URL.Host != req.URL.Host

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		detail := probeStatusClass(res.StatusCode, redirected)
		// A 404 has two unrelated causes and one useless message. Ask the
		// endpoint which one it is instead of guessing — see modelIsUnknown.
		if detail == "bad_endpoint" && modelIsUnknown(ctx, d) {
			detail = "bad_model"
		}
		return probeOutcome{
			OK: false, StatusCode: res.StatusCode, LatencyMS: latency,
			Detail: detail,
		}, nil
	}
	// A 200 that is not a completion is a proxy or a captive portal answering for
	// the endpoint. Reporting it as success is how a member ends up with a model
	// that "tested fine" and cannot hold a conversation.
	if !looksLikeCompletion(raw) {
		return probeOutcome{
			OK: false, StatusCode: res.StatusCode, LatencyMS: latency,
			Detail: "not_a_completion",
		}, nil
	}
	return probeOutcome{OK: true, StatusCode: res.StatusCode, LatencyMS: latency}, nil
}

// looksLikeCompletion checks for the one field every OpenAI-compatible answer
// carries. It does not validate the whole schema: the question is "did a model
// answer", not "is this provider spec-perfect".
func looksLikeCompletion(raw []byte) bool {
	var parsed struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false
	}
	return len(parsed.Choices) > 0
}

// probeErrClass turns a transport failure into something a member can act on,
// without quoting a message that may embed the URL or the key.
func probeErrClass(err error) string {
	switch {
	case errors.Is(err, errBlockedTarget):
		return "blocked_target"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	if errors.Is(err, errTooManyRedirects) {
		return "redirect_loop"
	}
	if errors.Is(err, errInsecureRedirect) {
		return "redirect_insecure"
	}
	if strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "certificate") {
		return "tls"
	}
	return "unreachable"
}

// modelIsUnknown disambiguates a 404 by asking the endpoint for its model list.
//
// A 404 from a chat completion means either "no such route" or "no such model",
// and providers answer both the same way (NVIDIA does). Reporting the first for
// the second sends a member to inspect the URL — the one part that was right.
//
// The list is the OpenAI-compatible contract's own `GET {api_base}/models`, which
// is the same call picoclaw's `picoclaw model add` makes, so any provider this
// form accepts serves it. It runs ONLY on the 404 path, and only to choose
// between two messages: if the list cannot be read, nothing is claimed and the
// original "check the address" stands.
func modelIsUnknown(ctx context.Context, d probeDraft) bool {
	endpoint := strings.TrimRight(strings.TrimSpace(d.APIBase), "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(d.APIKey))
	res, err := probeClient().Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return false
	}
	// Both shapes picoclaw's own reader accepts: the {data:[…]} envelope and a
	// bare array.
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	ids := []string{}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Data != nil {
		for _, m := range envelope.Data {
			ids = append(ids, m.ID)
		}
	} else {
		var bare []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &bare); err != nil {
			return false
		}
		for _, m := range bare {
			ids = append(ids, m.ID)
		}
	}
	// An empty list says nothing: some gateways answer 200 with no models rather
	// than refusing, and "your model is wrong" would be a guess.
	if len(ids) == 0 {
		return false
	}
	want := strings.TrimSpace(d.Model)
	for _, id := range ids {
		if id == want {
			return false
		}
	}
	return true
}

// probeStatusClass names what to fix. `redirected` is load-bearing on a 404: an
// address that redirected to a DIFFERENT host and 404'd there is almost never a
// missing version path — it is a corporate website standing in for an API host
// (nvidia.com → www.nvidia.com is the reported case). Telling that member to
// check their `/v1` sends them to inspect the one part of the URL that was right.
func probeStatusClass(code int, redirected bool) string {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return "bad_key"
	case code == http.StatusNotFound && redirected:
		return "redirected_elsewhere"
	case code == http.StatusNotFound:
		return "bad_endpoint"
	case code == http.StatusTooManyRequests:
		return "rate_limited"
	case code >= 500:
		return "provider_error"
	}
	return fmt.Sprintf("http_%d", code)
}

// probeLimiter is the per-account floor between probes. Keyed by account id, so
// one member cannot spend the instance's outbound budget while another is
// blocked.
type probeLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func newProbeLimiter() *probeLimiter {
	return &probeLimiter{last: map[string]time.Time{}, now: time.Now}
}

// allow reports whether this account may probe now, and records the attempt when
// it may.
func (l *probeLimiter) allow(accID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if prev, ok := l.last[accID]; ok && now.Sub(prev) < probeInterval {
		return false
	}
	l.last[accID] = now
	return true
}
