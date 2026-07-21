package httpapi

import (
	_ "embed"
	"net/http"
)

// openapiDoc is the service's OpenAPI 3.0 document, served at /doc/openapi.json
// so the mycelium gateway can discover this agent as a tool (a service with
// discoverable=true + openapiPath fetches this directly, unauthenticated).
//
//go:embed openapi.json
var openapiDoc []byte

// handleOpenAPI serves the OpenAPI document. It is UNAUTHENTICATED, like
// /healthz: mycelium's discovery issues a plain GET straight to the service host
// (<protocol>://<host>/<openapiPath>), not through the authenticated gateway
// routes, so requiring a token here would make the service undiscoverable.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(openapiDoc)
}
