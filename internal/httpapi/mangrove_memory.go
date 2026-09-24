package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mangrove"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// What a post carries when it is not prose: a workspace file, or a piece of the
// member's memory graph. Plus the two routes those need afterwards -- fetching
// the bytes back, and merging a fragment somebody sent you.

// graphFragment is what travels in the object's content. Field names match the
// graph's own wire shape, so the webapp reads a shared fragment with the types
// it already has for its own graph.
type graphFragment struct {
	Entities  []memgraph.Entity   `json:"entities"`
	Relations []memgraph.Relation `json:"relations"`
}

// composedObject turns one of the three body shapes into the object to publish,
// writing the error response itself when it cannot.
func (s *Server) composedObject(ctx context.Context, w http.ResponseWriter, key docker.WorkspaceKey, agent config.Agent, project string, body publishBody) (mangrove.Object, error) {
	switch {
	case strings.TrimSpace(body.File) != "":
		return s.fileObject(ctx, w, key, agent, project, body)
	case len(body.Entities) > 0:
		return s.fragmentObject(w, key, project, body)
	default:
		return s.noteObject(w, body)
	}
}

func (s *Server) noteObject(w http.ResponseWriter, body publishBody) (mangrove.Object, error) {
	cell := strings.TrimSpace(body.Cell)
	if cell == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"cell" is required: it is what the reduction is keyed by`))
		return mangrove.Object{}, errHandled
	}
	mediaType, ok := composedMediaType(body.MediaType)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody(`"mediaType" must be text/markdown or text/plain`))
		return mangrove.Object{}, errHandled
	}
	return mangrove.Object{
		Type: "MemoryNote", Cell: cell, Content: body.Content, MediaType: mediaType,
	}, nil
}

// fileObject copies a workspace file into the mangrove's blob store.
//
// THE CELL IS THE FILE'S OWN PATH, not something the member types. The reduction
// is keyed by (cell, author), so re-sharing the same file supersedes the earlier
// share rather than accumulating -- which is what a member means when they share
// a document again after editing it.
func (s *Server) fileObject(ctx context.Context, w http.ResponseWriter, key docker.WorkspaceKey, agent config.Agent, project string, body publishBody) (mangrove.Object, error) {
	rel := mediaRelPath(body.File)
	rc, display, err := s.Mgr.OpenMedia(key, agent.Harness, project, rel)
	switch {
	case errors.Is(err, docker.ErrMediaName):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return mangrove.Object{}, errHandled
	case errors.Is(err, docker.ErrMediaNotFound):
		writeJSON(w, http.StatusNotFound, errBody("no such file in your workspace"))
		return mangrove.Object{}, errHandled
	case err != nil:
		s.logf("mangrove: open media: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not read the file"))
		return mangrove.Object{}, errHandled
	}
	defer rc.Close()

	digest, size, err := s.mangroveClient().PutBlob(ctx, rc)
	if err != nil {
		var me *mangrove.Error
		if errors.As(err, &me) {
			// The mangrove's own refusal, which says the limit in bytes.
			writeRawMangrove(w, me)
			return mangrove.Object{}, errHandled
		}
		s.logf("mangrove: put blob: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("the mangrove is unreachable"))
		return mangrove.Object{}, errHandled
	}
	return mangrove.Object{
		Type: "MemoryFile", Cell: rel, Blob: digest, FileName: display, Size: size,
	}, nil
}

// fragmentObject extracts the named entities AND THE RELATIONS AMONG THEM.
//
// memgraph.OpenNodes already computes exactly that, through relationsAmong, so a
// shared fragment never carries a dangling edge. Reimplementing the extraction
// here would be a second answer to a question the graph already answers.
func (s *Server) fragmentObject(w http.ResponseWriter, key docker.WorkspaceKey, project string, body publishBody) (mangrove.Object, error) {
	if s.MemoryGraph == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("no memory graph on this deployment"))
		return mangrove.Object{}, errHandled
	}
	names := make([]string, 0, len(body.Entities))
	for _, n := range body.Entities {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody(`"entities" named nothing`))
		return mangrove.Object{}, errHandled
	}

	sc := scopeOf(key)
	sc.Project = project
	g, err := s.MemoryGraph.OpenNodes(sc, names)
	if err != nil {
		s.logf("mangrove: open nodes: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not read your memory graph"))
		return mangrove.Object{}, errHandled
	}
	if len(g.Entities) == 0 {
		writeJSON(w, http.StatusNotFound, errBody("none of those entities are in your graph"))
		return mangrove.Object{}, errHandled
	}

	content, err := json.Marshal(graphFragment{Entities: g.Entities, Relations: g.Relations})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return mangrove.Object{}, errHandled
	}
	return mangrove.Object{
		Type:      "MemoryNote",
		Cell:      fragmentCell(g.Entities),
		Content:   string(content),
		MediaType: mangrove.GraphFragmentMediaType,
	}, nil
}

// fragmentCell derives the reduction key from the SET OF NAMES, never from
// anything the caller types.
//
// The reduction is keyed by (cell, author) and is last-writer-wins, so the cell
// decides what supersedes what. A caller-supplied label would let two unrelated
// multi-node shares collide on a careless string and silently overwrite each
// other. Deriving it makes a collision mean the one thing it should: the SAME
// set, shared again, which is exactly "here is my current view of these nodes".
//
// The `graph:` prefix also keeps a fragment from superseding a prose note whose
// cell is a bare entity name. They are different claims about the same subject.
func fragmentCell(entities []memgraph.Entity) string {
	names := make([]string, 0, len(entities))
	for _, e := range entities {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return "graph:" + hex.EncodeToString(sum[:])[:12]
}

// handleMangroveBlob serves GET /v1/mangrove/blob?blob=<digest> -- the bytes of a
// file somebody shared, for a member who can see a live post naming them.
//
// The proxy does not decide who may have it. It asks the mangrove, which answers
// from the same visibility its timeline uses, so a revoked post's bytes stop
// being readable at the same moment the post stops being readable.
func (s *Server) handleMangroveBlob(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.mangroveCaller(w, r, false)
	if !ok {
		return
	}
	digest := strings.TrimSpace(r.URL.Query().Get("blob"))
	if digest == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"blob" is required`))
		return
	}
	rc, disposition, size, err := s.mangroveClient().FetchBlob(r.Context(), s.mangroveTuple(key), digest)
	if err != nil {
		var me *mangrove.Error
		if errors.As(err, &me) {
			writeRawMangrove(w, me)
			return
		}
		s.logf("mangrove: fetch blob: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("the mangrove is unreachable"))
		return
	}
	defer rc.Close()

	// Never inline, whatever the bytes are -- the same invariant /v1/media holds.
	w.Header().Set("Content-Type", "application/octet-stream")
	if disposition == "" {
		disposition = `attachment; filename="download"`
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	_, _ = io.Copy(w, rc)
}

// errHandled says the response is already written. It is a sentinel rather than
// a bare nil so a caller cannot mistake "refused" for "succeeded with an empty
// object".
var errHandled = errors.New("handled")

// writeRawMangrove forwards the mangrove's own refusal verbatim, status and all.
// Its bodies name the addressee that was out of reach, or the limit in bytes --
// specifics this proxy would only blur.
func writeRawMangrove(w http.ResponseWriter, me *mangrove.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(me.Status)
	_, _ = io.WriteString(w, me.Body)
}

// handleMangroveMerge serves POST /v1/mangrove/merge -- a person taking a memory
// fragment somebody shared into their own graph.
//
// THIS IS THE ONE WRITE THE GRAPH HAS FROM THE WEB SIDE, and it is allowed
// because of what it cannot do. `internal/httpapi/memory_graph.go` states there
// is no write route here: the bot writes through MCP and the browser only reads.
// The purpose of that rule is that a member's UI cannot AUTHOR memory. This route
// keeps it: THE REQUEST BODY CANNOT CARRY AN ENTITY. It names an object, the
// proxy reads the fragment out of the mangrove, and merges what the log holds.
//
// AN AGENT CANNOT REACH IT. `mangrove_admit` lets an agent admit for itself with
// no human in the loop; if admitting merged, an agent steered by untrusted text
// could publish entities, address a peer, and have that peer write them into its
// own graph -- and the graph is what steers later turns. So merging is a person's
// act, taken here, and the agent's admit goes on writing nothing.
func (s *Server) handleMangroveMerge(w http.ResponseWriter, r *http.Request) {
	key, _, _, project, ok := s.mangroveWriteCaller(w, r)
	if !ok {
		return
	}
	if s.MemoryGraph == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("no memory graph on this deployment"))
		return
	}
	var body struct {
		ObjectID string `json:"objectId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil || body.ObjectID == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"objectId" is required`))
		return
	}

	obj, err := s.sharedObject(r, key, body.ObjectID)
	if err != nil {
		var me *mangrove.Error
		if errors.As(err, &me) {
			writeRawMangrove(w, me)
			return
		}
		s.logf("mangrove: merge: read timeline: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("the mangrove is unreachable"))
		return
	}
	if obj == nil {
		// Not visible to this member, or never existed. One answer for both.
		writeJSON(w, http.StatusNotFound, errBody("no such shared memory is visible to you"))
		return
	}
	if obj.MediaType != mangrove.GraphFragmentMediaType {
		writeJSON(w, http.StatusBadRequest, errBody("that post is not a memory-graph fragment"))
		return
	}

	var frag graphFragment
	if err := json.Unmarshal([]byte(obj.Content), &frag); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("the fragment could not be read"))
		return
	}

	sc := scopeOf(key)
	sc.Project = project
	source := "mangrove:" + obj.ID
	created, added, relations, err := s.mergeFragment(sc, frag, source)
	if err != nil {
		s.logf("mangrove: merge: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not write your memory graph"))
		return
	}

	// MERGING IS CERTAINLY READING. This used to admit the item so it would stop
	// being offered for admission; the hold is gone, but the receipt is still
	// right -- somebody who took a fragment into their graph has opened it, and
	// an inbox that still showed it as unread afterwards would be wrong in the
	// one case it can be sure about.
	//
	// Best effort, as the admit was: the graph write already succeeded, and
	// failing the whole call over a receipt would tell the member their merge
	// did not happen when it did.
	if _, err := s.mangroveClient().React(
		r.Context(), s.mangroveTuple(key), mangrove.AsPerson, "read", obj.ID, false,
	); err != nil {
		s.logf("mangrove: merge: read receipt after merge: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entitiesCreated":   created,
		"observationsAdded": added,
		"relationsCreated":  relations,
	})
}

