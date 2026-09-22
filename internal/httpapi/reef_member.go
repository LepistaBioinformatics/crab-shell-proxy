package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/reef"
)

// The MEMBER's half of the reef. The agent's half is the `reef_*` MCP tools.
//
// They are separate surfaces on purpose, and not only because one speaks MCP.
// Three of the operations here have NO agent equivalent and must not acquire
// one:
//
//	decide  -- accepting or rejecting a cross-scope publication is a governing
//	           role's call, and the role lives in a mycelium profile an agent
//	           never presents.
//	revoke  -- the human's authority over their own bot. An agent that could
//	           revoke could un-revoke, and the whole point is that it cannot.
//	admit   -- an agent CAN admit (it has reef_admit), but only for itself; the
//	           person admitting on their own behalf is the same operation signed
//	           by the other actor.
//
// TWO FACTS THE REEF CANNOT WORK OUT AND THIS FILE MUST: whether the caller
// governs the scope, and whether they are licensed on the tenant. Both come
// from authz.CallerTier over the injected mycelium profile. The reef does not
// read roles because it is not the component that can -- it has no profile, no
// gateway and no mycelium client.

// reefCaller resolves the member's workspace AND keeps the identity, which
// restartCallerKey drops. The identity is the only place the profile lives, and
// the profile is where the roles are.
func (s *Server) reefCaller(w http.ResponseWriter, r *http.Request, needWrite bool) (docker.WorkspaceKey, identity.Identity, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.WorkspaceKey{}, identity.Identity{}, false
	}
	tenantID, subsAccID, ok := s.reefScopeParams(w, r)
	if !ok {
		return docker.WorkspaceKey{}, identity.Identity{}, false
	}
	var key docker.WorkspaceKey
	if needWrite {
		key, ok = s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	} else {
		key, ok = s.authorizeRestartRead(w, agent, ident, tenantID, subsAccID)
	}
	if !ok {
		return docker.WorkspaceKey{}, identity.Identity{}, false
	}
	return key, ident, true
}

func (s *Server) reefTuple(key docker.WorkspaceKey) reef.Tuple {
	return reef.Tuple{
		TenantID:  key.TenantID,
		SubsAccID: key.SubsAccID,
		Role:      key.Role,
		UserAccID: key.UserAccID,
	}
}

// tenantLicensed reports whether this caller may address their tenant.
//
// THIS IS THE ONLY PLACE IT IS EVER TRUE. The MCP path hard-codes false,
// because an agent's token proves one subscription and cannot prove a tenant.
// A human with tenant-owner or tenant-manager can, and says so from a verified
// profile.
func tenantLicensed(ident identity.Identity, key docker.WorkspaceKey) bool {
	return authz.CallerTier(ident.Profile, key.TenantID, key.SubsAccID) >= authz.TierTenant
}

// governs reports whether this caller may decide a cross-scope publication into
// the subscription -- subscriptions-manager on it, or anything above.
func governs(ident identity.Identity, key docker.WorkspaceKey) bool {
	return authz.CallerTier(ident.Profile, key.TenantID, key.SubsAccID) >= authz.TierSubscription
}

// writeReefResult forwards the reef's answer, including its refusals.
//
// A REFUSAL KEEPS ITS BODY. The reef names the addressee that was out of reach;
// flattening that into a generic 502 would leave the member with nothing to act
// on, which is the failure FR-B6a exists to prevent.
func (s *Server) writeReefResult(w http.ResponseWriter, raw json.RawMessage, err error) {
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}
	var reefErr *reef.Error
	if errors.As(err, &reefErr) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reefErr.Status)
		_, _ = w.Write([]byte(reefErr.Body))
		return
	}
	// Unreachable, not refused. A DIFFERENT state from "nothing shared yet",
	// and the UI must be able to tell them apart.
	s.logf("reef: %v", err)
	writeJSON(w, http.StatusBadGateway, errBody("the reef is unreachable"))
}

