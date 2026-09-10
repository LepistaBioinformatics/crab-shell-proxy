package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
)

// Per-instance lifecycle mode administration.
//
// `agents.<key>.mode` in config.yaml is the DEFAULT for every instance of that
// agent; this endpoint sets the exception for one of them.
//
// The reason it exists is scheduled tasks. They run from timers inside the
// container, so a scale-to-zero instance fires none -- which today forces a
// whole agent to choose between costing a permanently running container per
// member and having no working schedules at all. One member who needs a daily
// report is a reason to keep ONE container up.
//
// Gated exactly like the sibling instance-config editor: user-management
// authority, an explicit `agent` parameter rather than the addressed agent, and
// no path or name parameter of any kind.

type instanceModeView struct {
	// Effective is what the instance actually runs as, and the only field a
	// caller should branch on.
	Effective config.Mode `json:"effective"`
	// Override is the value pinned on this instance, empty when it follows its
	// agent. Reported separately because "continuous, inherited" and
	// "continuous, pinned here" are the same today and differ the moment the
	// agent default moves -- an admin choosing between them needs to see which
	// they have.
	Override config.Mode `json:"override"`
	// AgentDefault is what clearing the override would leave.
	AgentDefault config.Mode `json:"agentDefault"`
	// ScaleToZeroAllowed is false when the agent declares no idleTimeout, which
	// makes scale-to-zero unrepresentable for its instances. Reported so the UI
	// can disable the choice instead of offering one the write will refuse.
	ScaleToZeroAllowed bool `json:"scaleToZeroAllowed"`
	// Fires mirrors what the member sees on their scheduled-tasks panel, so an
	// admin acting on a report of "my tasks do not run" is looking at the same
	// fact the member is.
	Fires bool `json:"fires"`
}

func (s *Server) handleAdminInstanceModeGet(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	key, ok := s.adminInstanceKey(w, r, ident)
	if !ok {
		return
	}
	agent, ok := s.Cfg.Agents[key.Role]
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("no such agent"))
		return
	}
	writeJSON(w, http.StatusOK, s.instanceModeView(agent, key))
}

type instanceModeRequest struct {
	// Mode is "continuous", "scale-to-zero", or "" to clear the override and
	// follow the agent again.
	Mode config.Mode `json:"mode"`
}

// handleAdminInstanceModePut sets or clears one instance's override.
//
// The write and the idle-timer transition happen together inside
// Manager.SetMode. Doing them separately is how this feature would fail
// silently: a container switched to scale-to-zero with nothing arming its timer
// runs forever, and one switched to continuous with a timer still armed is
// stopped once more after the admin was told it was done.
func (s *Server) handleAdminInstanceModePut(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	key, ok := s.adminInstanceKey(w, r, ident)
	if !ok {
		return
	}
	agent, ok := s.Cfg.Agents[key.Role]
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("no such agent"))
		return
	}

	var req instanceModeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("body must be a JSON object with a \"mode\" field"))
		return
	}
	if err := s.Mgr.SetMode(agent, key, req.Mode); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	s.logf("admin %s set instance %s/%s/%s/%s mode to %q",
		ident.AccID, key.TenantID, key.SubsAccID, key.Role, key.UserAccID, req.Mode)

	// The fresh view, not an acknowledgement: the caller needs the EFFECTIVE
	// mode after a clear, which is the agent default and not what they sent.
	writeJSON(w, http.StatusOK, s.instanceModeView(agent, key))
}

func (s *Server) instanceModeView(agent config.Agent, key docker.WorkspaceKey) instanceModeView {
	effective := s.Mgr.ModeFor(agent, key)
	return instanceModeView{
		Effective:          effective,
		Override:           s.Mgr.ModeOverride(key),
		AgentDefault:       agent.Mode,
		ScaleToZeroAllowed: agent.IdleTimeout.Std() > 0,
		Fires:              effective == config.ModeContinuous,
	}
}
