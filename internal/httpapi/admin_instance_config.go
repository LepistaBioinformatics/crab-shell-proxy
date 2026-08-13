package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

// Instance config administration (admin-instance-config-editor): read and
// replace ONE workspace's config.json, so an admin can repair an instance whose
// configuration is broken without host access and without destroying the
// member's transcripts, uploads and skills.
//
// This is NOT the content endpoint admin-shared-content FR-7 forbids, and the
// "no content route here" instructions in admin.go (user files), in the webapp's
// BFF route and in members-panel.tsx all stay in force. The distinction:
//
//   - FR-7's subject is the set ListUserFiles enumerates, which is the UPLOADS
//     dir alone (shared.go → config.PublicDir). config.json is not in it.
//   - FR-7 protects MEMBER-AUTHORED content. config.json is proxy-materialized
//     provisioning state: the proxy seeds it and rewrites six of its paths
//     (docker.ManagedConfigPaths) on every materialization.
//   - This route takes no name/path/file parameter. It addresses exactly
//     <userDir>/config.json and cannot be pointed anywhere else. Do not add one.

// adminInstanceKey resolves the (tenant, subscription, agent, user) key an
// instance-config op targets, gating it on user-management authority — the same
// authority the sibling /v1/admin/users/files endpoints require.
//
// Unlike adminUserFileKey, the agent comes from an EXPLICIT `agent` parameter
// rather than from the addressed agent (the auth vehicle). The webapp routes
// every admin call through /alpha/v1/admin, so inheriting the vehicle would read
// and repair ALPHA's config while the admin believes they are fixing beta's.
// `agent` is required and may not be "all": a workspace has exactly one role.
func (s *Server) adminInstanceKey(w http.ResponseWriter, r *http.Request, ident identity.Identity) (docker.WorkspaceKey, bool) {
	q := r.URL.Query()
	tenantID, err := uuid.Parse(q.Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return docker.WorkspaceKey{}, false
	}
	subsAccID, err := uuid.Parse(q.Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return docker.WorkspaceKey{}, false
	}
	userAccID, err := uuid.Parse(q.Get("user_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"user_acc_id" query parameter is required and must be a UUID`))
		return docker.WorkspaceKey{}, false
	}
	agentKey, msg := s.resolveAgentTarget(q.Get("agent"))
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errBody(msg))
		return docker.WorkspaceKey{}, false
	}
	if agentKey == "" {
		writeJSON(w, http.StatusBadRequest,
			errBody(`"agent" query parameter is required and must name one configured agent`))
		return docker.WorkspaceKey{}, false
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to manage this user's configuration"))
		return docker.WorkspaceKey{}, false
	}
	return docker.WorkspaceKey{
		TenantID:  tenantID.String(),
		SubsAccID: subsAccID.String(),
		Role:      agentKey,
		UserAccID: userAccID.String(),
	}, true
}

