package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mangrove"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// The mangrove surface: sharing memory with other members, as this member's bot.
//
// IT LIVES ON THIS SERVER RATHER THAN A SECOND ONE, and that is the whole
// delivery decision. The harness registers a remote server's tools under their
// own names and refuses its boot on a collision; it also refuses its boot on an
// UNREACHABLE server. A second configured server would therefore make every
// member's container unbootable whenever the mangrove was down -- a failure that
// arrives later, for one member, with no relation in time to its cause.
//
// The other half of the argument is that the token authenticating this endpoint
// is already an HMAC over tenant/subscription/agent/user, which is exactly the
// identity the mangrove needs in order to authorize. A second server would have to
// mint and carry a second credential, in plaintext, in a config file -- and
// header token indirection is specified and explicitly unbuilt.
//
// Names are prefixed `mangrove_` for the same reason `schedule_` is.

// boolean is the missing sibling of str/strArray/numArray above.
func boolean(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: desc}
}

// scopeTuple converts the verified workspace scope into the shape the mangrove
// takes. Project is deliberately dropped: the mangrove shares a MEMBER's memory,
// and a per-project split is a local concern of the graph.
func scopeTuple(sc memgraph.Scope) mangrove.Tuple {
	return mangrove.Tuple{
		TenantID:  sc.TenantID,
		SubsAccID: sc.SubsAccID,
		Role:      sc.Role,
		UserAccID: sc.UserAccID,
	}
}

type mangrovePublishIn struct {
	Type      string   `json:"type"`
	Cell      string   `json:"cell"`
	Content   string   `json:"content"`
	MediaType string   `json:"mediaType"`
	To        []string `json:"to"`

	// Entities publishes a piece of the agent's own memory graph -- those
	// entities AND the relations among them.
	Entities []string `json:"entities"`
	// File publishes one of the member's workspace files by its path.
	File string `json:"file"`
}

type mangroveShareIn struct {
	ObjectID string `json:"objectId"`
	Target   string `json:"target"`
	Undo     bool   `json:"undo"`
}

type mangroveTimelineIn struct {
	Reading string `json:"reading"`
}

type mangroveReactIn struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Undo bool   `json:"undo"`
}

type mangroveAdmitIn struct {
	ActivityID string `json:"activityId"`
}

// registerMangroveTools adds the sharing surface, when this deployment has a mangrove.
//
// NOT REGISTERED AT ALL when the client is nil or unconfigured, rather than
// registered and refusing -- the same rule registerScheduleTools follows, and
// the same reason: a tool the model can see but never use is a tool it will
// keep trying, and every registered tool costs context on every turn whether
// or not it is ever called.
//
// This is also the whole of the feature's off switch. Unset the base URL or the
// token and nothing here exists: no tool name, no route, no actor provisioned.
func (s *server) registerMangroveTools(srv *mcp.Server) {
	if !s.mangrove.Enabled() {
		return
	}

	// EVERY HANDLER PASSES tenantLicensed=false, and that is not a placeholder.
	//
	// An agent arrives here with an MCP token that proves ONE subscription and
	// carries no mycelium licences. It therefore cannot prove it may address a
	// tenant, and the mangrove refuses tenant scope without that proof. The result
	// is deliberate and stricter than the specification asked for: AN AGENT
	// CANNOT ADDRESS A GROUP AT ALL. Its MCP token proves MEMBERSHIP of one
	// subscription; addressing a group is a question about governance, which no
	// agent token can answer. Group scope is a human action taken in the
	// webapp, where a real profile exists to resolve.
	//
	// The zero value would do this on its own. It is named and passed
	// explicitly so that a reader of these two call sites sees the stance
	// rather than an omission.
	var agentHoldsNoLicences mangrove.Licences

	mcp.AddTool(srv, &mcp.Tool{
		Name: "mangrove_publish",
		Description: "Publish a memory to the mangrove, the shared network. " +
			"Omit `to` to keep it private to you. Address a colleague's actor id " +
			"-- or `email:<address>` -- to send it to them directly; they admit it " +
			"before it reaches their agent. " +
			"You cannot address a group, neither a subscription nor a tenant: " +
			"broadcasting to a whole scope is a human action, taken in the web UI " +
			"by somebody who governs it.",
		InputSchema: s.publishSchema(),
	}, tool(s, func(sc memgraph.Scope, in mangrovePublishIn) (any, error) {
		to, err := s.audience(sc, in.To)
		if err != nil {
			return nil, err
		}
		obj, err := s.publishObject(sc, in)
		if err != nil {
			return nil, err
		}
		return s.mangrove.Publish(context.Background(), scopeTuple(sc), mangrove.AsService,
			agentHoldsNoLicences, obj, to)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "mangrove_share",
		Description: "Share a memory you already published with somebody else, " +
			"or withdraw it from them with undo. You cannot share beyond what you " +
			"can already reach, and you cannot share into a group.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"objectId": str("The mangrove object id to share"),
			"target":   str("The scope or actor to share into"),
			"undo":     boolean("Remove from the target instead of adding"),
		}, "objectId", "target"),
	}, tool(s, func(sc memgraph.Scope, in mangroveShareIn) (any, error) {
		target := in.Target
		if resolved, err := s.audience(sc, []string{target}); err == nil && len(resolved) == 1 {
			target = resolved[0]
		}
		return s.mangrove.Share(context.Background(), scopeTuple(sc), mangrove.AsService, agentHoldsNoLicences,
			in.ObjectID, target, in.Undo)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "mangrove_timeline",
		Description: "Read the mangrove. `received` is what others shared with you, " +
			"including items still held awaiting your person's admission; " +
			"`published` is what you shared; `pending` is what awaits a decision.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"reading": str("received, published or pending"),
		}, "reading"),
	}, tool(s, func(sc memgraph.Scope, in mangroveTimelineIn) (any, error) {
		return s.mangrove.Timeline(context.Background(), scopeTuple(sc), mangrove.AsService, in.Reading)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "mangrove_react",
		Description: "Respond to a memory. `read` confirms you took it INTO memory " +
			"(not merely that you saw it listed); `like` endorses it, which is weight " +
			"of evidence and never a claim that it is true; `flag` reports it to " +
			"whoever governs its scope.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"kind": str("read, like or flag"),
			"ref":  str("The object or activity id"),
			"undo": boolean("Withdraw this reaction instead of making it"),
		}, "kind", "ref"),
	}, tool(s, func(sc memgraph.Scope, in mangroveReactIn) (any, error) {
		return s.mangrove.React(context.Background(), scopeTuple(sc), mangrove.AsService, in.Kind, in.Ref, in.Undo)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "mangrove_admit",
		Description: "Take a memory somebody sent you directly into your own memory. " +
			"Until it is admitted it is visible to your person but is not yours.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"activityId": str("The held activity id, from mangrove_timeline's `held` list"),
		}, "activityId"),
	}, tool(s, func(sc memgraph.Scope, in mangroveAdmitIn) (any, error) {
		return s.mangrove.Admit(context.Background(), scopeTuple(sc), mangrove.AsService, in.ActivityID)
	}))
}

