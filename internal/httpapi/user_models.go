package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// The member-facing half of user-owned-models: a person registering their own
// model, proving it answers, and choosing between it and whatever their
// administrator provides.
//
// Everything here is scoped to the CALLER. There is no route that names another
// member's model — the owner is taken from the injected profile, never from the
// request — so the authorization question is only ever "may this person use this
// workspace", which authorizeSecret already answers.

// userModelRequest is the register/edit body. api_key is a POINTER for the same
// reason it is in the inventory's modelRequest: it is the one field no response
// ever returns, so it cannot round-trip. Absent means "keep the stored key";
// present-and-empty is rejected rather than treated as a clear, because a
// personal model without a key cannot resolve at all.
type userModelRequest struct {
	Slug      string          `json:"slug"`
	Label     string          `json:"label"`
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	APIBase   string          `json:"api_base"`
	APIKey    *string         `json:"api_key"`
	ExtraBody json.RawMessage `json:"extra_body"`
	Version   uint64          `json:"version"`
	TenantID  string          `json:"tenant_id"`
	SubsAccID string          `json:"subs_acc_id"`
}

// resolveUserModelCaller runs the preamble every route here shares. write selects
// which permission the profile must carry: reading one's own list needs read,
// registering or selecting is a write.
func (s *Server) resolveUserModelCaller(
	w http.ResponseWriter, r *http.Request, write bool,
) (docker.WorkspaceKey, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.WorkspaceKey{}, false
	}
	tenantID, subsAccID, ok := workspaceParams(w, r)
	if !ok {
		return docker.WorkspaceKey{}, false
	}
	if write {
		return s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	}
	return s.authorizeRestartRead(w, agent, ident, tenantID, subsAccID)
}

// workspaceParams reads the workspace pair from the query string, or — for a
// body-carrying method — from the body the caller already decoded. Query first,
// because that is where every other member route carries it.
func workspaceParams(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	q := r.URL.Query()
	tenantID, err := uuid.Parse(q.Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return uuid.UUID{}, uuid.UUID{}, false
	}
	subsAccID, err := uuid.Parse(q.Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return uuid.UUID{}, uuid.UUID{}, false
	}
	return tenantID, subsAccID, true
}

// requireAllowedEndpoint refuses an api_base outside the catalog unless an
// administrator opened this scope to them.
//
// The default is refusal, which is the opposite of the personal-model switch and
// deliberately so: picking a provider chooses among endpoints the instance
// already ships, while typing one aims the proxy's outbound request wherever the
// member likes. The address guard in the probe is a floor under that, not a
// licence for it.
//
// An api_base that MATCHES the catalog's is never custom, so the ordinary path —
// pick a provider, let the form fill the endpoint — needs no permission at all.
func (s *Server) requireAllowedEndpoint(w http.ResponseWriter, ref registry.WorkspaceRef, provider, apiBase string) bool {
	if sameEndpointAs(apiBase, catalogEndpointFor(provider)) {
		return true
	}
	allowed, by, err := s.Reg.CustomEndpointAllowed(ref)
	if err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return false
	}
	if allowed {
		return true
	}
	_ = by
	writeJSON(w, http.StatusForbidden, errBody("custom_endpoint_not_allowed"))
	return false
}

func userRef(key docker.WorkspaceKey) registry.WorkspaceRef {
	return registry.WorkspaceRef{
		TenantID: key.TenantID, SubsAccID: key.SubsAccID,
		Agent: key.Role, UserAccID: key.UserAccID,
	}
}

