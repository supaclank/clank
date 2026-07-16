package webpreview

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
)

// DictationEngine selects how the overlay transcribes push-to-talk
// audio. The value is a user preference persisted across preview runs
// (see Options.PersistDictationEngine); empty means "not chosen yet",
// which makes the overlay ask on first dictation.
type DictationEngine string

const (
	// DictationLocal transcribes on this machine via the configured
	// Engine (clank-voice or an exec command) — audio never leaves it.
	DictationLocal DictationEngine = "local"
	// DictationWebSpeech transcribes in the browser via the Web Speech
	// API (SpeechRecognition), which typically uploads audio to the
	// browser vendor's speech service (Google in Chrome, Apple in
	// Safari). No audio touches the /__clank/voice socket.
	DictationWebSpeech DictationEngine = "webspeech"
)

// ParseDictationEngine validates a stored or client-sent engine string.
// Empty is NOT valid here — "unchosen" is a caller-level state, not an
// engine.
func ParseDictationEngine(s string) (DictationEngine, bool) {
	switch DictationEngine(s) {
	case DictationLocal, DictationWebSpeech:
		return DictationEngine(s), true
	}
	return "", false
}

// dictationState is the engine choice as served to pages this run.
// Shared by the injected-config builder (reads) and the settings
// endpoint (writes), so a reload right after a switch sees the new
// engine without waiting for a preview restart.
type dictationState struct {
	mu     sync.RWMutex
	engine DictationEngine
}

func (d *dictationState) get() DictationEngine {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.engine
}

func (d *dictationState) set(e DictationEngine) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.engine = e
}

// maxDictationBodyBytes bounds the settings request body; the real
// payload is a ~25-byte JSON object.
const maxDictationBodyBytes = 4096

// handleSetDictationEngine is the overlay's engine-picker write path:
// POST {"engine":"local"|"webspeech"}. The in-memory state updates
// first — the choice must hold for this run even when persisting it
// for future runs fails (that failure surfaces as a 500 the overlay
// toasts about).
func handleSetDictationEngine(state *dictationState, persist func(DictationEngine) error, lg *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Engine string `json:"engine"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxDictationBodyBytes)).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		engine, ok := ParseDictationEngine(body.Engine)
		if !ok {
			http.Error(w, fmt.Sprintf("unknown dictation engine %q (want %q or %q)", body.Engine, DictationLocal, DictationWebSpeech), http.StatusBadRequest)
			return
		}
		state.set(engine)
		if persist != nil {
			if err := persist(engine); err != nil {
				lg.Printf("webpreview: persist dictation engine: %v", err)
				http.Error(w, "engine switched for this preview, but saving the choice failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
