package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcptoken"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// The approvals endpoint: the answering half of the ganglion's approver port.
//
// The harness has asked permission since its approver adapter shipped -- every
// tool call goes through Loop.runTool, which calls the endpoint named by
// GANGLION_APPROVAL_ENDPOINT. Nothing answered it, so the variable was unset
// everywhere and Client.Request short-circuited "not gated" before it built a
// body. This is the answer.
//
// DC-1 IS THE WHOLE DESIGN CONSTRAINT (ganglion-agent-confinement spec):
//
//	The approvals endpoint must not treat a harness-presented bearer as
//	authorization for a decision. The harness presents IDENTITY; the proxy
//	decides from its own state and mycelium's account id. A token the agent can
//	read is not an authorization token.
//
// So the container's credential establishes WHICH WORKSPACE IS ASKING and
// nothing else. The decision comes from a member, authenticated the way every
// other member-facing route authenticates them, and the answer is recorded
// against the account id mycelium vouched for.
//
// WHY THE SCOPE TRAVELS IN THE URL. The shipped harness sends GANGLION_TOKEN as
// the bearer -- a per-user random string the proxy minted, which identifies a
// container but tells the proxy nothing about which workspace it belongs to
// without an index the proxy does not keep. So the proxy hands it an endpoint
// whose query already carries a scoped, MAC'd token: the same mcptoken the MCP
// route verifies, minted for the same workspace. Verifying it yields the scope
// with no lookup and no trust in anything the container chose. The endpoint URL
// lives in the container's environment, which the shell tool's env scrub hides
// and /proc denies -- the same protection GANGLION_API_KEY has.
//
// A denial is not a failure. The loop turns Allowed:false into a Result the
// model reads and can explain (DEC-2), and a timeout denies (DEC-4). Every path
// here that cannot produce a member's explicit allow produces a refusal.

// pendingApproval is one blocked tool call waiting on a person.
type pendingApproval struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionId"`
	ToolCallID string          `json:"toolCallId"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	Requested  time.Time       `json:"requestedAt"`

	scope  memgraph.Scope
	answer chan approvalDecision
}

// approvalDecision is the wire shape the harness decodes. Field names are fixed
// by approver/proxy's wireDecision -- this is not ours to rename.
type approvalDecision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	By      string `json:"by"`
}

// approvalRequest is the body the harness sends. Also not ours to rename.
type approvalRequest struct {
	SessionKey string          `json:"session_key"`
	SessionID  string          `json:"session_id"`
	ToolCallID string          `json:"tool_call_id"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
}

// approvalStore holds what is waiting, per workspace.
//
// In memory, deliberately. A pending request is a turn blocked on a socket; if
// the proxy restarts, that turn is gone and so is anything it was waiting for.
// Persisting them would resurrect decisions with nothing left to decide.
type approvalStore struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval // id -> request
	// grants records that a member allowed a tool, so the ROUTE that tool calls
	// can require the approval rather than trust that it happened.
	//
	// WHY THIS EXISTS. Gating is configured by an environment variable on the
	// container: GANGLION_GATED_TOOLS. A variable is a thing that can be wrong —
	// unset in one deployment, dropped in a refactor, absent on an older image —
	// and when it is wrong the tool simply runs, unasked. AD-025 D-2 is the
	// standing answer to that shape: a gate with no enforcer is a sentence that
	// changes nothing. So the enforcer is here, on the side that cannot be
	// reconfigured from inside the container.
	grants map[grantKey]grant
	now    func() time.Time
}

// grantKey is one workspace's permission to run one tool.
type grantKey struct {
	scope memgraph.Scope
	tool  string
}

type grant struct {
	by      string
	expires time.Time
}

// grantTTL bounds how long an allow stays spendable.
//
// The harness invokes the tool the instant the decision returns, so this only
// has to cover that hop. Short, because a grant that outlived its turn would let
// a LATER call — including one from a scheduled run nobody is watching — spend an
// approval a member gave for something else.
const grantTTL = 2 * time.Minute

func newApprovalStore() *approvalStore {
	return &approvalStore{
		pending: map[string]*pendingApproval{},
		grants:  map[grantKey]grant{},
		now:     time.Now,
	}
}

