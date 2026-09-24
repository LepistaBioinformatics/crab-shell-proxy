package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// The roster the directory answers from. Alice is the caller.
func rosterOrch() *fakeOrch {
	o := newFakeOrch()
	o.users = []docker.UserRef{
		{AccID: accAlice, Role: "alpha", Email: "alice@example.test"},
		{AccID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Role: "alpha", Email: "bob@example.test"},
		{AccID: "cccccccc-cccc-cccc-cccc-cccccccccccc", Role: "alpha", Email: "carol@other.test"},
	}
	return o
}

func directoryReq(t *testing.T, q string) *http.Request {
	t.Helper()
	return memberReq(t, http.MethodGet, "/v1/mangrove/directory"+mangroveScope+"&q="+q,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), "")
}

func decodeDirectory(t *testing.T, body []byte) directoryResponse {
	t.Helper()
	var got directoryResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v -- body %s", err, body)
	}
	return got
}

// STRICT IS THE DEFAULT. With no policy set anywhere, a member gets exact match
// and no actor id -- the friendlier mode is a decision an administrator makes,
// not one they inherit.
func TestDirectoryDefaultsToExactAndHidesTheActorID(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, directoryReq(t, "bob@example.test"))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", w.Code, w.Body.String())
	}
	got := decodeDirectory(t, w.Body.Bytes())
	if got.Mode != "exact" {
		t.Errorf("mode = %q, want exact", got.Mode)
	}
	if len(got.Results) != 1 || got.Results[0].Email != "bob@example.test" {
		t.Fatalf("results = %+v", got.Results)
	}
	if got.Results[0].ActorID != "" {
		t.Errorf("strict mode handed back an actor id: %q", got.Results[0].ActorID)
	}
}

// And a partial needle finds nothing in strict mode -- it is not a weaker
// search, it is a different question the mode does not answer.
func TestDirectoryStrictModeIgnoresAPartialNeedle(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, directoryReq(t, "bob"))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d", w.Code)
	}
	if got := decodeDirectory(t, w.Body.Bytes()); len(got.Results) != 0 {
		t.Errorf("a partial needle matched in strict mode: %+v", got.Results)
	}
}

// YOURSELF IS NOT A SEARCH RESULT. Your own ids come from /identity, which is
// never switched off -- so the directory does not need to carry you, and a
// roster that listed you would read as though you had to look yourself up.
func TestDirectoryNeverReturnsTheCaller(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, directoryReq(t, "alice@example.test"))
	if got := decodeDirectory(t, w.Body.Bytes()); len(got.Results) != 0 {
		t.Errorf("the caller found themselves: %+v", got.Results)
	}
}

func TestDirectoryNeedsANeedle(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet, "/v1/mangrove/directory"+mangroveScope,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), ""))
	if w.Code != http.StatusBadRequest {
		t.Errorf("answered %d with no q, want 400", w.Code)
	}
}

// Unconfigured, the directory does not exist either -- the same off switch as
// every other mangrove route.
func TestDirectoryAbsentWhenMangroveUnconfigured(t *testing.T) {
	s := memberMangroveServer("")
	s.Mgr = rosterOrch()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, directoryReq(t, "bob@example.test"))
	if w.Code != http.StatusNotFound {
		t.Errorf("answered %d, want 404", w.Code)
	}
}

// YOUR OWN IDS ARE ALWAYS AVAILABLE, in either mode. The whole point of having
// them is to hand them to somebody whose deployment cannot search for you, so
// the switch that hides other people's ids must never hide your own.
func TestIdentityAlwaysAnswersWithYourOwnIDs(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet, "/v1/mangrove/identity"+mangroveScope,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", w.Code, w.Body.String())
	}
	var got identityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.ServiceID, accAlice) || !strings.HasSuffix(got.ServiceID, ":service") {
		t.Errorf("serviceId = %q", got.ServiceID)
	}
	if !strings.HasSuffix(got.PersonID, ":person") {
		t.Errorf("personId = %q", got.PersonID)
	}
	if got.Email != "u@x" {
		t.Errorf("email = %q, want the profile's principal owner", got.Email)
	}
}

