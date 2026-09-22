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
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mangrove"
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
	// OwnsProject answers "does this member have a project by this id?".
	//
	// Required for the project header to be honoured at all; see projectFor.
	// Optional in the sense that nil is a valid configuration -- it simply means
	// no caller can select a project, and a call that tries is refused rather
	// than quietly served the member's global graph.
	OwnsProject func(memgraph.Scope, string) (bool, error)
	// OpenWorkspaceFile opens one of the member's own workspace files, so the
	// agent can put a document in a post instead of pasting it.
	//
	// OPTIONAL. Nil means mangrove_publish does not offer the `file` argument at
	// all -- absent rather than present-and-refusing, the same rule the schedule
	// tools follow: a tool argument the model can see but never use is one it
	// will keep trying.
	OpenWorkspaceFile func(memgraph.Scope, string) (io.ReadCloser, string, error)

	// Schedules lets an agent manage its own scheduled tasks.
	//
	// OPTIONAL, and nil is a real configuration: the three schedule tools are
	// then not registered at all. Absent rather than present-and-refusing, the
	// same rule the harness applies to its own tools -- a tool the model can see
	// but never use is a tool it will keep trying.
	//
	// The interface is here rather than a concrete type because the store, its
	// bounds and its provenance all live beside the member-facing write route,
	// and this package must not reach into that one.
	Schedules ScheduleStore
	// Mangrove lets an agent share memory with other members through
	// crab-mangrove-network.
	//
	// OPTIONAL, and nil -- or a client with no base URL or no token -- is a real
	// configuration: the mangrove tools are then not registered at all. Absent
	// rather than present-and-refusing, for the reason Schedules gives above,
	// and because this is the whole of an EXPERIMENTAL feature's off switch.
	Mangrove *mangrove.Client
	// ResolveAudience turns an `email:<address>` addressee into the actor id it
	// names, within the caller's own subscription.
	//
	// A FUNCTION rather than a client, for the reason Store and SourceFor are:
	// the lookup reads the workspace tree, which lives in the docker layer, and
	// this package must not reach into that one. Nil means the email form is
	// simply not resolved -- the gate then refuses it by name, which is the
	// correct failure rather than a silent drop.
	ResolveAudience func(memgraph.Scope, []string) ([]string, error)
}

// ScheduleStore is the scheduled-task surface an agent may reach.
//
// CREATE, LIST AND REMOVE — no edit. An edit is how a task a member approved
// becomes a different task without a second approval, so the shape of this
// interface is itself part of the control.
//
// Every method takes the scope the token carried. None takes a tenant,
// subscription, role or user argument, for the reason the package header gives:
// a caller cannot be allowed to name a workspace.
type ScheduleStore interface {
	// CreateSchedule files a new task. The implementation applies the bounds and
	// records who authored it; this package only carries the request.
	CreateSchedule(sc memgraph.Scope, in ScheduleInput) (any, error)
	// ListSchedules returns this workspace's tasks.
	ListSchedules(sc memgraph.Scope) (any, error)
	// DeleteSchedule removes one by id, refusing an id from another workspace.
	DeleteSchedule(sc memgraph.Scope, id string) (any, error)
}

// ScheduleInput is what the agent asks for. Deliberately narrower than the
// member-facing request body: no id, no enabled flag, no project.
type ScheduleInput struct {
	Name           string
	Message        string
	Kind           string
	Expr           string
	EveryMs        int64
	AtMs           int64
	TZ             string
	DeleteAfterRun bool
}

// ProjectHeader is how a caller says which of its own projects a call belongs to.
//
// Same name the proxy uses in the other direction (crab-shell-proxy sends
// X-Ganglion-Project to the harness on a turn), because it is the same fact: the
// project this work belongs to. One name is easier to grep for than two.
const ProjectHeader = "X-Ganglion-Project"

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
	store           *memgraph.Store
	secret          string
	logf            func(string, ...any)
	sourceFor       func(memgraph.Scope) (string, bool)
	ownsProject     func(memgraph.Scope, string) (bool, error)
	openFile        func(memgraph.Scope, string) (io.ReadCloser, string, error)
	schedules       ScheduleStore
	mangrove        *mangrove.Client
	resolveAudience func(memgraph.Scope, []string) ([]string, error)
}

// audience resolves `email:` entries when a resolver was supplied, and is the
// identity function when one was not.
func (s *server) audience(sc memgraph.Scope, to []string) ([]string, error) {
	if s.resolveAudience == nil {
		return to, nil
	}
	return s.resolveAudience(sc, to)
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
	s := &server{store: d.Store, secret: d.Secret, logf: d.Logf,
		sourceFor: d.SourceFor, ownsProject: d.OwnsProject, openFile: d.OpenWorkspaceFile,
		schedules: d.Schedules,
		mangrove:  d.Mangrove, resolveAudience: d.ResolveAudience}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, &mcp.ServerOptions{
		// The tools are pure request/response over a stateless transport, so the
		// server never needs to push anything to the client.
		SchemaCache: mcp.NewSchemaCache(),
	})
	s.registerTools(srv)
	s.registerMangroveTools(srv)

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
	return s.projectFor(sc, req.Extra.Header.Get(ProjectHeader))
}

// projectFor narrows a scope to one of the caller's own projects.
//
// THIS IS A DELIBERATE WIDENING of the rule scopeFromHeader states, and the
// limits matter more than the feature.
//
// The token still carries the whole WORKSPACE -- tenant, subscription, role,
// user -- and nothing here can change any of it. What the header may select is a
// project INSIDE that workspace, and only one the workspace actually has, which
// is why OwnsProject is consulted rather than trusted. So the boundary the token
// defends (one member's memory against another's) is untouched; what moves is a
// boundary between a member's own subjects, which memgraph.Scope already
// describes as context hygiene rather than security.
//
// It exists because the ganglion cannot express a project the way picoclaw does.
// There, each project is a separate AGENT with its own MCP server and its own
// project-scoped token, so the token alone says everything. The ganglion has ONE
// agent that takes the project per TURN, and giving it one server per project
// would collide by tool name and refuse the boot (AD-028). So the project
// travels per CALL.
//
// Three refusals, and each one exists because the alternative is a silent
// cross-project write -- which is the defect this function was written to fix,
// and which nobody notices until a project's memory is full of another's:
//
//   - a token that already names a project WINS over the header. That is
//     picoclaw's shape, where the token is the whole truth, and a header must
//     never be able to move a call out of the project its credential names.
//   - no OwnsProject wired: the header cannot be validated, so it is refused.
//     Serving the global graph instead would be the exact bug.
//   - a project the workspace does not have: refused, for the same reason
//     workspaceSegmentFor refuses one on every other route.
func (s *server) projectFor(sc memgraph.Scope, project string) (memgraph.Scope, error) {
	project = strings.TrimSpace(project)
	if project == "" || sc.Project != "" {
		return sc, nil
	}
	if s.ownsProject == nil {
		s.logf("mcp: refusing a project-scoped call: no project resolver is wired")
		return memgraph.Scope{}, errNoScope
	}
	ok, err := s.ownsProject(sc, project)
	if err != nil {
		s.logf("mcp: project lookup failed for %s: %v", project, err)
		return memgraph.Scope{}, errNoScope
	}
	if !ok {
		s.logf("mcp: refusing a call naming project %q, which this workspace does not have", project)
		return memgraph.Scope{}, errNoScope
	}
	sc.Project = project
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