// consumeGrant spends a member's allow for one tool, if one is live.
//
// ONE-SHOT: the entry is removed whether or not it had expired, so an approval
// buys exactly one call. Two scheduled tasks need two answers.
//
// The expiry comes back so a caller that refuses the request AFTER consuming can
// hand the grant back unchanged — see restoreGrant.
func (a *approvalStore) consumeGrant(scope memgraph.Scope, tool string) (string, time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := grantKey{scope: scope, tool: tool}
	g, ok := a.grants[k]
	delete(a.grants, k)
	if !ok || a.now().After(g.expires) {
		return "", time.Time{}, false
	}
	return g.by, g.expires, true
}

// restoreGrant puts back an allow that was consumed for a call the proxy then
// refused for its own reasons.
//
// WHY THIS EXISTS. The grant is checked before anything else on purpose: the
// shape of a refusal must not tell an unapproved caller which of its arguments
// the proxy liked. But that ordering means a request refused by a LIMIT — an
// interval that is too short, a workspace already at its ceiling — would spend
// the member's answer on a call that never happened, and the agent would have to
// ask again for a mistake it can see and fix itself.
//
// THE ORIGINAL EXPIRY IS KEPT, never extended. Otherwise a caller could hold an
// approval open indefinitely by failing on purpose, which would turn a two-minute
// window into however long it cares to keep trying.
func (a *approvalStore) restoreGrant(scope memgraph.Scope, tool, by string, expires time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grants[grantKey{scope: scope, tool: tool}] = grant{by: by, expires: expires}
}

func (a *approvalStore) recordGrant(scope memgraph.Scope, tool, by string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grants[grantKey{scope: scope, tool: tool}] = grant{by: by, expires: a.now().Add(grantTTL)}
}

func (a *approvalStore) add(p *pendingApproval) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending[p.ID] = p
}

func (a *approvalStore) remove(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pending, id)
}

// list returns what this workspace is waiting on, oldest first so a member
// answering several works through them in the order they blocked.
func (a *approvalStore) list(scope memgraph.Scope) []pendingApproval {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []pendingApproval{}
	for _, p := range a.pending {
		if p.scope == scope {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requested.Before(out[j].Requested) })
	return out
}

// answer resolves one pending request, but only for the workspace it belongs to.
//
// The scope check is not belt-and-braces: the id is the only thing the answering
// request names, and without it a member who learned another member's id could
// answer on their behalf. Returns false when the id is unknown OR belongs to
// someone else -- the same answer for both, so the route cannot be used to test
// whether an id exists.
func (a *approvalStore) answer(id string, scope memgraph.Scope, dec approvalDecision) bool {
	a.mu.Lock()
	p, ok := a.pending[id]
	if !ok || p.scope != scope {
		a.mu.Unlock()
		return false
	}
	delete(a.pending, id)
	a.mu.Unlock()

	// Buffered at 1 and the waiter may already be gone (its deadline passed, or
	// the container died), so this never blocks.
	select {
	case p.answer <- dec:
	default:
	}
	return true
}

// handleApprovalRequest is the container's side: register, block, answer.
func (s *Server) handleApprovalRequest(w http.ResponseWriter, r *http.Request) {
	secret := s.Cfg.ResolvedMCPTokenSecret
	scope, ok := mcptoken.Verify(secret, r.URL.Query().Get(mcptoken.QueryParam))
	if !ok {
		// No body and no token in the log, the posture /v1/mcp takes: this route
		// is reachable by any container on the network.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req approvalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid approval request body"))
		return
	}
	if req.Tool == "" {
		writeJSON(w, http.StatusBadRequest, errBody("\"tool\" is required"))
		return
	}

	p := &pendingApproval{
		ID:         newApprovalID(),
		SessionID:  req.SessionID,
		ToolCallID: req.ToolCallID,
		Tool:       req.Tool,
		Arguments:  req.Arguments,
		Requested:  s.Approvals.now(),
		scope:      scope,
		answer:     make(chan approvalDecision, 1),
	}
	s.Approvals.add(p)

	select {
	case dec := <-p.answer:
		if dec.Allowed {
			s.Approvals.recordGrant(scope, p.Tool, dec.By)
		}
		s.recordApproval(scope, p, dec)
		writeJSON(w, http.StatusOK, dec)
	case <-r.Context().Done():
		// The harness gave up, or the container went away. Drop the request
		// rather than leaving a row a member could still answer into nothing.
		//
		// NOT an allow, and not a 500 either: the harness turns a transport
		// failure into a denial with an opaque reason, and a reason nobody can
		// act on is worse than the true one.
		s.Approvals.remove(p.ID)
	}
}