// userModelErrStatus maps a failure to an HTTP status and a CODE the interface
// resolves into the member's language. It is separate from registryErrStatus,
// which answers administrators: an admin reads the proxy's prose (and its logs),
// while a member gets "Something went wrong" for every one of these unless the
// wire carries something lib/i18n/errors.ts can look up.
func userModelErrStatus(err error) (int, any) {
	var input inputError
	switch {
	case errors.As(err, &input):
		return http.StatusBadRequest, errBody(input.Code)
	case errors.Is(err, registry.ErrUserModelLimit):
		return http.StatusBadRequest, errBody("user_models_cap")
	case errors.Is(err, registry.ErrUserModelDisabled):
		return http.StatusBadRequest, errBody("user_model_disabled")
	case errors.Is(err, registry.ErrDuplicate):
		return http.StatusConflict, errBody("user_model_duplicate")
	case errors.Is(err, registry.ErrVersionConflict):
		return http.StatusConflict, errBody("version_conflict")
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound, errBody("not_found")
	case errors.Is(err, registry.ErrInvalid):
		return http.StatusBadRequest, errBody("user_model_invalid")
	}
	return http.StatusInternalServerError, errBody("unknown")
}

// handleUserModelsList answers everything the drawer needs in ONE call: the
// member's own models, which one is selected, whether personal models are
// allowed here at all, and what the administrator's cascade currently resolves.
//
// One call rather than four because the four answers are only meaningful
// together: "you selected X" plus "your scope blocks personal models" is a
// different screen from either half alone, and two round trips could show the
// member a state that never existed.
func (s *Server) handleUserModelsList(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveUserModelCaller(w, r, false)
	if !ok {
		return
	}
	ref := userRef(key)

	models, err := s.Reg.ListUserModels(key.UserAccID)
	if err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	out := make([]registry.PublicUserModel, 0, len(models))
	for _, m := range models {
		out = append(out, registry.PublicUser(m))
	}

	selected := ""
	if sel, err := s.Reg.GetUserSelection(ref); err == nil {
		selected = sel.Slug
	} else if !errors.Is(err, registry.ErrNotFound) {
		s.logf("user models: read selection %s: %v", ref.Key(), err)
	}

	allowed, blockedBy, err := s.Reg.UserModelsAllowed(ref)
	if err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}

	// What the member falls back to, named. Without it the switch's other option
	// reads "the organisation's model" with no way to tell WHICH — and that is
	// the option they are asked to trust when their own one fails.
	organisation := ""
	if name, _, err := s.Reg.ScopeCandidate(ref); err == nil {
		organisation = name
	}

	// Reported so the form can present the endpoint as fixed rather than let a
	// member type one and be refused on submit.
	customAllowed, _, err := s.Reg.CustomEndpointAllowed(ref)
	if err != nil {
		s.logf("user models: read endpoint policy %s: %v", ref.Key(), err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models":                  out,
		"selected":                selected,
		"allowed":                 allowed,
		"blocked_by":              string(blockedBy),
		"organisation_model":      organisation,
		"custom_endpoint_allowed": customAllowed,
		"providers":               UserModelProviderOptions(),
	})
}

func (s *Server) handleUserModelCreate(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveUserModelCaller(w, r, true)
	if !ok {
		return
	}
	var req userModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if req.APIKey == nil || *req.APIKey == "" {
		status, body := userModelErrStatus(errAPIKeyRequired)
		writeJSON(w, status, body)
		return
	}
	if !userModelProviders[strings.ToLower(strings.TrimSpace(req.Provider))] {
		status, body := userModelErrStatus(errProviderNotAllowed)
		writeJSON(w, status, body)
		return
	}
	if _, err := probeURL(req.APIBase); err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	if !s.requireAllowedEndpoint(w, userRef(key), req.Provider, req.APIBase) {
		return
	}
	created, err := s.Reg.CreateUserModel(registry.UserModel{
		OwnerAccID: key.UserAccID,
		Slug:       strings.ToLower(strings.TrimSpace(req.Slug)),
		Label:      strings.TrimSpace(req.Label),
		Provider:   strings.ToLower(strings.TrimSpace(req.Provider)),
		Model:      strings.TrimSpace(req.Model),
		APIBase:    strings.TrimRight(strings.TrimSpace(req.APIBase), "/"),
		APIKey:     *req.APIKey,
		ExtraBody:  req.ExtraBody,
	})
	if err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	s.logf("user models: registered user=%s slug=%s provider=%s", key.UserAccID, created.Slug, created.Provider)
	writeJSON(w, http.StatusOK, map[string]any{"model": registry.PublicUser(created)})
}

