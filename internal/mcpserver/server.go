// Package mcpserver serves the per-workspace knowledge-graph memory over MCP, so
// a picoclaw instance gets a native memory server with no extra container in the
// environment.
//
// The transport is streamable HTTP from the official Go SDK — the same SDK, at the
// same version, that picoclaw's client is built from (context.md E-2), so
// compatibility is a property of the code rather than of careful reading. Verified
// against the real client before this package was written (E-9).
package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcptoken"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// Deps is everything the MCP endpoint needs.
type Deps struct {
	// Store owns the graphs. Required.
	Store *memgraph.Store
	// Secret signs and verifies the workspace tokens. Required — NewHandler with an
	// empty secret is a programming error, because the caller is supposed to skip
	// registering the route entirely (spec FR-4.5).
	Secret string
	// Logf is the proxy's logger. Optional.
	Logf func(string, ...any)
	// SourceFor answers "which conversation is this workspace mid-turn on?", so a
	// write can be attributed to the chat it came out of. Optional: nil means no
	// provenance is recorded, which is a degraded but correct state.
	//
	// It returns false whenever the answer is not unambiguous — no turn open (cron,
	// heartbeat, post-turn evolution) or more than one (concurrent conversations).
	// Attributing a guess would be worse than attributing nothing: the member clicks
	// through and reads a conversation that never said it.
	SourceFor func(memgraph.Scope) (string, bool)
}

// ServerName and ServerVersion identify this server in the MCP handshake. The name
// is deliberately not "memory" — the picoclaw side already calls the server
// "memory", and having both say it makes a log line ambiguous.
const (
	ServerName    = "crab-memory-graph"
	ServerVersion = "0.1.0"
)

// MaxRequestBytes bounds one JSON-RPC request body. Generous next to any real tool
// call (the largest is a create_entities batch) and small next to the graph's own
// 4 MiB ceiling, so a caller cannot use a single request to push a workspace over
// the limit either.
const MaxRequestBytes = 1 << 20

// errNoScope means the request carried no usable workspace token. It is returned
// from a tool handler, which the SDK turns into a tool error — but it should be
// unreachable, because the HTTP wrapper rejects such a request with 401 before the
// MCP layer ever sees it. It exists so a future refactor that loses the wrapper
// fails closed instead of serving somebody an empty graph.
var errNoScope = errors.New("no authorized workspace for this request")

type server struct {
	store     *memgraph.Store
	secret    string
	logf      func(string, ...any)
	sourceFor func(memgraph.Scope) (string, bool)
}

// source resolves the conversation to record on a write, or "" when it cannot be
// attributed. Empty is a normal outcome, not an error.
func (s *server) source(sc memgraph.Scope) string {
	if s.sourceFor == nil {
		return ""
	}
	if id, ok := s.sourceFor(sc); ok {
		return id
	}
	return ""
}

// NewHandler returns the http.Handler for /v1/mcp.
//
// Two things about the shape here are load-bearing:
//
// First, the bearer token is verified in a plain HTTP wrapper BEFORE the MCP
// handler runs, so a bad token is an ordinary 401 rather than a half-completed MCP
// handshake. The real client surfaces that as
// `calling "initialize": sending "initialize": Unauthorized`, which is a
// diagnosable failure (E-9).
//
// Second, there is ONE *mcp.Server, built once, shared by every request — not one
// per request with the scope captured in a closure. Each tool handler resolves its
// own scope from the request's own Authorization header (the SDK hands it over as
// RequestExtra.Header). That means the scope authorising a call is always the
// scope in the header of THAT call, never inherited connection state, and it avoids
// re-resolving fifteen JSON schemas on every request.
func NewHandler(d Deps) http.Handler {
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	s := &server{store: d.Store, secret: d.Secret, logf: d.Logf, sourceFor: d.SourceFor}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, &mcp.ServerOptions{
		// The tools are pure request/response over a stateless transport, so the
		// server never needs to push anything to the client.
		SchemaCache: mcp.NewSchemaCache(),
	})
	s.registerTools(srv)

	// Stateless: no session bookkeeping. The client opens no standalone SSE stream
	// (measured — E-9), so there is nothing for a session to hold.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cap the body BEFORE anything parses it. This route is the only one on the
		// proxy reachable by any container on the network without mycelium in front,
		// so an oversized body must be refused by us rather than absorbed by the
		// JSON-RPC decoder (NFR-1). MaxBytesReader makes the read fail, which the SDK
		// surfaces as a request error.
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		}
		if _, ok := s.scopeFromHeader(r.Header); !ok {
			// No detail in the body and no token in the log: this route is reachable
			// by anything on the container network, so it says as little as possible
			// about why it refused.
			s.logf("mcp: rejected %s %s from %s (no valid workspace token)",
				r.Method, r.URL.Path, r.RemoteAddr)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
}

// scopeFromHeader verifies the bearer token and returns the workspace it
// authorises. It is the ONLY way a scope enters this package: no tool takes a
// tenant, subscription, role or user parameter, so there is no path by which a
// caller can name a workspace other than the one its token carries.
func (s *server) scopeFromHeader(h http.Header) (memgraph.Scope, bool) {
	auth := h.Get("Authorization")
	// Case-insensitive scheme, exactly one space, as clients vary.
	const prefix = "bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return memgraph.Scope{}, false
	}
	return mcptoken.Verify(s.secret, strings.TrimSpace(auth[len(prefix):]))
}

// scope resolves the workspace for one tool call from that call's own headers.
func (s *server) scope(req *mcp.CallToolRequest) (memgraph.Scope, error) {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return memgraph.Scope{}, errNoScope
	}
	sc, ok := s.scopeFromHeader(req.Extra.Header)
	if !ok {
		return memgraph.Scope{}, errNoScope
	}
	return sc, nil
}

// tool is the shape every handler in tools.go has: resolve the scope from the
// call, then do one thing with it. Wrapping it here means no individual handler can
// forget the resolution step.
func tool[In any](s *server, fn func(memgraph.Scope, In) (any, error)) mcp.ToolHandlerFor[In, any] {
	return func(_ context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		sc, err := s.scope(req)
		if err != nil {
			return nil, nil, err
		}
		out, err := fn(sc, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	}
}
