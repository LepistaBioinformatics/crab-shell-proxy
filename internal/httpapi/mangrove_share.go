package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mangrove"
)

// handleMangroveShare serves POST /v1/mangrove/share -- a person widening
// something already published to somebody else.
//
// SHARE IS NOT PUBLISH AGAIN. It names an object that already exists and adds an
// addressee, so the object keeps its id, its cell and its author: a memory that
// travelled further is still the same memory, and re-publishing it would put a
// second claim on the same cell competing with the first.
//
// The route existed for agents (`mangrove_share`) and for the service, and for
// nobody else -- so a member could publish something to one colleague and then
// had no way to let a second one see it. That is the gap this closes.
func (s *Server) handleMangroveShare(w http.ResponseWriter, r *http.Request) {
	key, ident, ok := s.mangroveCaller(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ObjectID string               `json:"objectId"`
		To       []string             `json:"to"`
		ToEmails []publishEmailTarget `json:"toEmails"`
		// Undo withdraws a share instead of adding one.
		Undo bool `json:"undo"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.ObjectID == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"objectId" is required`))
		return
	}

	audience, err := s.composedAudience(key, publishBody{To: body.To, ToEmails: body.ToEmails})
	if err != nil {
		s.logf("mangrove: share: resolve audience: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not read the subscription's members"))
		return
	}
	if len(audience) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody("name somebody to share it with"))
		return
	}

	lic := mangrove.Licences{
		Tenant: tenantLicensed(ident, key),
		Groups: governs(ident, key),
	}

	// ONE ACTIVITY PER ADDRESSEE, because that is what the mangrove's share is:
	// an Add naming one target. A partial result is possible and is reported as
	// the refusal it is -- the FIRST refusal stops the rest, so the member is
	// never told "shared" about a list where one name was out of reach.
	var last []byte
	for _, target := range audience {
		raw, err := s.mangroveClient().Share(r.Context(), s.mangroveTuple(key), mangrove.AsPerson,
			lic, body.ObjectID, target, body.Undo)
		if err != nil {
			s.writeMangroveResult(w, nil, err)
			return
		}
		last = raw
	}
	s.writeMangroveResult(w, last, nil)
}
