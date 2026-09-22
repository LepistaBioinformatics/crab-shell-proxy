package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
)

// publishServer is memberMangroveServer with a subscription that has somebody
// in it to address.
func publishServer(t *testing.T, mangroveURL string) *Server {
	t.Helper()
	s := memberMangroveServer(mangroveURL)
	s.Mgr.(*fakeOrch).users = []docker.UserRef{
		{AccID: accAlice, Role: "alpha", Email: "alice@x.test"},
		{AccID: accBob, Role: "alpha", Email: "bob@x.test"},
	}
	return s
}

// plainMember holds only the agent role that authorizes the workspace: enough to
// use the mangrove, not enough to govern anything.
func plainMember() string {
	return `{"accId":"` + accAlice + `","owners":[{"email":"u@x","isPrincipal":true}],` +
		`"licensedResources":{"records":[` +
		`{"accId":"` + subsX + `","tenantId":"` + tenantT + `","role":"alpha","perm":"write","verified":true}` +
		`]}}`
}

func withRole(role string) string {
	return `{"accId":"` + accAlice + `","owners":[{"email":"u@x","isPrincipal":true}],` +
		`"licensedResources":{"records":[` +
		`{"accId":"` + subsX + `","tenantId":"` + tenantT + `","role":"alpha","perm":"write","verified":true},` +
		`{"accId":"` + subsX + `","tenantId":"` + tenantT + `","role":"` + role + `","perm":"write","verified":true}` +
		`]}}`
}

func publish(t *testing.T, s *Server, profile, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodPost, "/v1/mangrove/publish"+mangroveScope, profile, body))
	return w
}

