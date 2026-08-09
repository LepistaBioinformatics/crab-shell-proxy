package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// The administrator's half of user-owned-models: seeing what members registered,
// switching one off, and locking a scope.
//
// Seeing is deliberately not optional. An administrator who cannot list personal
// models cannot answer "why did this member's agent start failing", and the lock
// in the next section would be a control over something invisible.

// adminUserModel is one row of the listing: the public record plus the owner's
// place in the organisation, which is what the admin screen groups by.
type adminUserModel struct {
	registry.PublicUserModel
	Agent string `json:"agent"`
}

// handleAdminUserModelsList reports the personal models of every member under one
// subscription.
//
// It enumerates the subscription's members and reads each one's list, rather than
// scanning the store and filtering: the store has no tenant or subscription on a
// personal model (it is per account), so the membership roster is the only
// authority on who belongs here. A store scan would need a second, weaker answer
// to the same question.
func (s *Server) handleAdminUserModelsList(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	tenantID, err := uuid.Parse(q.Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return
	}
	subsAccID, err := uuid.Parse(q.Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this subscription"))
		return
	}
	users, err := s.Mgr.ListSubscriptionUsers(tenantID.String(), subsAccID.String())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	out := []adminUserModel{}
	// A member with workspaces under two agents appears twice in the roster and
	// owns ONE personal list, so the same model would be listed twice. seen keys
	// on owner+slug to report it once, tagged with the first agent it was found
	// under.
	seen := map[string]bool{}
	for _, u := range users {
		models, err := s.Reg.ListUserModels(u.AccID)
		if err != nil {
			s.logf("admin user models: list %s: %v", u.AccID, err)
			continue
		}
		for _, m := range models {
			key := m.OwnerAccID + "/" + m.Slug
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, adminUserModel{PublicUserModel: registry.PublicUser(m), Agent: u.Role})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// handleAdminUserModelStatus is the administrator's switch. Disabling stops the
// model resolving; the member's own workspaces fall back to the cascade on the
// spot, with a restart notice rather than a forced bounce.
//
// It re-checks that the named owner is actually a member of the named
// subscription. Without that, an administrator of one subscription could pass any
// account id and reach a member they have no authority over — the authorization
// above answers "may you administer THIS subscription", not "does this account
// belong to it".
func (s *Server) handleAdminUserModelStatus(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	var req struct {
		TenantID   string `json:"tenant_id"`
		SubsAccID  string `json:"subs_acc_id"`
		OwnerAccID string `json:"owner_acc_id"`
		Slug       string `json:"slug"`
		Enabled    bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return
	}
	subsAccID, err := uuid.Parse(req.SubsAccID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return
	}
	if req.OwnerAccID == "" || req.Slug == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"owner_acc_id" and "slug" are required`))
		return
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this subscription"))
		return
	}
	users, err := s.Mgr.ListSubscriptionUsers(tenantID.String(), subsAccID.String())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	member := false
	for _, u := range users {
		if u.AccID == req.OwnerAccID {
			member = true
			break
		}
	}
	if !member {
		writeJSON(w, http.StatusForbidden, errBody("that account is not a member of this subscription"))
		return
	}

	updated, err := s.Reg.SetUserModelEnabled(req.OwnerAccID, req.Slug, req.Enabled)
	if err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	// Every workspace running it has to be re-materialized either way: disabling
	// drops it back to the cascade, re-enabling puts it back.
	refs, err := s.Reg.SelectionsOf(req.OwnerAccID, req.Slug)
	if err != nil {
		s.logf("admin user models: selections of %s/%s: %v", req.OwnerAccID, req.Slug, err)
	}
	for _, ref := range refs {
		s.reapplyRef(ref)
	}
	s.logf("admin user models: %s/%s enabled=%v by=%s", req.OwnerAccID, req.Slug, req.Enabled, ident.AccID)
	writeJSON(w, http.StatusOK, map[string]any{"model": registry.PublicUser(updated)})
}

// handleAdminModelPolicyGet reports ONE level's lock, without inheritance, so the
// screen can tell "allowed here" from "inherits from above". The effective answer
// is a different question, and the member's own screen is where it is asked.
func (s *Server) handleAdminModelPolicyGet(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.authorizeScopeDefault(w, r, false)
	if !ok {
		return
	}
	p, err := s.Reg.GetScopePolicy(sel)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"policy": nil})
			return
		}
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": p})
}

// handleAdminModelPolicySet writes the lock at one level.
//
// It does NOT re-materialize anything. A lock changes which model a workspace
// resolves, so the workspaces affected are exactly those whose member selected a
// personal model — and finding them means scanning the selection bucket for the
// scope. That scan is done lazily instead: the next EnsureRunning re-resolves,
// and the member's own screen tells them their selection is blocked. Bouncing
// every affected container the moment an administrator flips a policy is the
// disruption restart-control exists to avoid.
func (s *Server) handleAdminModelPolicySet(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.authorizeScopeDefault(w, r, true)
	if !ok {
		return
	}
	// Pointers, so a screen that owns one switch cannot clear the other by
	// omitting it. At least one has to be present, or the write says nothing.
	var req struct {
		AllowUserModels     *bool `json:"allow_user_models"`
		AllowCustomEndpoint *bool `json:"allow_custom_endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if req.AllowUserModels == nil && req.AllowCustomEndpoint == nil {
		writeJSON(w, http.StatusBadRequest,
			errBody(`at least one of "allow_user_models" or "allow_custom_endpoint" is required`))
		return
	}
	patch := registry.ScopePolicy{
		AllowUserModels: req.AllowUserModels, AllowCustomEndpoint: req.AllowCustomEndpoint,
	}
	if err := s.Reg.SetScopePolicy(sel, patch); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleAdminModelPolicyClear removes a level's lock so it inherits again.
func (s *Server) handleAdminModelPolicyClear(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.authorizeScopeDefault(w, r, true)
	if !ok {
		return
	}
	// `?field=` clears one switch; its absence clears the level entirely. A screen
	// with two controls always names the one it is releasing.
	var fields []registry.PolicyField
	switch f := registry.PolicyField(r.URL.Query().Get("field")); f {
	case "":
	case registry.FieldUserModels, registry.FieldCustomEndpoint:
		fields = append(fields, f)
	default:
		writeJSON(w, http.StatusBadRequest, errBody(`"field" must be user_models or custom_endpoint`))
		return
	}
	if err := s.Reg.ClearScopePolicy(sel, fields...); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