// handleReefTimeline serves GET /v1/reef/timeline.
func (s *Server) handleReefTimeline(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.reefCaller(w, r, false)
	if !ok {
		return
	}
	reading := r.URL.Query().Get("reading")
	if reading == "" {
		reading = "received"
	}
	raw, err := s.reefClient().Timeline(r.Context(), s.reefTuple(key), reef.AsPerson, reading)
	s.writeReefResult(w, raw, err)
}

// handleReefAdmit serves POST /v1/reef/admit -- the person letting an object
// somebody sent them into their own agent's memory (FR-B7).
func (s *Server) handleReefAdmit(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.reefCaller(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ActivityID string `json:"activityId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.ActivityID == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"activityId" is required`))
		return
	}
	raw, err := s.reefClient().Admit(r.Context(), s.reefTuple(key), reef.AsPerson, body.ActivityID)
	s.writeReefResult(w, raw, err)
}

// handleReefDecide serves POST /v1/reef/decide -- a governing role accepting or
// rejecting a cross-scope publication (FR-F2).
//
// A REJECT IS NOT A FAILURE. It is a result the author's agent reads and can
// explain, which is how the approver loop already treats a denied tool call.
func (s *Server) handleReefDecide(w http.ResponseWriter, r *http.Request) {
	key, ident, ok := s.reefCaller(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ActivityID string `json:"activityId"`
		Accept     bool   `json:"accept"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.ActivityID == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"activityId" is required`))
		return
	}
	// The role is resolved HERE and sent as a fact. The reef trusts it because
	// this proxy is its one caller; it has no way to resolve a role itself.
	if !governs(ident, key) {
		writeJSON(w, http.StatusForbidden,
			errBody("only a holder of the governing role may decide this scope"))
		return
	}
	raw, err := s.reefClient().Decide(r.Context(), s.reefTuple(key), body.ActivityID, body.Accept, true)
	s.writeReefResult(w, raw, err)
}

// handleReefRevoke serves POST /v1/reef/revoke -- the human tombstoning
// something their own bot published (FR-E3).
//
// Tombstones, does not erase. ActivityPub cannot un-deliver, and the UI says so
// rather than implying otherwise.
func (s *Server) handleReefRevoke(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.reefCaller(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ObjectID string `json:"objectId"`
		Cell     string `json:"cell"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil ||
		body.ObjectID == "" || body.Cell == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"objectId" and "cell" are required`))
		return
	}
	raw, err := s.reefClient().Revoke(r.Context(), s.reefTuple(key), body.ObjectID, body.Cell)
	s.writeReefResult(w, raw, err)
}

// handleReefCapabilities serves GET /v1/reef/capabilities.
//
// It exists so the UI can make the pending-decisions reading ABSENT rather than
// empty for somebody who governs nothing (FR-I5). An affordance that renders
// and then refuses teaches the wrong model of who decides -- and the webapp
// cannot work this out alone, because the roles live in a profile only the
// gateway injects and only this proxy decodes.
func (s *Server) handleReefCapabilities(w http.ResponseWriter, r *http.Request) {
	key, ident, ok := s.reefCaller(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"governs":        governs(ident, key),
		"tenantLicensed": tenantLicensed(ident, key),
	})
}

// reefScopeParams reads the tenant and subscription the call is about. Same
// shape and same messages as every other member route here, so a client that
// got one right gets them all right.
func (s *Server) reefScopeParams(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	tenantID, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return uuid.Nil, uuid.Nil, false
	}
	subsAccID, err := uuid.Parse(r.URL.Query().Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, subsAccID, true
}

// reefClient builds the client from configuration. Cheap enough to build per
// call -- it holds a URL, a token and an http.Client with a timeout -- and
// building it here keeps "is the reef on?" a single expression rather than a
// field that could drift out of step with the config it came from.
func (s *Server) reefClient() *reef.Client {
	return reef.New(s.Cfg.ReefBaseURL, s.Cfg.ResolvedReefToken)
}
