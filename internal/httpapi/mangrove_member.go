package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mangrove"
)

// The MEMBER's half of the mangrove. The agent's half is the `mangrove_*` MCP tools.
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
//	admit   -- an agent CAN admit (it has mangrove_admit), but only for itself; the
//	           person admitting on their own behalf is the same operation signed
//	           by the other actor.
//
// TWO FACTS THE MANGROVE CANNOT WORK OUT AND THIS FILE MUST: whether the caller
// governs the scope, and whether they are licensed on the tenant. Both come
// from authz.CallerTier over the injected mycelium profile. The mangrove does not
// read roles because it is not the component that can -- it has no profile, no
// gateway and no mycelium client.

// mangroveWriteCaller is mangroveCaller plus the two things a WRITE that touches
// the workspace needs and a read does not: which agent (for its harness, which
// decides the directory layout) and which project (each keeps its own files and
// its own graph).
//
// It is a separate function rather than a wider mangroveCaller because the seven
// routes that only read must not start carrying a project they never use -- an
// unused parameter is where a future caller gets it wrong.
func (s *Server) mangroveWriteCaller(w http.ResponseWriter, r *http.Request) (docker.WorkspaceKey, identity.Identity, config.Agent, string, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.WorkspaceKey{}, identity.Identity{}, config.Agent{}, "", false
	}
	tenantID, subsAccID, ok := s.mangroveScopeParams(w, r)
	if !ok {
		return docker.WorkspaceKey{}, identity.Identity{}, config.Agent{}, "", false
	}
	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return docker.WorkspaceKey{}, identity.Identity{}, config.Agent{}, "", false
	}
	_, project, ok := s.workspaceSegmentFor(w, r, agent.Harness, key)
	if !ok {
		return docker.WorkspaceKey{}, identity.Identity{}, config.Agent{}, "", false
	}
	return key, ident, agent, project, true
}

// mangroveCaller resolves the member's workspace AND keeps the identity, which
// restartCallerKey drops. The identity is the only place the profile lives, and
// the profile is where the roles are.
func (s *Server) mangroveCaller(w http.ResponseWriter, r *http.Request, needWrite bool) (docker.WorkspaceKey, identity.Identity, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.WorkspaceKey{}, identity.Identity{}, false
	}
	tenantID, subsAccID, ok := s.mangroveScopeParams(w, r)
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

func (s *Server) mangroveTuple(key docker.WorkspaceKey) mangrove.Tuple {
	return mangrove.Tuple{
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

// writeMangroveResult forwards the mangrove's answer, including its refusals.
//
// A REFUSAL KEEPS ITS BODY. The mangrove names the addressee that was out of reach;
// flattening that into a generic 502 would leave the member with nothing to act
// on, which is the failure FR-B6a exists to prevent.
func (s *Server) writeMangroveResult(w http.ResponseWriter, raw json.RawMessage, err error) {
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}
	var mangroveErr *mangrove.Error
	if errors.As(err, &mangroveErr) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(mangroveErr.Status)
		_, _ = w.Write([]byte(mangroveErr.Body))
		return
	}
	// Unreachable, not refused. A DIFFERENT state from "nothing shared yet",
	// and the UI must be able to tell them apart.
	s.logf("mangrove: %v", err)
	writeJSON(w, http.StatusBadGateway, errBody("the mangrove is unreachable"))
}

// handleMangroveTimeline serves GET /v1/mangrove/timeline.
func (s *Server) handleMangroveTimeline(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.mangroveCaller(w, r, false)
	if !ok {
		return
	}
	reading := r.URL.Query().Get("reading")
	if reading == "" {
		reading = "received"
	}
	raw, err := s.mangroveClient().Timeline(r.Context(), s.mangroveTuple(key), mangrove.AsPerson, reading)
	s.writeMangroveResult(w, raw, err)
}

// handleMangroveAdmit serves POST /v1/mangrove/admit -- the person letting an object
// somebody sent them into their own agent's memory (FR-B7).
func (s *Server) handleMangroveAdmit(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.mangroveCaller(w, r, true)
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
	raw, err := s.mangroveClient().Admit(r.Context(), s.mangroveTuple(key), mangrove.AsPerson, body.ActivityID)
	s.writeMangroveResult(w, raw, err)
}

// handleMangroveDecide serves POST /v1/mangrove/decide -- a governing role accepting or
// rejecting a cross-scope publication (FR-F2).
//
// A REJECT IS NOT A FAILURE. It is a result the author's agent reads and can
// explain, which is how the approver loop already treats a denied tool call.
func (s *Server) handleMangroveDecide(w http.ResponseWriter, r *http.Request) {
	key, ident, ok := s.mangroveCaller(w, r, true)
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
	// The role is resolved HERE and sent as a fact. The mangrove trusts it because
	// this proxy is its one caller; it has no way to resolve a role itself.
	if !governs(ident, key) {
		writeJSON(w, http.StatusForbidden,
			errBody("only a holder of the governing role may decide this scope"))
		return
	}
	raw, err := s.mangroveClient().Decide(r.Context(), s.mangroveTuple(key), body.ActivityID, body.Accept, true)
	s.writeMangroveResult(w, raw, err)
}

// handleMangroveRevoke serves POST /v1/mangrove/revoke -- the human tombstoning
// something their own bot published (FR-E3).
//
// Tombstones, does not erase. ActivityPub cannot un-deliver, and the UI says so
// rather than implying otherwise.
func (s *Server) handleMangroveRevoke(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.mangroveCaller(w, r, true)
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
	raw, err := s.mangroveClient().Revoke(r.Context(), s.mangroveTuple(key), body.ObjectID, body.Cell)
	s.writeMangroveResult(w, raw, err)
}

// handleMangroveCapabilities serves GET /v1/mangrove/capabilities.
//
// It exists so the UI can make the pending-decisions reading ABSENT rather than
// empty for somebody who governs nothing (FR-I5). An affordance that renders
// and then refuses teaches the wrong model of who decides -- and the webapp
// cannot work this out alone, because the roles live in a profile only the
// gateway injects and only this proxy decodes.
func (s *Server) handleMangroveCapabilities(w http.ResponseWriter, r *http.Request) {
	key, ident, ok := s.mangroveCaller(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"governs":        governs(ident, key),
		"tenantLicensed": tenantLicensed(ident, key),
	})
}

// mangroveScopeParams reads the tenant and subscription the call is about. Same
// shape and same messages as every other member route here, so a client that
// got one right gets them all right.
func (s *Server) mangroveScopeParams(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
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

// mangroveClient builds the client from configuration. Cheap enough to build per
// call -- it holds a URL, a token and an http.Client with a timeout -- and
// building it here keeps "is the mangrove on?" a single expression rather than a
// field that could drift out of step with the config it came from.
func (s *Server) mangroveClient() *mangrove.Client {
	return mangrove.New(s.Cfg.MangroveBaseURL, s.Cfg.ResolvedMangroveToken)
}