// handleApprovalsPending is the member's side: what is waiting on me.
func (s *Server) handleApprovalsPending(w http.ResponseWriter, r *http.Request) {
	key, ok := s.restartCallerKey(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pending": s.Approvals.list(scopeOfKey(key)),
	})
}

// handleApprovalsAnswer is the member's side: allow or refuse one.
func (s *Server) handleApprovalsAnswer(w http.ResponseWriter, r *http.Request) {
	key, ok := s.restartCallerKey(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ID      string `json:"id"`
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("\"id\" is required"))
		return
	}

	// BY IS THE PROXY'S, never the request's. It is the account id mycelium
	// vouched for on this call, which is the only fact here the container did
	// not influence.
	dec := approvalDecision{Allowed: body.Allowed, Reason: body.Reason, By: key.UserAccID}
	if dec.Reason == "" && !dec.Allowed {
		dec.Reason = "refused by the member"
	}
	if !s.Approvals.answer(body.ID, scopeOfKey(key), dec) {
		writeJSON(w, http.StatusNotFound, errBody("no approval request is waiting with that id"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "answered"})
}

// recordApproval appends the answered request to the audit file. Best effort:
// losing the record must not turn an allowed call into a refused one, because
// the member already answered and the turn is waiting on that answer.
func (s *Server) recordApproval(scope memgraph.Scope, p *pendingApproval, dec approvalDecision) {
	path := config.ApprovalsFile(s.Cfg.ContainerDataRoot,
		scope.TenantID, scope.SubsAccID, scope.Role, scope.UserAccID)
	line, err := json.Marshal(map[string]any{
		"answeredAt": s.Approvals.now().UTC().Format(time.RFC3339),
		"sessionId":  p.SessionID,
		"toolCallId": p.ToolCallID,
		"tool":       p.Tool,
		"arguments":  p.Arguments,
		"allowed":    dec.Allowed,
		"reason":     dec.Reason,
		"by":         dec.By,
		"project":    scope.Project,
	})
	if err != nil {
		return
	}
	// The user directory exists in production -- the workspace was provisioned
	// before any turn could run in it -- but this is the first thing to write
	// here on a workspace that has never been approved into, so do not assume.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// scopeOfKey turns the member's authorized workspace key into the scope the
// container's token carries, so the two can be compared.
//
// NO PROJECT, and that is the fix for a bug this shipped with.
//
// An approval is about a WORKSPACE and the member who owns it. The container
// reaches this endpoint with a token minted once, when the container was
// created, and that token cannot carry a project -- one container serves every
// project the member has. So a request always registers with an empty project.
//
// The member's side, though, asks from wherever they are chatting, and inside a
// project that query carries `project=<id>`. Keying the lookup on the narrowed
// scope meant the two never matched: the request was registered under "" and
// looked up under the project, the list came back empty, and the card never
// appeared while the turn sat blocked. The member saw "waiting for approval"
// forever and had nothing to answer.
//
// Widening this does not widen who may answer: tenant, subscription, role and
// user all still come from the authorization chain, and the project was never
// part of what the token proves.
func scopeOfKey(key docker.WorkspaceKey) memgraph.Scope {
	return memgraph.Scope{
		TenantID:  key.TenantID,
		SubsAccID: key.SubsAccID,
		Role:      key.Role,
		UserAccID: key.UserAccID,
	}
}

func newApprovalID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Time is not a secret, and an id only has to be unique within the
		// process. A collision here would let one answer resolve another
		// member's request, which the scope check already refuses.
		return fmt.Sprintf("a%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