func (s *Server) handleUserModelUpdate(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveUserModelCaller(w, r, true)
	if !ok {
		return
	}
	slug := r.URL.Query().Get("slug")
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request"))
		return
	}
	var req userModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if !userModelProviders[strings.ToLower(strings.TrimSpace(req.Provider))] {
		status, body := userModelErrStatus(errProviderNotAllowed)
		writeJSON(w, status, body)
		return
	}
	if _, err := probeURL(req.APIBase); err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	if !s.requireAllowedEndpoint(w, userRef(key), req.Provider, req.APIBase) {
		return
	}
	updated, err := s.Reg.UpdateUserModel(key.UserAccID, slug, req.Version, func(m *registry.UserModel) error {
		m.Label = strings.TrimSpace(req.Label)
		m.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
		m.Model = strings.TrimSpace(req.Model)
		m.APIBase = strings.TrimRight(strings.TrimSpace(req.APIBase), "/")
		m.ExtraBody = req.ExtraBody
		if req.APIKey != nil {
			if *req.APIKey == "" {
				return inputError{"api_key_not_clearable"}
			}
			m.APIKey = *req.APIKey
		}
		return nil
	})
	if err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	// An edit reaches the workspaces running this model, and only those. A member
	// editing a model they have not selected changes nothing on disk, so nothing
	// is re-materialized and no notice is raised.
	s.reapplyPersonalModel(key, slug)
	writeJSON(w, http.StatusOK, map[string]any{"model": registry.PublicUser(updated)})
}

func (s *Server) handleUserModelDelete(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveUserModelCaller(w, r, true)
	if !ok {
		return
	}
	slug := r.URL.Query().Get("slug")
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request"))
		return
	}
	// The refs are collected BEFORE the delete: DeleteUserModel drops the
	// selections in the same transaction, so afterwards there is nothing left to
	// tell us which workspaces have to fall back to the administrator's model.
	refs, err := s.Reg.SelectionsOf(key.UserAccID, slug)
	if err != nil {
		s.logf("user models: selections of %s/%s: %v", key.UserAccID, slug, err)
	}
	if err := s.Reg.DeleteUserModel(key.UserAccID, slug); err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	for _, ref := range refs {
		s.reapplyRef(ref)
	}
	s.logf("user models: deleted user=%s slug=%s", key.UserAccID, slug)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleUserModelSelect points this workspace at one of the caller's own models.
func (s *Server) handleUserModelSelect(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveUserModelCaller(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	ref := userRef(key)
	// Refusing here rather than storing a selection the resolver will ignore: a
	// switch that appears to work and changes nothing is the exact failure this
	// feature is supposed to remove.
	if allowed, by, err := s.Reg.UserModelsAllowed(ref); err == nil && !allowed {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":      "user_models_blocked",
			"blocked_by": string(by),
		})
		return
	}
	if err := s.Reg.SetUserSelection(ref, req.Slug); err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	s.reapplyRef(ref)
	s.logf("user models: selected user=%s agent=%s slug=%s", key.UserAccID, key.Role, req.Slug)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "selected": req.Slug})
}

// handleUserModelDeselect returns the workspace to the administrator's cascade.
// It is a separate route from select rather than "select nothing", so the client
// cannot express it by accident with an empty field.
func (s *Server) handleUserModelDeselect(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveUserModelCaller(w, r, true)
	if !ok {
		return
	}
	ref := userRef(key)
	if err := s.Reg.ClearUserSelection(ref); err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	s.reapplyRef(ref)
	s.logf("user models: deselected user=%s agent=%s", key.UserAccID, key.Role)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "selected": ""})
}

