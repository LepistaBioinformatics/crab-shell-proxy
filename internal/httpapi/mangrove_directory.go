package httpapi

import (
	"net/http"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// Finding somebody to share with, and finding yourself.
//
// THE DIRECTORY NEVER REACHES PAST THE CALLER'S OWN SUBSCRIPTION. It is answered
// from ListSubscriptionUsers over the (tenant, subscription) the caller is
// already authorized on, which is the same set the containment gate would let
// them address anyway. So the answer discloses nothing they could not already
// act on -- it saves them from having to be told an id out of band, it does not
// widen their reach by one account.
//
// TWO MODES, AND THE ADMIN PICKS. Exact match answers a question the member
// already had ("what is alice's id?"). Prefix search answers one they did not
// ("who is there?"), and turns the directory into something that can be swept by
// trying letters. So prefix is off unless an administrator turns it on for the
// scope, through the same ScopePolicy cascade that already governs personal
// models -- a third field on a mechanism that exists, rather than a second
// mechanism.
//
// IN STRICT MODE THE ACTOR ID IS NOT RETURNED. The member confirms the person is
// reachable and shares by EMAIL; the id stays an internal handle. That is why
// addressing accepts an `email:` form (see resolveAudience) -- without it,
// hiding the id would leave the member unable to act on what they just found.

type directoryEntry struct {
	Email string `json:"email"`
	// ActorID is omitted in strict mode. A member who cannot see it addresses by
	// email instead, which the publish path resolves.
	ActorID string `json:"actorId,omitempty"`
}

// resolvedEntry names an id the caller ALREADY HAS. See handleMangroveDirectory.
type resolvedEntry struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type resolveResponse struct {
	Resolved []resolvedEntry `json:"resolved"`
}

// How many ids one call will name. Not a security boundary -- the boundary is the
// subscription, below -- but a call that named a thousand would be a sweep with
// one request instead of many, and nothing legitimate asks for more than a
// screenful of cards' worth.
const maxResolveIDs = 60

type directoryResponse struct {
	// Mode is echoed so the UI can say which question it is able to answer,
	// rather than leaving the member to infer it from the shape of the results.
	Mode    string           `json:"mode"` // "exact" | "prefix"
	Results []directoryEntry `json:"results"`
}

// prefixSearchAllowed resolves the policy for this workspace. A registry that is
// not configured answers "no", which is the same end of the choice the policy
// itself defaults to.
func (s *Server) prefixSearchAllowed(key docker.WorkspaceKey) bool {
	if s.Reg == nil {
		return false
	}
	allowed, _, err := s.Reg.EmailPrefixSearchAllowed(registry.WorkspaceRef{
		TenantID:  key.TenantID,
		SubsAccID: key.SubsAccID,
		Agent:     key.Role,
		UserAccID: key.UserAccID,
	})
	if err != nil {
		s.logf("mangrove: directory policy: %v", err)
		return false
	}
	return allowed
}

// handleMangroveDirectory serves GET /v1/mangrove/directory, two ways:
//
//	?q=<email or part>  -- SEARCH: who is there, subject to the mode above.
//	?ids=<id>,<id>,...  -- RESOLVE: name ids the caller is already holding.
//
// RESOLVE IS NOT SEARCH TURNED AROUND, and the difference is what makes it safe
// to answer in strict mode where search will not hand an id out. Search starts
// from a needle the member typed and ends at somebody they had not named; resolve
// starts at an id the mangrove ALREADY PUT ON THEIR SCREEN -- the author of a
// post they can read, or a recipient of one -- and only says who it is. The
// member gains a name for a row they are already looking at.
//
// It reaches no further than search does: both are answered from
// ListSubscriptionUsers over the caller's own (tenant, subscription), so an id
// belonging to anybody else simply is not found. There is no sweep in it either,
// because the input is an account uuid: you cannot guess your way to one, and the
// only place they come from is content this member may already read.
func (s *Server) handleMangroveDirectory(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.mangroveCaller(w, r, false)
	if !ok {
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		s.resolveActorIDs(w, key, raw)
		return
	}
	q := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("q")))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"q" is required`))
		return
	}

	prefix := s.prefixSearchAllowed(key)
	mode := "exact"
	if prefix {
		mode = "prefix"
	}
	// A short needle in prefix mode is a sweep with extra steps. Two characters
	// would enumerate most of a subscription in a handful of tries.
	if prefix && len(q) < 3 {
		writeJSON(w, http.StatusBadRequest, errBody("search needs at least three characters"))
		return
	}

	users, err := s.Mgr.ListSubscriptionUsers(key.TenantID, key.SubsAccID)
	if err != nil {
		s.logf("mangrove: directory: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not read the subscription's members"))
		return
	}

	out := directoryResponse{Mode: mode, Results: []directoryEntry{}}
	for _, u := range users {
		email := strings.ToLower(strings.TrimSpace(u.Email))
		if email == "" || u.AccID == key.UserAccID {
			continue // yourself is not a search result; see capabilities
		}
		match := email == q
		if prefix {
			match = strings.Contains(email, q)
		}
		if !match {
			continue
		}
		e := directoryEntry{Email: u.Email}
		if prefix {
			// Only the friendlier mode hands back the id. In strict mode the
			// member has confirmed reachability and addresses by email.
			e.ActorID = mangroveServiceID(u.AccID)
		}
		out.Results = append(out.Results, e)
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveActorIDs answers the names for actor ids the caller already holds.
//
// UNKNOWN IDS ARE ABSENT, NOT AN ERROR. A card may name somebody who has since
// left the subscription, or an actor from another deployment entirely, and
// neither is a malfunction the member should be shown -- the row simply keeps
// the id it was already showing.
func (s *Server) resolveActorIDs(w http.ResponseWriter, key docker.WorkspaceKey, raw string) {
	wanted := map[string]bool{}
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}
	if len(wanted) > maxResolveIDs {
		writeJSON(w, http.StatusBadRequest, errBody("too many ids in one call"))
		return
	}

	users, err := s.Mgr.ListSubscriptionUsers(key.TenantID, key.SubsAccID)
	if err != nil {
		s.logf("mangrove: resolve: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not read the subscription's members"))
		return
	}

	out := resolveResponse{Resolved: []resolvedEntry{}}
	for _, u := range users {
		if strings.TrimSpace(u.Email) == "" {
			continue
		}
		// BOTH OF THE MEMBER'S ACTORS RESOLVE TO THE SAME PERSON, because they
		// are the same person: the agent is theirs, and a card saying an agent
		// sent something is answering "whose agent". The two ids are what
		// differs, so both are offered and whichever the caller asked for is
		// what comes back.
		for _, id := range []string{mangroveServiceID(u.AccID), mangrovePersonID(u.AccID)} {
			if wanted[id] {
				out.Resolved = append(out.Resolved, resolvedEntry{ID: id, Email: u.Email})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// mangroveServiceID mirrors the mangrove service's own derivation. It is
// duplicated rather than imported because the two are separate modules, and the
// shape is a wire contract between them rather than an implementation detail of
// either.
func mangroveServiceID(accID string) string {
	return "mangrove:actor:" + identity.SanitizeID(accID) + ":service"
}

func mangrovePersonID(accID string) string {
	return "mangrove:actor:" + identity.SanitizeID(accID) + ":person"
}

// subscriptionByEmail indexes the caller's own subscription by lowercased email.
//
// It is the single lookup behind both ways of addressing by email -- an
// `email:<address>` entry in an agent's audience, and the webapp's structured
// `toEmails`. One function so the two cannot disagree about who is in a
// subscription, and so the membership call crosses the process boundary once.
func (s *Server) subscriptionByEmail(key docker.WorkspaceKey) (map[string]string, error) {
	users, err := s.Mgr.ListSubscriptionUsers(key.TenantID, key.SubsAccID)
	if err != nil {
		return nil, err
	}
	byEmail := make(map[string]string, len(users))
	for _, u := range users {
		if e := strings.ToLower(strings.TrimSpace(u.Email)); e != "" {
			byEmail[e] = u.AccID
		}
	}
	return byEmail, nil
}

// resolveAudience turns any `email:<address>` entry in an audience into the
// actor id it names, leaving every other entry untouched.
//
// THIS IS WHAT MAKES STRICT MODE USABLE. A member who is never shown an id has
// to be able to address the person they found, and the address they have is an
// email. Resolving it HERE rather than in the mangrove service keeps that
// service's contract on actor ids, so its containment gate goes on comparing the
// thing it was designed to compare.
//
// An address that matches nobody in the caller's own subscription is left as-is
// and refused downstream by the gate, which already names what it refused --
// rather than being silently dropped, which would deliver to fewer people than
// the member asked for.
func (s *Server) resolveAudience(key docker.WorkspaceKey, audience []string) ([]string, error) {
	needsLookup := false
	for _, a := range audience {
		if strings.HasPrefix(a, "email:") {
			needsLookup = true
			break
		}
	}
	if !needsLookup {
		return audience, nil
	}
	byEmail, err := s.subscriptionByEmail(key)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(audience))
	for _, a := range audience {
		addr, ok := strings.CutPrefix(a, "email:")
		if !ok {
			out = append(out, a)
			continue
		}
		if acc, found := byEmail[strings.ToLower(strings.TrimSpace(addr))]; found {
			out = append(out, mangroveServiceID(acc))
			continue
		}
		// Unresolvable: keep it, so the gate refuses it by name.
		out = append(out, a)
	}
	return out, nil
}

// identityResponse is what a member needs in order to be found by somebody who
// cannot search for them: their own ids, to send out of band.
type identityResponse struct {
	Email     string `json:"email"`
	PersonID  string `json:"personId"`
	ServiceID string `json:"serviceId"`
}

// handleMangroveIdentity serves GET /v1/mangrove/identity.
//
// Your own ids are not a disclosure: they are yours, and the whole point of
// having them is to hand them to somebody else. This exists separately from the
// directory because the directory can be switched to a mode that hides ids, and
// that switch must never take away a member's ability to give out their own.
func (s *Server) handleMangroveIdentity(w http.ResponseWriter, r *http.Request) {
	key, ident, ok := s.mangroveCaller(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, identityResponse{
		Email:     ident.Email,
		PersonID:  mangrovePersonID(key.UserAccID),
		ServiceID: mangroveServiceID(key.UserAccID),
	})
}
