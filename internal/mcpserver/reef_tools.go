package mcpserver

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/reef"
)

// The reef surface: sharing memory with other members, as this member's bot.
//
// IT LIVES ON THIS SERVER RATHER THAN A SECOND ONE, and that is the whole
// delivery decision. The harness registers a remote server's tools under their
// own names and refuses its boot on a collision; it also refuses its boot on an
// UNREACHABLE server. A second configured server would therefore make every
// member's container unbootable whenever the reef was down -- a failure that
// arrives later, for one member, with no relation in time to its cause.
//
// The other half of the argument is that the token authenticating this endpoint
// is already an HMAC over tenant/subscription/agent/user, which is exactly the
// identity the reef needs in order to authorize. A second server would have to
// mint and carry a second credential, in plaintext, in a config file -- and
// header token indirection is specified and explicitly unbuilt.
//
// Names are prefixed `reef_` for the same reason `schedule_` is.

// boolean is the missing sibling of str/strArray/numArray above.
func boolean(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: desc}
}

// scopeTuple converts the verified workspace scope into the shape the reef
// takes. Project is deliberately dropped: the reef shares a MEMBER's memory,
// and a per-project split is a local concern of the graph.
func scopeTuple(sc memgraph.Scope) reef.Tuple {
	return reef.Tuple{
		TenantID:  sc.TenantID,
		SubsAccID: sc.SubsAccID,
		Role:      sc.Role,
		UserAccID: sc.UserAccID,
	}
}

type reefPublishIn struct {
	Type    string   `json:"type"`
	Cell    string   `json:"cell"`
	Content string   `json:"content"`
	To      []string `json:"to"`
}

type reefShareIn struct {
	ObjectID string `json:"objectId"`
	Target   string `json:"target"`
	Undo     bool   `json:"undo"`
}

type reefTimelineIn struct {
	Reading string `json:"reading"`
}

type reefReactIn struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Undo bool   `json:"undo"`
}

type reefAdmitIn struct {
	ActivityID string `json:"activityId"`
}

// registerReefTools adds the sharing surface, when this deployment has a reef.
//
// NOT REGISTERED AT ALL when the client is nil or unconfigured, rather than
// registered and refusing -- the same rule registerScheduleTools follows, and
// the same reason: a tool the model can see but never use is a tool it will
// keep trying, and every registered tool costs context on every turn whether
// or not it is ever called.
//
// This is also the whole of the feature's off switch. Unset the base URL or the
// token and nothing here exists: no tool name, no route, no actor provisioned.
func (s *server) registerReefTools(srv *mcp.Server) {
	if !s.reef.Enabled() {
		return
	}

	// EVERY HANDLER PASSES tenantLicensed=false, and that is not a placeholder.
	//
	// An agent arrives here with an MCP token that proves ONE subscription and
	// carries no mycelium licences. It therefore cannot prove it may address a
	// tenant, and the reef refuses tenant scope without that proof. The result
	// is deliberate and stricter than the specification asked for: AN AGENT
	// CANNOT BROADCAST TENANT-WIDE AT ALL. Tenant scope is a human action taken
	// in the webapp, where a real profile exists to resolve.
	const agentCannotProveTenant = false

	mcp.AddTool(srv, &mcp.Tool{
		Name: "reef_publish",
		Description: "Publish a memory to the reef, the shared network. " +
			"Omit `to` to keep it private to you. Address `reef:group:subscription:<id>` " +
			"to offer it to your subscription (a manager approves before it travels), " +
			"or a colleague's actor id to send it to them directly. " +
			"You cannot address a tenant; only a person can.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"type":    str("MemoryNote for a fact or note, MemoryFile for a document"),
			"cell":    str("What this memory is ABOUT -- the entity name or file path. Two authors may hold different claims about one cell and neither overwrites the other."),
			"content": str("The memory itself"),
			"to":      strArray("Who to address. Empty means private to you."),
		}, "type", "cell", "content"),
	}, tool(s, func(sc memgraph.Scope, in reefPublishIn) (any, error) {
		return s.reef.Publish(context.Background(), scopeTuple(sc), reef.AsService, agentCannotProveTenant,
			reef.Object{Type: in.Type, Cell: in.Cell, Content: in.Content}, in.To)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "reef_share",
		Description: "Share a memory you already published into a wider scope, " +
			"or withdraw it from one with undo. You cannot share beyond what you " +
			"can already reach.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"objectId": str("The reef object id to share"),
			"target":   str("The scope or actor to share into"),
			"undo":     boolean("Remove from the target instead of adding"),
		}, "objectId", "target"),
	}, tool(s, func(sc memgraph.Scope, in reefShareIn) (any, error) {
		return s.reef.Share(context.Background(), scopeTuple(sc), reef.AsService, agentCannotProveTenant,
			in.ObjectID, in.Target, in.Undo)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "reef_timeline",
		Description: "Read the reef. `received` is what others shared with you, " +
			"including items still held awaiting your person's admission; " +
			"`published` is what you shared; `pending` is what awaits a decision.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"reading": str("received, published or pending"),
		}, "reading"),
	}, tool(s, func(sc memgraph.Scope, in reefTimelineIn) (any, error) {
		return s.reef.Timeline(context.Background(), scopeTuple(sc), reef.AsService, in.Reading)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "reef_react",
		Description: "Respond to a memory. `read` confirms you took it INTO memory " +
			"(not merely that you saw it listed); `like` endorses it, which is weight " +
			"of evidence and never a claim that it is true; `flag` reports it to " +
			"whoever governs its scope.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"kind": str("read, like or flag"),
			"ref":  str("The object or activity id"),
			"undo": boolean("Withdraw this reaction instead of making it"),
		}, "kind", "ref"),
	}, tool(s, func(sc memgraph.Scope, in reefReactIn) (any, error) {
		return s.reef.React(context.Background(), scopeTuple(sc), reef.AsService, in.Kind, in.Ref, in.Undo)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "reef_admit",
		Description: "Take a memory somebody sent you directly into your own memory. " +
			"Until it is admitted it is visible to your person but is not yours.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"activityId": str("The held activity id, from reef_timeline's `held` list"),
		}, "activityId"),
	}, tool(s, func(sc memgraph.Scope, in reefAdmitIn) (any, error) {
		return s.reef.Admit(context.Background(), scopeTuple(sc), reef.AsService, in.ActivityID)
	}))
}
