package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/history"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// turnTimeout bounds a picoclaw turn once it is decoupled from the client
// request. It must be generous enough for a long tool-using turn, but stops a
// stuck turn from running forever after the client has gone.
const turnTimeout = 10 * time.Minute

// heartbeatInterval is how often an in-flight turn writes a keep-alive comment.
//
// picoclaw does not stream: it answers in one terminal frame, and a measured
// tool-free turn emitted NOTHING for 51 seconds (chat-responsiveness OQ-1). Between
// its frames this stream is genuinely idle, and an idle connection can be reclaimed
// by any hop between here and the browser -- Traefik, the BFF, mycelium, or the
// member's own carrier/NAT/VPN, which is the hop we can neither see nor configure.
//
// Ten seconds is chosen against the TIGHTEST plausible hop, not against mycelium's
// gatewayTimeout (60s). That 60 is the loosest bound in the chain and the only one
// written down; mobile NAT and edge idle timeouts sit well below it.
//
// NOT configurable on purpose: a knob here cannot be set correctly without knowing
// the member's carrier.
const heartbeatInterval = 10 * time.Second

// streamTurn serves a streaming (SSE) chat completion.
//
// Ordering matters (design D9 / advisor note): the 200 headers and the initial
// role chunk are flushed BEFORE EnsureRunning, so the client connection stays
// open through a cold start and mycelium's gatewayTimeout isn't tripped waiting
// on Docker. Once headers are sent the HTTP status can no longer change, so a
// cold-start or turn failure is surfaced by closing the stream cleanly (a
// [DONE] with no content) and logging — same as server.js.
func (s *Server) streamTurn(w http.ResponseWriter, r *http.Request, agent config.Agent, key docker.WorkspaceKey, ownerEmail, sessionKey, userContent, model, id, project string, steering bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("streaming unsupported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	created := time.Now().Unix()

	// Every write to `w` goes through writeMu.
	//
	// Until the heartbeat below existed there was exactly ONE writer -- streamTurn
	// runs on the request goroutine and RunTurn calls the sinks inline -- so this
	// file needed no lock and had none. The heartbeat is a second goroutine writing
	// the same http.ResponseWriter, which is a data race, and an interleaved write
	// would corrupt a frame the client is mid-parse on.
	//
	// The lock is function-local: it protects ONE response, and two concurrent turns
	// share nothing.
	//
	// Go mutexes are not reentrant, so the emit* closures below are the UNLOCKED
	// bodies and the write* closures are the locking wrappers. `done` needs the
	// unlocked one: it writes two frames (the finish_reason chunk and [DONE]) and
	// must hold the lock across BOTH, or a heartbeat could land between them.
	var writeMu sync.Mutex

	emitChunk := func(delta map[string]any, finish any) {
		payload := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// Progress rides as an extra top-level field on an otherwise-normal chunk
	// with an EMPTY delta. A client that knows nothing about it -- including a
	// generic OpenAI SDK -- reads choices[0].delta.content, finds nothing, and
	// skips the frame: the extension is ignored, never a parse error. A named
	// SSE event (`event: progress`) would instead be dropped wholesale by
	// data:-only parsers, so this shape is the compatible one.
	emitProgress := func(p turn.Progress) {
		payload := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": nil}},
			"x_crab_progress": map[string]any{
				"kind":  p.Kind,
				"text":  p.Text,
				"tool":  p.Tool,
				"state": p.State,
			},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// A failed turn, on the same extension shape as progress and for the same
	// compatibility reason: empty delta, extra top-level field.
	//
	// It is what the member's interface needs to distinguish "answered nothing" from
	// "broke". Without it, both paths are silent — picoclaw's error text is not
	// persisted, so any client that treats it as content loses it to the next
	// reconcile against the durable transcript, and a RunTurn error was only logged.
	emitError := func(message string) {
		payload := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": nil}},
			"x_crab_error": map[string]any{
				"message": message,
			},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// This request's message was folded into a turn that was already running, so the
	// frames that follow belong to that turn and no separate answer is coming. Same
	// shape as progress and error, for the same compatibility reason: an ordinary
	// chunk with an empty delta plus one extra top-level field.
	//
	// An ANNOUNCEMENT, not a refusal. The turn it was folded into is the member's
	// own conversation and its output is what they want to see; what they cannot
	// know without this is why their message got no reply of its own.
	emitSteering := func() {
		payload := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": nil}},
			"x_crab_steering": map[string]any{
				"folded": true,
			},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// The locking wrappers. Everything outside this block calls these, never the
	// emit* bodies above.
	writeChunk := func(delta map[string]any, finish any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		emitChunk(delta, finish)
	}
	writeProgress := func(p turn.Progress) {
		writeMu.Lock()
		defer writeMu.Unlock()
		emitProgress(p)
	}
	writeError := func(message string) {
		writeMu.Lock()
		defer writeMu.Unlock()
		emitError(message)
	}
	done := func() {
		// One critical section for both frames, deliberately: they are the turn's
		// terminal signal and the client treats "no marker" as a cut, so nothing may
		// be written between them.
		writeMu.Lock()
		defer writeMu.Unlock()
		emitChunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	// Open the stream immediately so the connection survives the cold start.
	writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
	// Immediately after the opening chunk, before any wait: this is the one thing
	// the member can act on (wait, or stop the running turn), and it is known
	// before the harness is even contacted.
	if steering {
		writeMu.Lock()
		emitSteering()
		writeMu.Unlock()
	}

	// The turn must complete even if the client disconnects (page reload /
	// navigation). Tying it to r.Context() would cancel the picoclaw WebSocket
	// mid-turn on disconnect, and picoclaw would persist a truncated transcript
	// (the "initial messages disappear after reload" bug). So run it on a
	// background context with its own bound; we only stop *writing* to the client
	// once it goes away, while the turn keeps draining to completion.
	turnCtx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	defer cancel()
	clientCtx := r.Context()

	// Keep the connection carrying bytes for as long as the turn runs.
	//
	// The frame is an SSE COMMENT, and that is load-bearing rather than cosmetic. The
	// webapp derives its "quiet for" readout and its background-turn dock chips from
	// the timestamp of the last EVENT (background-turn-dock DEC-12: "a chip and the
	// band it corresponds to can never disagree"). A heartbeat shaped as an
	// empty-delta x_crab_progress chunk -- the obvious shape, since that extension
	// already exists -- would stamp that timestamp every ten seconds and pin the
	// readout at zero forever, silently deleting long-turn-resilience FR-11/FR-12.
	//
	// A comment cannot: the client's consumeStream skips every line that does not
	// start with "data:" BEFORE any bookkeeping runs. So this needs no webapp change
	// at all, and that property is worth preserving.
	//
	// stopHeartbeat WAITS for the goroutine to exit. Cancelling without waiting would
	// let a ping be written after streamTurn returns, i.e. to a ResponseWriter that is
	// no longer valid.
	hbCtx, cancelHeartbeat := context.WithCancel(turnCtx)
	hbDone := make(chan struct{})
	var hbOnce sync.Once
	stopHeartbeat := func() {
		hbOnce.Do(func() {
			cancelHeartbeat()
			<-hbDone
		})
	}
	defer stopHeartbeat()
	every := s.heartbeatEvery
	if every <= 0 {
		every = heartbeatInterval
	}
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				// Same guard as every sink: the client being gone stops us WRITING,
				// never the turn. Keep ticking rather than returning -- the turn is
				// still draining and nothing here decides its lifetime.
				if clientCtx.Err() != nil {
					continue
				}
				writeMu.Lock()
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
				writeMu.Unlock()
			}
		}
	}()

	tgt, err := s.Mgr.EnsureRunning(turnCtx, agent, key, ownerEmail)
	if err != nil {
		s.logf("stream: ensure running failed: %v", err)
		stopHeartbeat()
		if clientCtx.Err() == nil {
			done()
		}
		return
	}

	_, err = s.Pico.RunTurn(turnCtx, turn.Request{
		Endpoint:   tgt.Endpoint,
		AuthToken:  tgt.AuthToken,
		SessionID:  sessionKey,
		SessionKey: key.UserAccID + ":" + key.Role,
		Model:      model,
		Content:    userContent,
	}, turn.Sink{
		Content: func(delta string) {
			if clientCtx.Err() != nil {
				return // client gone — keep draining so the agent finishes its write
			}
			writeChunk(map[string]any{"content": delta}, nil)
		},
		Progress: func(p turn.Progress) {
			if clientCtx.Err() != nil {
				return
			}
			writeProgress(p)
		},
		Error: func(message string) {
			if clientCtx.Err() != nil {
				return
			}
			writeError(message)
		},
		// A file the agent delivered out-of-band. picoclaw hands over a URL on its
		// own media route plus the bearer for it; the bytes are copied into the
		// user's uploads dir so they outlive the harness's media store and show up
		// in the uploads sidebar, downloadable, with no frontend work.
		//
		// The reply text is already streaming when this runs, so a failure here is
		// logged and never surfaced as an error: losing the file is bad, replacing
		// the answer the user is reading with a 502 is worse.
		Attachment: func(a turn.Attachment) {
			stored, err := s.storeTurnAttachment(turnCtx, key, project, a)
			if err != nil {
				s.logf("stream: attachment %q not stored: %v", a.Filename, err)
				return
			}
			s.logf("stream: attachment stored at %s (%d bytes)", stored.Path, stored.Size)
			if clientCtx.Err() != nil {
				return
			}
			writeChunk(map[string]any{"content": attachmentNotice(stored.Path, a.Filename)}, nil)
		},
	})
	s.Mgr.ArmIdle(agent, key)
	if err != nil {
		s.logf("stream: turn failed: %v", err)
		// picoclaw's own `error` FRAME lands here (internal/pico/turn.go), as does a
		// transport failure. This used to be logged and nothing else: the client got a
		// well-formed finish_reason "stop" with no content and no reason, so a broken
		// turn was indistinguishable from one that answered nothing.
		if clientCtx.Err() == nil {
			writeError(err.Error())
		}
	}
	// Fold the just-written turn into the durable transcript now — while the live
	// file still holds it — so a later restart that rewrites the live file can't
	// erase the history.
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID, workspaceSegmentOf(project))
	if syncErr := history.SyncDurable(sessionsDir, sessionKey); syncErr != nil {
		s.logf("stream: sync durable history failed: %v", syncErr)
	}
	// Before done(), not merely at function exit: a ping interleaved between the
	// finish_reason chunk and [DONE] would be harmless to parse but is exactly the
	// sort of thing a later reader of a packet capture spends an hour on. done()
	// holding writeMu across both frames closes the window; this closes it earlier.
	stopHeartbeat()
	if clientCtx.Err() == nil {
		done()
	}
}