// handleAdminInstanceConfigGet returns one workspace's config.json as RAW BYTES
// plus a parse verdict. Deliberately not a parsed object: a config.json that does
// not parse is the primary failure this repairs, so a broken file must still
// load. It comes back 200 with valid=false — it is data, not an error.
func (s *Server) handleAdminInstanceConfigGet(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	key, ok := s.adminInstanceKey(w, r, ident)
	if !ok {
		return
	}
	cfg, err := s.Mgr.ReadInstanceConfig(key)
	if err != nil {
		s.writeInstanceConfigError(w, key, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

type instanceConfigRequest struct {
	Raw string `json:"raw"`
	// Revision is the token the GET returned. Empty skips the check, which only a
	// non-UI caller would do.
	Revision string `json:"revision"`
}

// handleAdminInstanceConfigPut replaces one workspace's config.json.
//
// The restart policy is parsed BEFORE the write: validating it afterwards would
// 400 a write that already succeeded, telling the admin their repair failed when
// only the restart instruction was unusable. Delivery then reuses the
// per-workspace reduction every targeted model re-apply uses — `schedule`
// behaves as `notice` there because the scheduler arms per SCOPE, and scheduling
// one member's config change would bounce every member under it.
func (s *Server) handleAdminInstanceConfigPut(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	key, ok := s.adminInstanceKey(w, r, ident)
	if !ok {
		// A refused write is audited too, and this is the only place that can do
		// it: adminInstanceKey has already answered the request, and a 403 on a
		// sandbox-boundary-capable endpoint is the line an operator most wants in
		// the log. The key is unusable here, so the raw parameters are what gets
		// recorded.
		s.logInstanceConfigRefusal(ident, r, "rejected")
		return
	}
	bounce, ok := s.bounceNow(w, r)
	if !ok {
		s.logInstanceConfigRefusal(ident, r, "bad-restart-policy")
		return
	}

	// The body is capped at the reader so an oversize document is refused before
	// it is buffered, not after.
	var req instanceConfigRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInstanceConfigBody))
	if err != nil {
		s.logInstanceConfigWrite(ident, key, 0, false, 0, "unreadable-body", false)
		writeJSON(w, http.StatusBadRequest, errBody("could not read request body"))
		return
	}
	if len(body) >= maxInstanceConfigBody {
		s.logInstanceConfigWrite(ident, key, 0, false, 0, "413", false)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "too_large"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.logInstanceConfigWrite(ident, key, 0, false, 0, "malformed-envelope", false)
		writeJSON(w, http.StatusBadRequest, errBody("body must be JSON: {\"raw\": \"…\", \"revision\": \"…\"}"))
		return
	}

	before, readErr := s.Mgr.ReadInstanceConfig(key)
	if readErr != nil {
		s.writeInstanceConfigError(w, key, readErr)
		return
	}

	cfg, reapplied, err := s.Mgr.WriteInstanceConfig(key, req.Raw, req.Revision)
	if err != nil {
		s.logInstanceConfigWrite(ident, key, before.Size, before.Valid, 0, err.Error(), false)
		s.writeInstanceConfigError(w, key, err)
		return
	}
	s.logInstanceConfigWrite(ident, key, before.Size, before.Valid, cfg.Size, "ok", reapplied.OK)

	// The write already landed, so a restart failure is logged rather than
	// returned — the same best-effort contract every other admin mutation has.
	if bounce {
		if err := s.Mgr.RestartWorkspace(key); err != nil {
			s.logf("instance config: restart %+v failed: %v", key, err)
		}
	} else if err := s.Mgr.RaiseWorkspaceRestartNotice(key, restart.ReasonConfig); err != nil {
		s.logf("instance config: raise notice %+v failed: %v", key, err)
	}

	writeJSON(w, http.StatusOK, instanceConfigWriteResponse{
		InstanceConfig: cfg,
		Reapplied:      reapplied,
	})
}

// maxInstanceConfigBody bounds the whole request envelope. It is deliberately
// looser than the document cap the docker layer enforces: JSON string escaping
// inside `raw` can inflate the same document, and the authoritative limit is the
// one applied to the document itself.
const maxInstanceConfigBody = 4 << 20

type instanceConfigWriteResponse struct {
	docker.InstanceConfig
	// Reapplied reports the post-write materialization. ok=false means the
	// configuration IS saved but the model resolution could not be re-imposed —
	// never that the save failed.
	Reapplied docker.ReapplyResult `json:"reapplied"`
}

func (s *Server) writeInstanceConfigError(w http.ResponseWriter, key docker.WorkspaceKey, err error) {
	switch {
	case errors.Is(err, docker.ErrNotProvisioned):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_provisioned"})
	case errors.Is(err, docker.ErrStaleRevision):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "stale_revision"})
	case errors.Is(err, docker.ErrConfigTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "too_large"})
	case errors.Is(err, docker.ErrConfigNotObject):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid_json", "detail": err.Error(), "offset": -1,
		})
	default:
		var se *json.SyntaxError
		if errors.As(err, &se) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid_json", "detail": se.Error(), "offset": se.Offset,
			})
			return
		}
		s.logf("admin: instance config op failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
	}
}