func audienceOf(t *testing.T, stub *stubMangrove) []string {
	t.Helper()
	raw, _ := stub.last["to"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// THE LICENCES ARE THE POINT OF THIS ROUTE. They are what the agent path cannot
// set, and getting them from the wrong tier is a privilege escalation rather
// than a bug -- so the table walks every tier against what reaches the mangrove.
func TestPublishSendsTheLicencesItsTierEarns(t *testing.T) {
	for _, tc := range []struct {
		name       string
		profile    string
		wantGroups bool
		wantTenant bool
	}{
		{"a plain member holds neither", plainMember(), false, false},
		{"a subscriptions-manager governs its subscription", withRole("subscriptions-manager"), true, false},
		{"a tenant-manager holds both", withRole("tenant-manager"), true, true},
		{"a tenant-owner holds both", withRole("tenant-owner"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubMangrove(t)
			s := publishServer(t, stub.srv.URL)

			w := publish(t, s, tc.profile, `{"cell":"soil-ph","content":"6.4"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("publish: %d %s", w.Code, w.Body.String())
			}
			// Absent and false are the same refusal downstream, so read both as
			// "not licensed" rather than asserting the field is present.
			if got, _ := stub.last["groupsLicensed"].(bool); got != tc.wantGroups {
				t.Errorf("groupsLicensed = %v, want %v", got, tc.wantGroups)
			}
			if got, _ := stub.last["tenantLicensed"].(bool); got != tc.wantTenant {
				t.Errorf("tenantLicensed = %v, want %v", got, tc.wantTenant)
			}
		})
	}
}

// A human's composition is signed by their person actor, not by their bot. The
// recipient has to be able to tell which of the two wrote something.
func TestPublishActsAsThePerson(t *testing.T) {
	stub := newStubMangrove(t)
	s := publishServer(t, stub.srv.URL)

	if w := publish(t, s, plainMember(), `{"cell":"c","content":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	if stub.last["as"] != "person" {
		t.Errorf("as = %v, want person", stub.last["as"])
	}
	obj, _ := stub.last["object"].(map[string]any)
	if obj["type"] != "MemoryNote" {
		t.Errorf("type = %v, want MemoryNote", obj["type"])
	}
}

// The person/agent choice is why toEmails is structured rather than a new
// address prefix: in the directory's strict mode the webapp never sees an id.
func TestPublishResolvesEmailsToTheChosenActors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			"the agent alone",
			`{"cell":"c","content":"x","toEmails":[{"email":"bob@x.test","agent":true}]}`,
			[]string{"mangrove:actor:" + accBob + ":service"},
		},
		{
			"the person alone",
			`{"cell":"c","content":"x","toEmails":[{"email":"bob@x.test","person":true}]}`,
			[]string{"mangrove:actor:" + accBob + ":person"},
		},
		{
			"both, person first",
			`{"cell":"c","content":"x","toEmails":[{"email":"bob@x.test","person":true,"agent":true}]}`,
			[]string{"mangrove:actor:" + accBob + ":person", "mangrove:actor:" + accBob + ":service"},
		},
		{
			// Naming somebody and ticking neither box addressed them at nobody.
			// An unqualified email means the agent everywhere else in this stack.
			"neither box falls back to the agent",
			`{"cell":"c","content":"x","toEmails":[{"email":"bob@x.test"}]}`,
			[]string{"mangrove:actor:" + accBob + ":service"},
		},
		{
			// Kept, not dropped: the gate refuses it BY NAME, which is what
			// tells the author their colleague was not found. Dropping it would
			// deliver to fewer people than they asked for and say nothing.
			"an unknown address survives to be refused by name",
			`{"cell":"c","content":"x","toEmails":[{"email":"nobody@x.test","agent":true}]}`,
			[]string{"email:nobody@x.test"},
		},
		{
			"case and padding do not hide a member",
			`{"cell":"c","content":"x","toEmails":[{"email":"  BOB@X.test ","agent":true}]}`,
			[]string{"mangrove:actor:" + accBob + ":service"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubMangrove(t)
			s := publishServer(t, stub.srv.URL)

			if w := publish(t, s, plainMember(), tc.body); w.Code != http.StatusOK {
				t.Fatalf("publish: %d %s", w.Code, w.Body.String())
			}
			got := audienceOf(t, stub)
			if len(got) != len(tc.want) {
				t.Fatalf("audience = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("audience[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Explicit ids and emails address the same activity; they are two spellings, not
// two requests.
func TestPublishMergesExplicitIdsWithEmails(t *testing.T) {
	stub := newStubMangrove(t)
	s := publishServer(t, stub.srv.URL)

	body := `{"cell":"c","content":"x",` +
		`"to":["mangrove:group:subscription:` + subsX + `"],` +
		`"toEmails":[{"email":"bob@x.test","agent":true}]}`
	if w := publish(t, s, withRole("subscriptions-manager"), body); w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	got := audienceOf(t, stub)
	if len(got) != 2 || got[0] != "mangrove:group:subscription:"+subsX ||
		got[1] != "mangrove:actor:"+accBob+":service" {
		t.Errorf("audience = %v", got)
	}
}

// An empty audience is meaningful, not a mistake: it publishes privately to the
// author, which is what a bare publish already means on the agent path.
func TestPublishWithNoAudienceIsPrivate(t *testing.T) {
	stub := newStubMangrove(t)
	s := publishServer(t, stub.srv.URL)

	if w := publish(t, s, plainMember(), `{"cell":"c","content":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	if got := audienceOf(t, stub); len(got) != 0 {
		t.Errorf("audience = %v, want empty", got)
	}
}

func TestPublishValidatesItsBody(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no cell", `{"content":"x"}`},
		{"blank cell", `{"cell":"   ","content":"x"}`},
		{"no content", `{"cell":"c"}`},
		{"blank content", `{"cell":"c","content":"  "}`},
		{"a media type the reader cannot rely on", `{"cell":"c","content":"x","mediaType":"application/pdf"}`},
		{"malformed", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubMangrove(t)
			s := publishServer(t, stub.srv.URL)
			if w := publish(t, s, plainMember(), tc.body); w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if stub.last != nil {
				t.Errorf("a rejected body still reached the mangrove: %v", stub.last)
			}
		})
	}
}

// The declared format is the whole point of the field: the reader stops
// guessing. Absent, markdown is the assumption the preview already makes.
func TestPublishCarriesTheDeclaredMediaType(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"absent defaults to markdown", `{"cell":"c","content":"x"}`, "text/markdown"},
		{"plain text is kept", `{"cell":"c","content":"x","mediaType":"text/plain"}`, "text/plain"},
		{"markdown is kept", `{"cell":"c","content":"x","mediaType":"text/markdown"}`, "text/markdown"},
		{"case does not matter", `{"cell":"c","content":"x","mediaType":"TEXT/Plain"}`, "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubMangrove(t)
			s := publishServer(t, stub.srv.URL)
			if w := publish(t, s, plainMember(), tc.body); w.Code != http.StatusOK {
				t.Fatalf("publish: %d %s", w.Code, w.Body.String())
			}
			obj, _ := stub.last["object"].(map[string]any)
			if obj["mediaType"] != tc.want {
				t.Errorf("mediaType = %v, want %v", obj["mediaType"], tc.want)
			}
		})
	}
}

// Unconfigured, this route does not exist either -- absent, not refusing, like
// every other mangrove surface.
func TestPublishAbsentWhenMangroveUnconfigured(t *testing.T) {
	s := memberMangroveServer("")
	w := publish(t, s, plainMember(), `{"cell":"c","content":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 with no mangrove configured", w.Code)
	}
}
