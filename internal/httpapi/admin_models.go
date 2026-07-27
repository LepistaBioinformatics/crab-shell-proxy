package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// modelRequest is the create/update body. PUT is a full replace of every
// field a client can read back via GET — provider, model, api_base,
// auth_method, extra_body and fallbacks are all overwritten with whatever the
// request carries, including zero values, so omitting one of them from a PUT
// body clears it. api_key is the sole exception, and deliberately so: it is
// the one field a client is NEVER given back by any response (see
// registry.PublicModel), so it cannot round-trip a value it never received.
// That is why it alone is a POINTER — absent means "leave the stored key
// alone", an explicit "" means "clear it" — while every other field stays a
// plain value with ordinary full-replace semantics.
type modelRequest struct {
	ModelName  string          `json:"model_name"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	APIBase    string          `json:"api_base"`
	APIKey     *string         `json:"api_key"`
	AuthMethod string          `json:"auth_method"`
	ExtraBody  json.RawMessage `json:"extra_body"`
	Fallbacks  []string        `json:"fallbacks"`
	Version    uint64          `json:"version"`
}

// registryErrStatus maps a registry error to an HTTP status and a body. An in-use
// rejection carries the referrer list, because a bare 409 leaves the admin with
// no next action.
func registryErrStatus(err error) (int, any) {
	var inUse *registry.InUseError
	switch {
	case errors.As(err, &inUse):
		return http.StatusConflict, map[string]any{
			"error":     inUse.Error(),
			"referrers": inUse.Referrers,
		}
	case errors.Is(err, registry.ErrDuplicate):
		return http.StatusConflict, errBody(err.Error())
	case errors.Is(err, registry.ErrVersionConflict):
		return http.StatusConflict, map[string]any{
			"error":            err.Error(),
			"version_conflict": true,
		}
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound, errBody(err.Error())
	case errors.Is(err, registry.ErrInvalid):
		return http.StatusBadRequest, errBody(err.Error())
	case errors.Is(err, registry.ErrNoModelResolvable):
		return http.StatusConflict, errBody(err.Error())
	}
	return http.StatusInternalServerError, errBody(err.Error())
}

// requireProxyAdmin resolves the caller and enforces the proxy-admin gate shared
// by every inventory operation. The inventory holds API keys whose blast radius is
// the whole instance, so a scope-level tier is not enough.
func (s *Server) requireProxyAdmin(w http.ResponseWriter, r *http.Request) bool {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return false
	}
	if !ident.Profile.HasAdminPrivileges() {
		writeJSON(w, http.StatusForbidden,
			errBody("admin privileges required to administer the model inventory"))
		return false
	}
	return true
}

func (s *Server) handleAdminModelsList(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	models, err := s.Reg.ListModels()
	if err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	out := make([]registry.PublicModel, 0, len(models))
	for _, m := range models {
		refs, err := s.Reg.Referrers(m.ModelName)
		if err != nil {
			s.logf("admin models: referrers %q: %v", m.ModelName, err)
		}
		out = append(out, registry.Public(m, len(refs)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (s *Server) handleAdminModelCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	var req modelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	m := registry.Model{
		ModelName: req.ModelName, Provider: req.Provider, Model: req.Model,
		APIBase: req.APIBase, AuthMethod: req.AuthMethod, ExtraBody: req.ExtraBody,
		Fallbacks: req.Fallbacks, Status: registry.StatusActive,
	}
	if req.APIKey != nil {
		m.APIKey = *req.APIKey
	}
	created, err := s.Reg.CreateModel(m)
	if err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": registry.Public(created, 0)})
}

func (s *Server) handleAdminModelUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	var req modelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	updated, err := s.Reg.UpdateModel(name, req.Version, func(cur *registry.Model) error {
		cur.Provider = req.Provider
		cur.Model = req.Model
		cur.APIBase = req.APIBase
		cur.AuthMethod = req.AuthMethod
		cur.ExtraBody = req.ExtraBody
		cur.Fallbacks = req.Fallbacks
		// Absent api_key keeps the stored one; an explicit "" clears it.
		if req.APIKey != nil {
			cur.APIKey = *req.APIKey
		}
		return nil
	})
	if err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	// A definition or key change must reach every workspace holding this model —
	// as primary OR as a chain member. Reaching only primaries would leave the
	// fallback holders on a stale credential.
	bounce, ok := s.bounceNow(w, r)
	if !ok {
		return
	}
	if err := s.Mgr.ReapplyModelForModel(name, bounce); err != nil {
		s.logf("admin models: reapply after updating %q: %v", name, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": registry.Public(updated, 0)})
}

func (s *Server) handleAdminModelDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	if err := s.Reg.DeleteModel(r.PathValue("name")); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

type statusRequest struct {
	Status     registry.Status `json:"status"`
	ReplacedBy string          `json:"replaced_by"`
	Version    uint64          `json:"version"`
}

func (s *Server) handleAdminModelStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	var req statusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	updated, err := s.Reg.SetStatus(r.PathValue("name"), req.Version, req.Status, req.ReplacedBy)
	if err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": registry.Public(updated, 0)})
}

func (s *Server) handleAdminModelDeprecate(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	var req statusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	updated, err := s.Reg.Deprecate(r.PathValue("name"), req.Version, req.ReplacedBy)
	if err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	// Deliberately no re-apply: existing users keeping the model is the point.
	writeJSON(w, http.StatusOK, map[string]any{"model": registry.Public(updated, 0)})
}

func (s *Server) handleAdminModelsReorder(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	var req struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if err := s.Reg.SetPositions(req.Order); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	// Deliberately no re-apply and no restart: position is presentation only.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleAdminModelUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	refs, err := s.Reg.Referrers(r.PathValue("name"))
	if err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	if refs == nil {
		refs = []registry.Referrer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"referrers": refs})
}

func (s *Server) handleAdminModelCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyAdmin(w, r) {
		return
	}
	entries, err := docker.SuggestionCatalog()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