// logInstanceConfigWrite is the audit line. Write access to config.json is write
// access to the agent's sandbox boundary (restrict_to_workspace,
// allow_read_outside_workspace, tools.exec deny patterns), so every attempt —
// accepted or refused — has to be reconstructable from the logs.
//
// The document itself is NEVER logged: a workspace not materialized since the
// model-registry migration can still carry credentials in model_list.
// handleAdminInstanceRestart bounces ONE member's workspace on an admin's behalf.
//
// restart-control deliberately did not build this: the notice model is per scope,
// so a targeted bounce would be "a bounce with no notice attached", and members
// were expected to press their own button. That reasoning does not survive this
// feature. A workspace whose config.json is broken may not boot picoclaw at all,
// so its member cannot reach a restart button — the notice path is useless for
// exactly the instance an admin just repaired. The bounce here is attached to that
// repair, which is the missing piece.
//
// It is a separate route rather than a flag on the PUT because a repair is often
// several saves, and an admin should be able to apply the result once when they
// are done — and because an instance can need a bounce for a change made from
// another screen.
func (s *Server) handleAdminInstanceRestart(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	key, ok := s.adminInstanceKey(w, r, ident)
	if !ok {
		s.logf("admin: instance restart by=%s result=rejected", callerLabel(ident))
		return
	}
	if err := s.Mgr.RestartWorkspace(key); err != nil {
		s.logf("admin: instance restart by=%s key=%+v failed: %v", callerLabel(ident), key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	st, err := s.Mgr.RestartStatus(key)
	if err != nil {
		s.logf("admin: instance restart status by=%s key=%+v failed: %v", callerLabel(ident), key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	// "noop" when the container was absent or scaled to zero: nothing was bounced,
	// but the pending notice is resolved all the same, because the next cold start
	// begins from the repaired file. Same contract as the member's own restart.
	status := "restarted"
	if !st.Running {
		status = "noop"
	}
	s.logf("admin: instance restart by=%s tenant=%s subs=%s agent=%s user=%s result=%s",
		callerLabel(ident), key.TenantID, key.SubsAccID, key.Role, key.UserAccID, status)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        status,
		"lastRestartAt": st.LastRestartAt,
	})
}

// logInstanceConfigRefusal audits an attempt that never reached the manager: the
// key could not be built, the caller was refused, or the restart policy was
// unusable. The workspace it TARGETED is still worth recording, so the raw
// parameters are logged as given — they are query values, not paths, and nothing
// here reaches the filesystem.
func (s *Server) logInstanceConfigRefusal(ident identity.Identity, r *http.Request, result string) {
	q := r.URL.Query()
	s.logf("admin: instance config write by=%s tenant=%s subs=%s agent=%s user=%s result=%s",
		callerLabel(ident), q.Get("tenant_id"), q.Get("subs_acc_id"), q.Get("agent"), q.Get("user_acc_id"), result)
}

// callerLabel is the audit identity: the email when the profile carries one, else
// the account id.
func callerLabel(ident identity.Identity) string {
	if ident.Email != "" {
		return ident.Email
	}
	return ident.AccID
}

func (s *Server) logInstanceConfigWrite(
	ident identity.Identity, key docker.WorkspaceKey,
	beforeSize int64, beforeValid bool, afterSize int64, result string, reapplied bool,
) {
	s.logf("admin: instance config write by=%s tenant=%s subs=%s agent=%s user=%s before=%dB valid=%t after=%dB result=%s reapplied=%t",
		callerLabel(ident), key.TenantID, key.SubsAccID, key.Role, key.UserAccID,
		beforeSize, beforeValid, afterSize, result, reapplied)
}
