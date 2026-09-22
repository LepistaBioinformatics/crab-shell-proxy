package mangrove

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func tuple() Tuple {
	return Tuple{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "alice"}
}

// Both halves are required. Either one alone is a misconfiguration, and
// treating it as "on" would register tools that can only fail.
func TestEnabledNeedsBothHalves(t *testing.T) {
	for _, tc := range []struct {
		name, base, token string
		want              bool
	}{
		{"both set", "http://mangrove:8090", "secret", true},
		{"no token", "http://mangrove:8090", "", false},
		{"no base url", "", "secret", false},
		{"neither", "", "", false},
	} {
		if got := New(tc.base, tc.token).Enabled(); got != tc.want {
			t.Errorf("%s: Enabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
	var nilClient *Client
	if nilClient.Enabled() {
		t.Error("a nil client reported itself enabled")
	}
}

func TestUnconfiguredClientRefusesRatherThanDialling(t *testing.T) {
	_, err := New("", "").Publish(context.Background(), tuple(), AsService, false, Object{}, nil)
	if err == nil {
		t.Fatal("an unconfigured client attempted a call")
	}
}

// A REFUSAL MUST KEEP THE ADDRESSEE IT NAMED. The mangrove answers 403 with a body
// saying which entry was out of reach; swallowing that body would leave the
// agent -- and the member reading its answer -- with no way to know what to fix.
func TestRefusalBodySurvives(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"out of reach: mangrove:actor:mallory:service (no workspace under a subscription this caller shares)","addressee":"mangrove:actor:mallory:service","delivered":false}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "secret").Publish(context.Background(), tuple(), AsService, false,
		Object{Type: "MemoryNote", Cell: "c"}, []string{"mangrove:actor:mallory:service"})
	if err == nil {
		t.Fatal("a 403 was not reported as an error")
	}
	var mangroveErr *Error
	if !errors.As(err, &mangroveErr) {
		t.Fatalf("want a *mangrove.Error, got %T", err)
	}
	if mangroveErr.Status != http.StatusForbidden {
		t.Errorf("status = %d", mangroveErr.Status)
	}
	if !strings.Contains(err.Error(), "mallory") {
		t.Errorf("the offending addressee was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "delivered") {
		t.Errorf("the delivered=false signal was lost: %v", err)
	}
}

// An unreachable mangrove is an error the caller can render, never a panic and
// never a hang beyond the timeout.
func TestUnreachableMangroveIsAnOrdinaryError(t *testing.T) {
	// A port nothing listens on.
	_, err := New("http://127.0.0.1:1", "secret").Timeline(context.Background(), tuple(), AsService, "received")
	if err == nil {
		t.Fatal("reaching a dead address succeeded")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error does not say it could not reach the mangrove: %v", err)
	}
}

// The bearer token authenticates the proxy as the mangrove's one caller.
func TestTokenIsSent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "s3cret").Timeline(context.Background(), tuple(), AsService, "received"); err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if got != "Bearer s3cret" {
		t.Errorf("Authorization = %q", got)
	}
}

// THE TWO FIELDS THIS PROXY ALONE MAY SET.
//
// `as` decides which actor signs, and `tenantLicensed` is what the mangrove takes
// as proof that a human's mycelium profile licenses the tenant. An agent's MCP
// token cannot prove the latter, so the MCP path must never set it -- this test
// pins the wire shape that makes the distinction expressible at all.
func TestCallerFieldsAreOnTheWire(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")

	if _, err := c.Publish(context.Background(), tuple(), AsService, false, Object{Type: "MemoryNote", Cell: "c"}, nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if body["as"] != "service" {
		t.Errorf("as = %v, want service", body["as"])
	}
	if _, present := body["tenantLicensed"]; present {
		t.Errorf("tenantLicensed was sent for an agent call: %v", body["tenantLicensed"])
	}
	tup, _ := body["tuple"].(map[string]any)
	if tup["userAccId"] != "alice" || tup["subsAccId"] != "s1" {
		t.Errorf("tuple did not travel intact: %v", tup)
	}

	body = nil
	if _, err := c.Publish(context.Background(), tuple(), AsPerson, true, Object{Type: "MemoryNote", Cell: "c"}, nil); err != nil {
		t.Fatalf("publish as person: %v", err)
	}
	if body["as"] != "person" {
		t.Errorf("as = %v, want person", body["as"])
	}
	if body["tenantLicensed"] != true {
		t.Errorf("tenantLicensed = %v, want true for a licensed human", body["tenantLicensed"])
	}
}

// Decide and Revoke are human-only and have no MCP tool. They hard-code the
// Person actor rather than taking it from a caller, so there is no argument an
// agent path could pass to act as the human.
func TestHumanOnlyCallsAlwaysActAsThePerson(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")

	if _, err := c.Revoke(context.Background(), tuple(), "mangrove:obj:1", "cell"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if body["as"] != "person" {
		t.Errorf("revoke acted as %v", body["as"])
	}

	body = nil
	if _, err := c.Decide(context.Background(), tuple(), "mangrove:act:1", true, true); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if body["as"] != "person" {
		t.Errorf("decide acted as %v", body["as"])
	}
	if body["governs"] != true {
		t.Errorf("governs did not travel: %v", body["governs"])
	}
}

func TestBaseURLTrailingSlashIsNormalised(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL+"/", "secret").Admit(context.Background(), tuple(), AsPerson, "mangrove:act:1"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if path != "/internal/v1/admit" {
		t.Errorf("path = %q; a trailing slash on the base URL produced a doubled separator", path)
	}
}