// `email:` resolves to an actor id within the caller's own subscription, which
// is what lets a member share with somebody whose id they were never shown.
func TestResolveAudienceTurnsAnEmailIntoAnActor(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()
	key := docker.WorkspaceKey{TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: accAlice}

	got, err := s.resolveAudience(key, []string{"email:bob@example.test", "mangrove:group:subscription:s1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if !strings.HasPrefix(got[0], "mangrove:actor:") || !strings.HasSuffix(got[0], ":service") {
		t.Errorf("email was not resolved: %q", got[0])
	}
	if got[1] != "mangrove:group:subscription:s1" {
		t.Errorf("a non-email entry was rewritten: %q", got[1])
	}
}

// AN UNRESOLVABLE ADDRESS IS KEPT, NOT DROPPED. Dropping it would deliver to
// fewer people than the member asked for and say nothing; keeping it lets the
// containment gate refuse it by name, which is the answer they can act on.
func TestResolveAudienceKeepsAnUnknownEmail(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()
	key := docker.WorkspaceKey{TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: accAlice}

	got, err := s.resolveAudience(key, []string{"email:nobody@example.test"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 || got[0] != "email:nobody@example.test" {
		t.Errorf("got %+v, want the entry kept verbatim", got)
	}
}

// withPrefixSearch returns a server whose policy allows prefix search for this
// subscription -- the administrator's decision, made explicitly.
func withPrefixSearch(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "model-registry.db"), nil)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	if err := reg.SetScopePolicy(
		registry.ScopeSel{Level: registry.LevelSubscription, TenantID: tenantT, SubsAccID: subsX},
		registry.AllowEmailPrefixSearchPolicy(true),
	); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()
	s.Reg = reg
	return s
}

// With the administrator's permission, a partial needle matches AND the actor id
// comes back -- the two halves of the friendlier mode travel together, because
// an id you cannot search for is no easier to use than one you cannot see.
func TestDirectoryPrefixModeMatchesAndReturnsTheActorID(t *testing.T) {
	s := withPrefixSearch(t)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, directoryReq(t, "bob"))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", w.Code, w.Body.String())
	}
	got := decodeDirectory(t, w.Body.Bytes())
	if got.Mode != "prefix" {
		t.Errorf("mode = %q, want prefix", got.Mode)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %+v", got.Results)
	}
	if !strings.HasPrefix(got.Results[0].ActorID, "mangrove:actor:") {
		t.Errorf("prefix mode withheld the actor id: %+v", got.Results[0])
	}
}

// A two-character needle is a sweep with extra steps: it would enumerate most of
// a subscription in a handful of tries. Refused even where prefix is allowed.
func TestDirectoryPrefixModeRefusesAShortNeedle(t *testing.T) {
	s := withPrefixSearch(t)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, directoryReq(t, "bo"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("answered %d for a two-character needle, want 400", w.Code)
	}
}

// RESOLVE NAMES WHAT THE MEMBER IS ALREADY LOOKING AT, and it does so in STRICT
// mode -- where a search would not have handed the id over. That is the whole
// distinction the route rests on: the id came from a card the mangrove already
// served this member, so naming it discloses nobody they were not already being
// shown.
func TestDirectoryResolvesIDsTheMemberAlreadyHolds(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()

	bob := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	ids := strings.Join([]string{
		mangroveServiceID(bob),
		mangrovePersonID(bob),
		// Somebody outside this subscription. Absent from the answer rather than
		// an error: a card may name an actor who has left, or one from another
		// deployment, and neither is a malfunction.
		"mangrove:actor:dddddddd-dddd-dddd-dddd-dddddddddddd:person",
	}, ",")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet,
		"/v1/mangrove/directory"+mangroveScope+"&ids="+ids,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", w.Code, w.Body.String())
	}

	var got resolveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v -- body %s", err, w.Body.String())
	}
	byID := map[string]string{}
	for _, e := range got.Resolved {
		byID[e.ID] = e.Email
	}
	// BOTH ACTORS NAME THE SAME PERSON, which is the answer a card needs when it
	// says an AGENT sent something: whose agent.
	for _, id := range []string{mangroveServiceID(bob), mangrovePersonID(bob)} {
		if byID[id] != "bob@example.test" {
			t.Errorf("%s resolved to %q", id, byID[id])
		}
	}
	if len(got.Resolved) != 2 {
		t.Errorf("resolved %d entries, want exactly the two that are members: %+v", len(got.Resolved), got.Resolved)
	}
}

// The subscription is the boundary, and it is the SAME boundary search has. An
// id from another tenant's account is not found here either.
func TestDirectoryResolveReachesNoFurtherThanTheSubscription(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	o := newFakeOrch()
	// Alice's subscription has only alice in it.
	o.users = []docker.UserRef{{AccID: accAlice, Role: "alpha", Email: "alice@example.test"}}
	s.Mgr = o

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet,
		"/v1/mangrove/directory"+mangroveScope+"&ids="+mangrovePersonID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", w.Code, w.Body.String())
	}
	var got resolveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Resolved) != 0 {
		t.Errorf("named somebody outside the caller's subscription: %+v", got.Resolved)
	}
}

// A call that named a thousand ids would be a sweep in one request. The
// subscription is what stops it mattering; this stops it being cheap.
func TestDirectoryResolveRefusesTooManyIDs(t *testing.T) {
	s := memberMangroveServer("http://mangrove:8090")
	s.Mgr = rosterOrch()

	// DISTINCT ids, because the limit counts the SET: a caller repeating one id a
	// thousand times is asking about one account, and refusing that would be the
	// test passing for the wrong reason.
	ids := make([]string, maxResolveIDs+1)
	for i := range ids {
		ids[i] = mangrovePersonID(fmt.Sprintf("acc-%04d", i))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet,
		"/v1/mangrove/directory"+mangroveScope+"&ids="+strings.Join(ids, ","),
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), ""))
	if w.Code != http.StatusBadRequest {
		t.Errorf("answered %d, want 400", w.Code)
	}
}