// handleUserModelTest probes an UNSAVED draft. When the draft names a saved
// model and carries no key, the stored key is used — so re-testing an existing
// model does not require the member to retype a credential they cannot read back.
func (s *Server) handleUserModelTest(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveUserModelCaller(w, r, true)
	if !ok {
		return
	}
	var req userModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if !s.probes.allow(key.UserAccID) {
		writeJSON(w, http.StatusTooManyRequests, errBody("probe_too_soon"))
		return
	}

	draft := probeDraft{
		Provider:  strings.ToLower(strings.TrimSpace(req.Provider)),
		Model:     strings.TrimSpace(req.Model),
		APIBase:   strings.TrimSpace(req.APIBase),
		ExtraBody: req.ExtraBody,
	}
	slug := strings.ToLower(strings.TrimSpace(req.Slug))
	if req.APIKey != nil && *req.APIKey != "" {
		draft.APIKey = *req.APIKey
	} else if slug != "" {
		stored, err := s.Reg.GetUserModel(key.UserAccID, slug)
		if err != nil {
			status, body := userModelErrStatus(errAPIKeyRequired)
			writeJSON(w, status, body)
			return
		}
		// Only when the endpoint is unchanged. Attaching a stored credential to
		// an api_base the member just retyped would send their key somewhere they
		// never saved it to.
		if !sameEndpoint(stored, draft) {
			status, body := userModelErrStatus(inputError{"api_key_required_new_endpoint"})
			writeJSON(w, status, body)
			return
		}
		draft.APIKey = stored.APIKey
	}
	if err := validateProbeDraft(draft); err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	// The probe is the one place an unsaved endpoint reaches the network, so the
	// permission is checked HERE too rather than only at save: testing an endpoint
	// you may not register is the request this rule exists to stop.
	if !s.requireAllowedEndpoint(w, userRef(key), draft.Provider, draft.APIBase) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout+time.Second)
	defer cancel()
	outcome, err := runProbe(ctx, draft)
	if err != nil {
		status, body := userModelErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	// A saved model remembers its last verdict, so the list can show it without
	// the member re-testing. An unsaved draft has nowhere to record it — the
	// response is the whole result.
	if slug != "" {
		if err := s.Reg.RecordUserModelTest(key.UserAccID, slug, outcome.result(time.Now().UTC())); err != nil &&
			!errors.Is(err, registry.ErrNotFound) {
			s.logf("user models: record test %s/%s: %v", key.UserAccID, slug, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          outcome.OK,
		"status_code": outcome.StatusCode,
		"latency_ms":  outcome.LatencyMS,
		"detail":      outcome.Detail,
	})
}

// sameEndpoint reports whether a draft still points where the stored model does.
func sameEndpoint(stored registry.UserModel, d probeDraft) bool {
	return strings.EqualFold(stored.Provider, d.Provider) &&
		strings.TrimRight(stored.APIBase, "/") == strings.TrimRight(d.APIBase, "/")
}

// reapplyRef re-materializes one workspace and leaves a restart notice on it.
//
// bounce=false always: a member changing their model mid-conversation must not
// have the container pulled out from under them (restart-control DEC-3). The
// notice is what the banner reads, and they press the button when ready.
func (s *Server) reapplyRef(ref registry.WorkspaceRef) {
	key := docker.WorkspaceKey{
		TenantID: ref.TenantID, SubsAccID: ref.SubsAccID,
		Role: ref.Agent, UserAccID: ref.UserAccID,
	}
	if err := s.Mgr.ReapplyModelUser(key, false); err != nil {
		s.logf("user models: reapply %s: %v", ref.Key(), err)
	}
}

// reapplyPersonalModel re-materializes every workspace of this member that runs
// the given personal model — a member with workspaces under two agents selected
// it twice, and an edit that reached only the one they happen to be looking at
// would leave the other on a stale key.
func (s *Server) reapplyPersonalModel(key docker.WorkspaceKey, slug string) {
	refs, err := s.Reg.SelectionsOf(key.UserAccID, slug)
	if err != nil {
		s.logf("user models: selections of %s/%s: %v", key.UserAccID, slug, err)
		return
	}
	for _, ref := range refs {
		s.reapplyRef(ref)
	}
}
