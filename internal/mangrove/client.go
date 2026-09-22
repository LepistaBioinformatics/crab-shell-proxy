// Package mangrove is this proxy's client for crab-mangrove-network, the EXPERIMENTAL
// federated memory network agents share through.
//
// THE PROXY IS THE ONLY CALLER THE MANGROVE HAS, and that is what makes the whole
// arrangement work. Every request carries a workspace tuple this proxy has
// ALREADY verified -- from the MCP bearer token for an agent, or from the
// mycelium profile for a human -- so the mangrove performs no authentication of its
// own beyond proving that the caller is us.
//
// Two fields in every request body are ours alone to set, and getting either
// wrong is a privilege escalation rather than a bug:
//
//	As              "service" when an agent is acting inside a turn, "person"
//	                when a human is acting in the webapp. It selects which of
//	                the workspace's two actors signs.
//	TenantLicensed  set ONLY when the caller arrived with a real mycelium
//	                profile that licenses the tenant. An agent's MCP token
//	                proves one subscription and cannot prove this, so the MCP
//	                path never sets it -- which is what stops an agent
//	                broadcasting tenant-wide.
package mangrove

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Tuple is the verified workspace identity. It mirrors docker.WorkspaceKey and
// the mangrove's own actor.Tuple; it is redeclared here rather than imported so the
// wire shape is visible at the boundary that produces it.
type Tuple struct {
	TenantID  string `json:"tenantId"`
	SubsAccID string `json:"subsAccId"`
	Role      string `json:"role"`
	UserAccID string `json:"userAccId"`
}

// Actor selects which of a workspace's two actors acts.
type Actor string

const (
	// AsService is an agent acting inside a turn.
	AsService Actor = "service"
	// AsPerson is a human acting in the webapp.
	AsPerson Actor = "person"
)

// Client talks to the mangrove. A zero BaseURL or Token means the feature is off,
// and Enabled reports that -- callers use it to decide whether to REGISTER the
// tools at all, rather than to register tools that refuse.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		// Bounded so a hung mangrove cannot hold a turn open. The mangrove does no
		// remote work of its own on these paths except, for an audience naming
		// a specific colleague, one call back to this proxy.
		HTTP: &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled reports whether the mangrove is configured at all.
//
// BOTH halves are required. A base URL with no token would reach a service that
// refuses every request, and a token with no base URL reaches nothing; either
// alone is a misconfiguration, and treating it as "on" would register tools
// that can only fail.
func (c *Client) Enabled() bool {
	return c != nil && c.BaseURL != "" && c.Token != ""
}

// Error is a non-2xx answer from the mangrove, carrying the body so a refusal keeps
// the addressee it named. FR-B6a requires a refusal to say WHICH addressee was
// out of reach; swallowing the body here would lose exactly that.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("mangrove: %s", e.Body)
	}
	return fmt.Sprintf("mangrove: answered %d", e.Status)
}

func (c *Client) post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("mangrove: not configured")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The mangrove being unreachable must NOT fail a turn or a boot -- it
		// returns something the model can read. The container boundary for
		// optionality is registration, not request time.
		return nil, fmt.Errorf("mangrove: unreachable: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("mangrove: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	return json.RawMessage(raw), nil
}

// base is the part of every request that describes the caller rather than the
// operation.
type base struct {
	Tuple          Tuple `json:"tuple"`
	As             Actor `json:"as"`
	TenantLicensed bool  `json:"tenantLicensed,omitempty"`
}

// Object is one unit of shared memory.
type Object struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type"`
	Cell      string `json:"cell"`
	Content   string `json:"content,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
}

type publishReq struct {
	base
	Object Object   `json:"object"`
	To     []string `json:"to"`
}

// Publish creates a memory object. `to` empty means private to the author,
// which is the default a bare publish carries.
func (c *Client) Publish(ctx context.Context, t Tuple, as Actor, tenantLicensed bool, obj Object, to []string) (json.RawMessage, error) {
	return c.post(ctx, "/internal/v1/publish", publishReq{
		base:   base{Tuple: t, As: as, TenantLicensed: tenantLicensed},
		Object: obj,
		To:     to,
	})
}

type shareReq struct {
	base
	ObjectID string `json:"objectId"`
	Target   string `json:"target"`
	Undo     bool   `json:"undo"`
}

// Share adds an existing object to a wider scope, or removes it with undo.
func (c *Client) Share(ctx context.Context, t Tuple, as Actor, tenantLicensed bool, objectID, target string, undo bool) (json.RawMessage, error) {
	return c.post(ctx, "/internal/v1/share", shareReq{
		base:     base{Tuple: t, As: as, TenantLicensed: tenantLicensed},
		ObjectID: objectID, Target: target, Undo: undo,
	})
}

type reactReq struct {
	base
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Undo bool   `json:"undo"`
}

// React emits a Read receipt, a Like endorsement, or a Flag report -- or undoes
// one. These are three different standard verbs, not three flavours of the
// same one: Read says the object was taken into memory, Like is weight of
// evidence, Flag is a report to whoever governs the scope.
func (c *Client) React(ctx context.Context, t Tuple, as Actor, kind, ref string, undo bool) (json.RawMessage, error) {
	return c.post(ctx, "/internal/v1/react", reactReq{
		base: base{Tuple: t, As: as},
		Kind: kind, Ref: ref, Undo: undo,
	})
}

type admitReq struct {
	base
	ActivityID string `json:"activityId"`
}

// Admit lets a held object into this workspace's own memory.
func (c *Client) Admit(ctx context.Context, t Tuple, as Actor, activityID string) (json.RawMessage, error) {
	return c.post(ctx, "/internal/v1/admit", admitReq{
		base:       base{Tuple: t, As: as},
		ActivityID: activityID,
	})
}

type timelineReq struct {
	base
	Reading string `json:"reading"`
}

// Timeline reads one of "received", "published" or "pending".
func (c *Client) Timeline(ctx context.Context, t Tuple, as Actor, reading string) (json.RawMessage, error) {
	return c.post(ctx, "/internal/v1/timeline", timelineReq{
		base:    base{Tuple: t, As: as},
		Reading: reading,
	})
}

type decideReq struct {
	base
	ActivityID string `json:"activityId"`
	Accept     bool   `json:"accept"`
	Governs    bool   `json:"governs"`
}

// Decide accepts or rejects a pending cross-scope publication.
//
// HUMANS ONLY, and there is no MCP tool for it. Governs is the caller's
// mycelium role resolved by this proxy -- the mangrove does not read roles, because
// it is not the component that can. An agent has no path to this call.
func (c *Client) Decide(ctx context.Context, t Tuple, activityID string, accept, governs bool) (json.RawMessage, error) {
	return c.post(ctx, "/internal/v1/decide", decideReq{
		base:       base{Tuple: t, As: AsPerson},
		ActivityID: activityID, Accept: accept, Governs: governs,
	})
}

type revokeReq struct {
	base
	ObjectID string `json:"objectId"`
	Cell     string `json:"cell"`
}

// Revoke tombstones an object the caller's own agent authored.
//
// HUMANS ONLY, and the mangrove enforces that independently: it refuses a revoke
// signed by the Service actor. Authority runs one way, and it is checked on
// both sides rather than trusted from here.
func (c *Client) Revoke(ctx context.Context, t Tuple, objectID, cell string) (json.RawMessage, error) {
	return c.post(ctx, "/internal/v1/revoke", revokeReq{
		base:     base{Tuple: t, As: AsPerson},
		ObjectID: objectID, Cell: cell,
	})
}
