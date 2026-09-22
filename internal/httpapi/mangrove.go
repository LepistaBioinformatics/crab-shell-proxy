package httpapi

import (
	"crypto/subtle"
	"net/http"
)

// The one question crab-mangrove-network asks this proxy.
//
// The mangrove keeps NO membership list of its own, deliberately: a stored list is
// a second source of truth that can disagree with mycelium, and every rule
// about who may address what would then depend on which of the two was
// consulted. So when its reachability gate has to answer "does this person have
// a workspace under a subscription the caller shares", it asks here.
//
// WHAT THIS DISCLOSES, stated plainly because it is the reason for the gate
// below: the account ids and emails of everyone with a workspace under one
// subscription. That is narrower than GET /v1/instances, which spans the whole
// deployment, but it is still a membership roll and it is not something an
// agent's token may buy.

type mangroveMemberView struct {
	AccID string `json:"accId"`
	Role  string `json:"role"`
	Email string `json:"email"`
}

type mangroveMembersResponse struct {
	AccIDs  []string             `json:"accIds"`
	Members []mangroveMemberView `json:"members"`
}

// authorizeMangrove proves the caller is crab-mangrove-network.
//
// It is a SEPARATE credential from the telemetry token and from any agent
// token, for the same reason those are separate from each other: a credential
// that gates one capability must not also gate another, or revoking it for one
// reason silently revokes — or fails to revoke — the other.
func (s *Server) authorizeMangrove(r *http.Request) bool {
	want := s.Cfg.ResolvedMangroveToken
	if want == "" {
		return false
	}
	got := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+want)) == 1
}

// handleMangroveSubscriptionMembers serves GET /v1/mangrove/subscription-members.
//
// Like /v1/instances it does NOT go through resolveAgent: the question spans a
// subscription rather than one agent, so no single agent's token is the right
// key for it.
func (s *Server) handleMangroveSubscriptionMembers(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeMangrove(r) {
		writeJSON(w, http.StatusUnauthorized, errBody("invalid mangrove token"))
		return
	}
	tenantID := r.URL.Query().Get("tenant_id")
	subsAccID := r.URL.Query().Get("subs_acc_id")
	if tenantID == "" || subsAccID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("tenant_id and subs_acc_id are required"))
		return
	}

	users, err := s.Mgr.ListSubscriptionUsers(tenantID, subsAccID)
	if err != nil {
		s.logf("mangrove: subscription members: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not read the subscription's members"))
		return
	}

	// accIds is what the reachability gate actually uses; members carries the
	// labels so a human-facing surface does not need a second call. An empty
	// subscription answers with empty lists rather than an error -- a
	// subscription nobody has a workspace under is a normal state, and reading
	// it as a failure would turn "nobody to share with yet" into "the service
	// is broken".
	out := mangroveMembersResponse{
		AccIDs:  make([]string, 0, len(users)),
		Members: make([]mangroveMemberView, 0, len(users)),
	}
	for _, u := range users {
		out.AccIDs = append(out.AccIDs, u.AccID)
		out.Members = append(out.Members, mangroveMemberView{AccID: u.AccID, Role: u.Role, Email: u.Email})
	}
	writeJSON(w, http.StatusOK, out)
}
