package webpreview

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Dictation WebSocket protocol (one connection per overlay, one
// engine Session per connection, reused across utterances). The client
// streams while the user holds push-to-talk, then commits:
//
//	client → server   binary frames: s16le 16kHz mono PCM
//	client → server   {"type":"end"}      decode the buffered utterance
//	client → server   {"type":"cancel"}   discard the buffer
//	server → client   {"type":"partial","text":"..."}  cumulative, may
//	                  arrive mid-hold (VAD-segmented engines)
//	server → client   {"type":"transcribing"}
//	server → client   {"type":"final","text":"..."}
//	server → client   {"type":"error","error":"..."}
//
// The binary/JSON split mirrors the deleted hub-era voice bridge
// (internal/voice at 9a5c98d^) — same framing, much smaller job:
// dictation only, no speaker path, no barge-in.
const (
	// maxUtteranceBytes caps buffered PCM between end/cancel marks:
	// 32 kB/s means ~8 minutes of held-down push-to-talk before we
	// refuse — runaway-client protection, not a UX limit.
	maxUtteranceBytes = 16 << 20
)

type voiceMsg struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// serveVoiceWS owns one dictation connection. Runs on the proxy's
// request goroutine until the client goes away.
func serveVoiceWS(w http.ResponseWriter, r *http.Request, engine Engine, lg *log.Logger) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		lg.Printf("webpreview: voice ws accept: %v", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "voice handler exited")
	// A worklet batch is ~4 KiB; raise the per-message limit above the
	// library's 32 KiB default anyway so a client that batches harder
	// (tab throttled in background) doesn't get dropped.
	conn.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Liveness pings. A half-open socket — laptop sleep, crashed tab,
	// network drop — leaves conn.Read blocked forever with no error,
	// and this handler would hold the (serialized) engine session until
	// preview restart, bricking voice for every later connection. Ping
	// is the only disconnect detector an idle WebSocket has; on failure,
	// cancel unblocks the read loop and the deferred Close frees the
	// session. (Found the hard way: a goroutine dump showed a 3-minute-
	// dead client still parked in Read while holding the engine.)
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				perr := conn.Ping(pctx)
				pcancel()
				if perr != nil {
					cancel()
					return
				}
			}
		}
	}()

	// One writer mutex: the read loop (transcribing/error acks) and the
	// results pump both write.
	var wmu sync.Mutex
	writeJSON := func(m voiceMsg) bool {
		data, _ := json.Marshal(m)
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		wmu.Lock()
		defer wmu.Unlock()
		if werr := conn.Write(wctx, websocket.MessageText, data); werr != nil {
			return false
		}
		return true
	}

	// The engine session (the exclusive model slot, for serializing
	// engines) is held PER UTTERANCE, not per connection: opened on the
	// first audio frame, released by the pump right after the final.
	// A connection that isn't actively dictating holds nothing — so a
	// zombie socket (half-open TCP, a tab whose network stack dutifully
	// pongs while the page is gone) can't brick voice for everyone else.
	var (
		sess     Session
		pumpDone chan struct{}
	)
	closeSess := func() {
		if sess == nil {
			return
		}
		_ = sess.Close() // idempotent; also runs after pump-initiated closes
		<-pumpDone
		sess, pumpDone = nil, nil
	}
	defer closeSess()

	// reapSess clears a session whose pump already finished (final
	// delivered → pump closed it and exited). Called from the read loop
	// only, so no lock: pumpDone is closed strictly before the pump ends.
	reapSess := func() {
		if sess == nil {
			return
		}
		select {
		case <-pumpDone:
			sess, pumpDone = nil, nil
		default:
		}
	}

	openSess := func() bool {
		octx, ocancel := context.WithTimeout(ctx, 10*time.Second)
		s, oerr := engine.Open(octx)
		ocancel()
		if oerr != nil {
			lg.Printf("webpreview: voice session open: %v", oerr)
			writeJSON(voiceMsg{Type: "error", Error: "voice engine busy or unavailable: " + oerr.Error()})
			return false
		}
		done := make(chan struct{})
		sess, pumpDone = s, done
		go func() {
			defer close(done)
			for res := range s.Results() {
				switch {
				case res.Err != nil:
					lg.Printf("webpreview: transcribe: %v", res.Err)
					writeJSON(voiceMsg{Type: "error", Error: res.Err.Error()})
					_ = s.Close() // terminal for this utterance — free the slot
				case res.Final:
					writeJSON(voiceMsg{Type: "final", Text: res.Text})
					_ = s.Close()
				default:
					writeJSON(voiceMsg{Type: "partial", Text: res.Text})
				}
			}
		}()
		return true
	}

	var utteranceBytes int
	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			return // client closed / navigated away — normal end
		}
		reapSess()
		switch typ {
		case websocket.MessageBinary:
			if sess == nil && !openSess() {
				continue // audio dropped; the client already got the error
			}
			if utteranceBytes+len(data) > maxUtteranceBytes {
				writeJSON(voiceMsg{Type: "error", Error: "utterance too long"})
				utteranceBytes = 0
				closeSess() // Close discards the buffered audio and frees the slot; Cancel alone would strand it
				continue
			}
			utteranceBytes += len(data)
			if ferr := sess.Feed(data); ferr != nil {
				writeJSON(voiceMsg{Type: "error", Error: ferr.Error()})
			}
		case websocket.MessageText:
			var m voiceMsg
			if jerr := json.Unmarshal(data, &m); jerr != nil {
				continue
			}
			switch m.Type {
			case "cancel":
				utteranceBytes = 0
				closeSess() // frees the slot; a cancel ends the utterance same as end/final does
			case "end":
				utteranceBytes = 0
				if !writeJSON(voiceMsg{Type: "transcribing"}) {
					return
				}
				if sess == nil {
					// Push-to-talk tapped with no audio captured.
					writeJSON(voiceMsg{Type: "final"})
					continue
				}
				if eerr := sess.End(); eerr != nil {
					writeJSON(voiceMsg{Type: "error", Error: eerr.Error()})
				}
			}
		}
	}
}