// publishSchema offers `file` only where the proxy wired a way to read one.
// Absent rather than present-and-refusing: an argument the model can see but
// never use is one it will keep trying.
func (s *server) publishSchema() *jsonschema.Schema {
	props := map[string]*jsonschema.Schema{
		"type":      str("MemoryNote for a fact or note, MemoryFile for a document"),
		"cell":      str("What this memory is ABOUT -- the entity name. Two authors may hold different claims about one cell and neither overwrites the other. Ignored when you publish entities or a file, which derive it."),
		"content":   str("The memory itself, as prose"),
		"mediaType": str("The format of `content`: text/markdown for prose, text/plain otherwise. Say it rather than letting the reader guess."),
		"entities":  strArray("Instead of `content`: names of entities in YOUR memory graph to share. The relations among them travel too."),
		"to":        strArray("Who to address. Empty means private to you."),
	}
	if s.openFile != nil {
		props["file"] = str("Instead of `content`: the path of one of your workspace files, like public/report.pdf")
	}
	return object(props, "type")
}

// publishObject turns the three shapes into one object. EXACTLY ONE, because
// `cell` is the reduction key and each shape derives it differently -- two at
// once would leave a reader asking which part is the content.
func (s *server) publishObject(sc memgraph.Scope, in mangrovePublishIn) (mangrove.Object, error) {
	kinds := 0
	if strings.TrimSpace(in.Content) != "" {
		kinds++
	}
	if len(in.Entities) > 0 {
		kinds++
	}
	if strings.TrimSpace(in.File) != "" {
		kinds++
	}
	if kinds != 1 {
		return mangrove.Object{}, errors.New("publish exactly one of content, entities or file")
	}

	switch {
	case len(in.Entities) > 0:
		g, err := s.store.OpenNodes(sc, in.Entities)
		if err != nil {
			return mangrove.Object{}, err
		}
		if len(g.Entities) == 0 {
			return mangrove.Object{}, errors.New("none of those entities are in your memory graph")
		}
		body, err := json.Marshal(map[string]any{"entities": g.Entities, "relations": g.Relations})
		if err != nil {
			return mangrove.Object{}, err
		}
		return mangrove.Object{
			Type:      "MemoryNote",
			Cell:      mangroveFragmentCell(g.Entities),
			Content:   string(body),
			MediaType: mangrove.GraphFragmentMediaType,
		}, nil

	case strings.TrimSpace(in.File) != "":
		if s.openFile == nil {
			return mangrove.Object{}, errors.New("this deployment cannot publish workspace files")
		}
		rc, display, err := s.openFile(sc, in.File)
		if err != nil {
			return mangrove.Object{}, err
		}
		defer rc.Close()
		digest, size, err := s.mangrove.PutBlob(context.Background(), rc)
		if err != nil {
			return mangrove.Object{}, err
		}
		return mangrove.Object{
			Type: "MemoryFile", Cell: in.File, Blob: digest, FileName: display, Size: size,
		}, nil

	default:
		cell := strings.TrimSpace(in.Cell)
		if cell == "" {
			return mangrove.Object{}, errors.New("cell is required: it is what the reduction is keyed by")
		}
		typ := in.Type
		if typ == "" {
			typ = "MemoryNote"
		}
		return mangrove.Object{Type: typ, Cell: cell, Content: in.Content, MediaType: in.MediaType}, nil
	}
}

// mangroveFragmentCell mirrors the proxy's own derivation, and is duplicated for
// the same reason mangroveServiceID is: the shape is a wire contract between two
// surfaces, not an implementation detail of either.
func mangroveFragmentCell(entities []memgraph.Entity) string {
	names := make([]string, 0, len(entities))
	for _, e := range entities {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return "graph:" + hex.EncodeToString(sum[:])[:12]
}