// sharedObject finds a shared object this member can see, by its object id.
//
// It goes through the timeline rather than a lookup of its own, because the
// timeline is where the visibility rule is answered. A merge must not be able to
// reach something a read cannot.
//
// ONE LIST TO SEARCH. There were two -- claims, and a `held` list for what the
// member had not admitted -- and this returned the held item's ACTIVITY id
// alongside the object so the caller could admit it afterwards. Both are gone:
// the mangrove returns one list, and the receipt the caller now emits names the
// object, which it already had.
func (s *Server) sharedObject(r *http.Request, key docker.WorkspaceKey, objectID string) (*mangrove.Object, error) {
	raw, err := s.mangroveClient().Timeline(r.Context(), s.mangroveTuple(key), mangrove.AsPerson, "received")
	if err != nil {
		return nil, err
	}
	var tl struct {
		Claims []struct {
			Object mangrove.Object `json:"object"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(raw, &tl); err != nil {
		return nil, err
	}
	for _, c := range tl.Claims {
		if c.Object.ID == objectID {
			o := c.Object
			return &o, nil
		}
	}
	return nil, nil
}

// mergeFragment is THREE existing functions in one order, and the order is the
// design.
//
// CreateEntities SKIPS a name that already exists, so using it alone would give
// a recipient who already knows an entity none of the shared observations about
// it -- a skip wearing the word merge. AddObservations afterwards works precisely
// because every name exists by then, which matters: it validates all its inputs
// before mutating any and fails the whole call on one missing entity.
//
// Re-adding the observations of a just-created entity costs nothing, because it
// deduplicates by content. CreateRelations deduplicates by key. All three stamp
// `source`, which is how a later reader tells borrowed memory from their own.
func (s *Server) mergeFragment(sc memgraph.Scope, frag graphFragment, source string) (created, added, relations int, err error) {
	ents, err := s.MemoryGraph.CreateEntities(sc, frag.Entities, source)
	if err != nil {
		return 0, 0, 0, err
	}
	created = len(ents.Created)

	// COUNT WHAT ARRIVED, not what one call did. CreateEntities brings a new
	// entity's observations in with it, so AddObservations afterwards finds them
	// all duplicates and reports zero -- and a member told "2 entities, 0
	// observations" would reasonably conclude the content had been dropped.
	fresh := map[string]bool{}
	for _, n := range ents.Created {
		fresh[n] = true
	}
	for _, e := range frag.Entities {
		if fresh[e.Name] {
			added += len(e.Observations)
		}
	}

	inputs := make([]memgraph.ObservationInput, 0, len(frag.Entities))
	for _, e := range frag.Entities {
		contents := make([]string, 0, len(e.Observations))
		for _, o := range e.Observations {
			contents = append(contents, o.Content)
		}
		if len(contents) > 0 {
			inputs = append(inputs, memgraph.ObservationInput{EntityName: e.Name, Contents: contents})
		}
	}
	if len(inputs) > 0 {
		res, err := s.MemoryGraph.AddObservations(sc, inputs, source)
		if err != nil {
			return created, 0, 0, err
		}
		for _, r := range res {
			// Only the entities that already existed can contribute here; the
			// fresh ones were counted above and deduplicate to zero.
			added += r.Added
		}
	}
	if len(frag.Relations) > 0 {
		rel, err := s.MemoryGraph.CreateRelations(sc, frag.Relations, source)
		if err != nil {
			return created, added, 0, err
		}
		relations = rel.Created
	}
	return created, added, relations, nil
}
