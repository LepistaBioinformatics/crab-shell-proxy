package httpapi

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// Attachments: the file half of a reply.
//
// picoclaw answers a "send me the file" request with the sentence "Requested
// output delivered via tool attachment." and nothing else -- the file itself goes
// out through the channel's media path, as a message.create carrying
// {type,url,filename,content_type} where url is the harness's own
// /pico/media/<id>. This proxy used to decode neither, which is why that sentence
// arrived alone.
//
// The bytes are copied into the user's uploads dir rather than proxied on demand,
// for two reasons: picoclaw's media store is its own cache with its own lifetime,
// and a file under uploads/ is already listable and downloadable by everything
// that serves a file the user uploaded.

// attachmentFetchTimeout bounds one download. Generous enough for a large report,
// short enough that a wedged media route cannot hold a turn's goroutine open for
// the whole turn budget.
const attachmentFetchTimeout = 60 * time.Second

// maxAttachmentBytes caps what a single delivery may write into the workspace. The
// agent already writes freely inside its own workspace, so this is not a security
// boundary -- it is a guard against one runaway generation filling the disk.
const maxAttachmentBytes = 64 << 20

// storeTurnAttachment fetches one delivered file from the harness and stores it
// under uploads/attachments/.
// project scopes the store to the workspace of the agent that produced the file:
// a turn answered by a project agent delivers into that project's uploads, not
// into the main workspace where its own agent could never open it again.
func (s *Server) storeTurnAttachment(
	ctx context.Context, key docker.WorkspaceKey, project string, a turn.Attachment,
) (docker.StoredMedia, error) {
	if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
		// The runner resolves the harness-relative path before it gets here; an
		// unresolved one means the endpoint could not be parsed, and guessing an
		// origin would be worse than reporting it.
		return docker.StoredMedia{}, fmt.Errorf("attachment url is not absolute: %q", a.URL)
	}

	ctx, cancel := context.WithTimeout(ctx, attachmentFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return docker.StoredMedia{}, err
	}
	// Same bearer the WebSocket authenticated with: the media route is served by
	// the channel itself and expects it (upstream pkg/channels/pico serves
	// GET /pico/media/<id> behind that token).
	if a.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AuthToken)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return docker.StoredMedia{}, fmt.Errorf("fetch attachment: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return docker.StoredMedia{}, fmt.Errorf("fetch attachment: harness answered %s", res.Status)
	}

	name := attachmentName(a, res.Header.Get("Content-Disposition"))
	return s.Mgr.StoreAgentAttachment(key, project, name, io.LimitReader(res.Body, maxAttachmentBytes))
}

// attachmentName is the file name to store under. The frame's own filename is
// authoritative; the response's Content-Disposition is the fallback, and a
// generated name is the last resort so a nameless delivery is still saved rather
// than dropped.
func attachmentName(a turn.Attachment, disposition string) string {
	if n := strings.TrimSpace(a.Filename); n != "" {
		return n
	}
	if _, params, err := mime.ParseMediaType(disposition); err == nil {
		if n := strings.TrimSpace(params["filename"]); n != "" {
			return n
		}
	}
	// The URL's last segment is the media ref id -- opaque, but stable and unique.
	if base := path.Base(a.URL); base != "" && base != "." && base != "/" {
		return "attachment-" + base
	}
	return "attachment"
}

// attachmentNotice is the line appended to the reply so the user knows a file
// arrived and where it is. Plain text on purpose: this is injected by the proxy
// and is NOT part of the harness transcript, so after a reload only the uploads
// sidebar remembers the file -- the path is what makes that findable.
func attachmentNotice(storedPath, filename string) string {
	name := strings.TrimSpace(filename)
	if name == "" {
		name = path.Base(storedPath)
	}
	return fmt.Sprintf("\n\n📎 %s — %s\n", name, storedPath)
}
