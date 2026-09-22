package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mangrove"
)

// The member's WRITE half of the mangrove: composing a memory and addressing it.
//
// Until this route existed the mangrove had an agent-facing publish path and no
// human one, with a consequence nobody had noticed: the two licences that widen
// reach were settable only from a real mycelium profile, the only callers of
// Publish were the MCP tools, and so NO GROUP SCOPE WAS REACHABLE BY ANYBODY.
// The gate's own comment said group scope "is therefore a human action, taken in
// the webapp" while describing a path that was never built. This is that path.

// maxComposedBody bounds a composed memory. Generous for prose, far below
// anything that would be a file -- MemoryFile is not this route's business.
const maxComposedBody = 256 << 10

// publishEmailTarget addresses somebody by email, saying which of their two
// actors is meant.
//
// THE CHOICE HAS TO TRAVEL STRUCTURALLY. The directory's strict mode returns an
// email and no actor id at all, by design: a member confirms that somebody is
// reachable without learning their id. So the webapp cannot build
// `mangrove:actor:<acc>:person` itself, and the person/agent decision has to
// survive the round trip some other way.
//
// It is NOT a new address prefix (`email-person:`). resolveAudience is shared
// with the agent path -- mangrove_share runs its target through it -- so
// extending the prefix vocabulary would widen the AGENT's addressing as a side
// effect of a human-path feature. This body is the proxy's own and nothing else
// consumes it.
type publishEmailTarget struct {
	Email  string `json:"email"`
	Person bool   `json:"person"`
	Agent  bool   `json:"agent"`
}

type publishBody struct {
	Cell      string               `json:"cell"`
	Content   string               `json:"content"`
	MediaType string               `json:"mediaType"`
	To        []string             `json:"to"`
	ToEmails  []publishEmailTarget `json:"toEmails"`

	// File is a workspace media path -- `public/report.pdf`. The proxy reads the
	// file and streams it into the mangrove's blob store; the post names the
	// digest. The member never uploads it again.
	File string `json:"file"`
	// Entities are memory-graph entity names. The post carries those entities
	// AND THE RELATIONS AMONG THEM, which is what makes the fragment usable
	// rather than a list of disconnected nodes.
	Entities []string `json:"entities"`
}

// WHAT A POST CARRIES IS EXACTLY ONE THING. Prose, a file, or a piece of the
// graph. Allowing two would make `cell` ambiguous -- it is the reduction key, and
// each kind derives it differently -- and would leave a reader asking which part
// of an object is the content.
func (b publishBody) kinds() int {
	n := 0
	if strings.TrimSpace(b.Content) != "" {
		n++
	}
	if strings.TrimSpace(b.File) != "" {
		n++
	}
	if len(b.Entities) > 0 {
		n++
	}
	return n
}

// handleMangrovePublish serves POST /v1/mangrove/publish -- a person composing a
// memory and choosing who it reaches.
//
// It publishes AsPerson. A human's composition is signed by their person actor,
// not by their bot: the recipient must be able to tell which of the two wrote
// something, and the whole governance model rests on that distinction.
func (s *Server) handleMangrovePublish(w http.ResponseWriter, r *http.Request) {
	key, ident, agent, project, ok := s.mangroveWriteCaller(w, r)
	if !ok {
		return
	}

	var body publishBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxComposedBody)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("malformed body"))
		return
	}
	if body.kinds() != 1 {
		writeJSON(w, http.StatusBadRequest,
			errBody(`exactly one of "content", "file" or "entities" is required`))
		return
	}

	obj, err := s.composedObject(r.Context(), w, key, agent, project, body)
	if err != nil {
		return // composedObject wrote the response
	}

	audience, err := s.composedAudience(key, body)
	if err != nil {
		s.logf("mangrove: publish: resolve audience: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not read the subscription's members"))
		return
	}

	// The two licences, from the same two functions that back
	// GET /v1/mangrove/capabilities. One function each, so what the UI offers
	// and what the gate accepts cannot drift apart.
	lic := mangrove.Licences{
		Tenant: tenantLicensed(ident, key),
		Groups: governs(ident, key),
	}

	raw, err := s.mangroveClient().Publish(r.Context(), s.mangroveTuple(key), mangrove.AsPerson, lic, obj, audience)
	s.writeMangroveResult(w, raw, err)
}

// composedMediaType accepts the two formats the composer offers, defaulting to
// markdown for an absent one.
//
// It is a closed set rather than a passthrough because the point of the field is
// that the READER stops guessing. An arbitrary string reintroduces the guess on
// the other side, one layer further in.
func composedMediaType(raw string) (string, bool) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", "text/markdown":
		return "text/markdown", true
	case "text/plain":
		return "text/plain", true
	default:
		return "", false
	}
}

// composedAudience turns the body's two addressing forms into one list of actor
// ids for the gate to check.
//
// AN EMAIL THAT MATCHES NOBODY IS KEPT AS `email:<address>`, not dropped. The
// gate refuses it and names it, which is what tells the author their colleague
// was not found -- dropping it would deliver to fewer people than they asked
// for and say nothing, and a silent partial share is the failure this whole
// design refuses.
func (s *Server) composedAudience(key docker.WorkspaceKey, body publishBody) ([]string, error) {
	audience := make([]string, 0, len(body.To)+len(body.ToEmails))
	audience = append(audience, body.To...)

	if len(body.ToEmails) == 0 {
		return audience, nil
	}
	byEmail, err := s.subscriptionByEmail(key)
	if err != nil {
		return nil, err
	}
	for _, t := range body.ToEmails {
		addr := strings.ToLower(strings.TrimSpace(t.Email))
		if addr == "" {
			continue
		}
		acc, found := byEmail[addr]
		if !found {
			audience = append(audience, "email:"+addr)
			continue
		}
		// Neither box ticked means the recipient was named and then addressed
		// at nobody. Treat it as the agent, which is what an unqualified
		// `email:` already means everywhere else in this stack.
		if t.Person {
			audience = append(audience, mangrovePersonID(acc))
		}
		if t.Agent || !t.Person {
			audience = append(audience, mangroveServiceID(acc))
		}
	}
	return audience, nil
}
