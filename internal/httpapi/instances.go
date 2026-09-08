package httpapi

import (
	"crypto/subtle"
	"net/http"
)

// instancesResponse is the inventory envelope.
//
// A named field rather than a bare array so the shape can gain a sibling later
// without breaking a reader, and so an empty answer is `{"instances": []}` — not
// `null`, which a consumer cannot distinguish from a field it failed to decode.
type instancesResponse struct {
	Instances []instanceView `json:"instances"`
}

type instanceView struct {
	TenantID      string `json:"tenant_id"`
	SubsAccID     string `json:"subs_acc_id"`
	Agent         string `json:"agent"`
	UserAccID     string `json:"user_acc_id"`
	ContainerName string `json:"container_name"`
	Mode          string `json:"mode"`
	State         string `json:"state"`
}

// authorizeTelemetry checks the inventory credential.
//
// Constant-time: the value is a secret and this route is reachable by anything
// on the container network, so a timing oracle would be free to exploit. The
// route is not registered when the token is unset, so an empty configured token
// cannot reach here — the guard is kept anyway, because "unreachable" is a
// property of the caller and this function should not depend on it.
func (s *Server) authorizeTelemetry(r *http.Request) bool {
	want := s.Cfg.ResolvedTelemetryToken
	if want == "" {
		return false
	}
	got := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+want)) == 1
}

// handleInstances serves GET /v1/instances.
//
// It deliberately does NOT go through resolveAgent. That helper resolves ONE
// agent from x-mycelium-service-name and validates that agent's token, which
// answers a different question than this route asks: the inventory spans every
// agent, so no single agent's token is the right key for it.
//
// The stronger reason is what an agent token is worth. The mycelium profile
// header is decoded and never verified (identity.SDKResolver.Resolve), so the
// agent bearer check is what stops a caller who reached this proxy directly on
// the container network from asserting any accId it likes. Handing that token to
// a telemetry component would let a monitoring service send a message as any
// member of any tenant. A separate, read-only credential is the whole point.
func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeTelemetry(r) {
		writeJSON(w, http.StatusUnauthorized, errBody("invalid telemetry token"))
		return
	}
	instances, err := s.Mgr.Instances(r.Context())
	if err != nil {
		s.logf("instances: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not read the instance inventory"))
		return
	}
	out := make([]instanceView, 0, len(instances))
	for _, in := range instances {
		out = append(out, instanceView{
			TenantID:      in.TenantID,
			SubsAccID:     in.SubsAccID,
			Agent:         in.Agent,
			UserAccID:     in.UserAccID,
			ContainerName: in.ContainerName,
			Mode:          in.Mode,
			State:         string(in.State),
		})
	}
	writeJSON(w, http.StatusOK, instancesResponse{Instances: out})
}
