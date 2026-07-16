package webpreview

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestParseDictationEngine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want DictationEngine
		ok   bool
	}{
		{"local", DictationLocal, true},
		{"webspeech", DictationWebSpeech, true},
		{"", "", false},
		{"Local", "", false},
		{"google", "", false},
	}
	for _, c := range cases {
		got, ok := ParseDictationEngine(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseDictationEngine(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestStartRejectsUnknownDictationEngine(t *testing.T) {
	t.Parallel()
	_, err := Start(Options{
		UpstreamPort:     1,
		DaemonSocketPath: "/tmp/nope.sock",
		Token:            "t",
		DictationEngine:  DictationEngine("cloudz"),
	})
	if err == nil || !strings.Contains(err.Error(), "dictation engine") {
		t.Fatalf("Start with a bogus engine: err = %v, want a dictation engine error", err)
	}
}

// htmlBody fetches / through the proxy and returns the injected HTML.
func htmlBody(t *testing.T, s *Server) string {
	t.Helper()
	resp, err := http.Get(s.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func postEngine(t *testing.T, s *Server, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", s.URL+"/__clank/voice/engine", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /__clank/voice/engine: %v", err)
	}
	resp.Body.Close()
	return resp
}

func TestDictationEnginePersistsAndServesTheSwitch(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var persisted []DictationEngine
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><head></head><body>app</body></html>")
	})
	s := startTestStackOpts(t, upstream, http.NotFoundHandler(), func(o *Options) {
		o.DictationEngine = DictationLocal
		o.PersistDictationEngine = func(e DictationEngine) error {
			mu.Lock()
			defer mu.Unlock()
			persisted = append(persisted, e)
			return nil
		}
	})

	if body := htmlBody(t, s); !strings.Contains(body, `"dictation_engine":"local"`) {
		t.Fatalf("initial config must carry the stored engine, got: %s", body)
	}

	if resp := postEngine(t, s, "sekrit", `{"engine":"webspeech"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("switch status = %d, want 204", resp.StatusCode)
	}
	mu.Lock()
	got := append([]DictationEngine(nil), persisted...)
	mu.Unlock()
	if len(got) != 1 || got[0] != DictationWebSpeech {
		t.Errorf("persisted = %v, want [webspeech]", got)
	}
	// A reload after the switch must see the new engine without a
	// preview restart — the injected config is built per response.
	if body := htmlBody(t, s); !strings.Contains(body, `"dictation_engine":"webspeech"`) {
		t.Errorf("reloaded config must carry the switched engine, got: %s", body)
	}
}

func TestDictationEngineEndpointRejectsBadInput(t *testing.T) {
	t.Parallel()
	persistCalls := 0
	s := startTestStackOpts(t, http.NotFoundHandler(), http.NotFoundHandler(), func(o *Options) {
		o.PersistDictationEngine = func(DictationEngine) error { persistCalls++; return nil }
	})

	if resp := postEngine(t, s, "", `{"engine":"webspeech"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tokenless status = %d, want 401", resp.StatusCode)
	}
	if resp := postEngine(t, s, "sekrit", `{"engine":"skynet"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown engine status = %d, want 400", resp.StatusCode)
	}
	if resp := postEngine(t, s, "sekrit", `{{`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("garbage body status = %d, want 400", resp.StatusCode)
	}
	if persistCalls != 0 {
		t.Errorf("persist ran %d times for rejected requests, want 0", persistCalls)
	}
}

// A failed persist still switches the running preview (the user made an
// explicit choice; losing it to a disk error would be worse), but the
// client hears about it via the 500.
func TestDictationEnginePersistFailureStillSwitchesThisRun(t *testing.T) {
	t.Parallel()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><head></head><body>app</body></html>")
	})
	s := startTestStackOpts(t, upstream, http.NotFoundHandler(), func(o *Options) {
		o.PersistDictationEngine = func(DictationEngine) error { return errors.New("disk full") }
	})

	if resp := postEngine(t, s, "sekrit", `{"engine":"local"}`); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failing persist status = %d, want 500", resp.StatusCode)
	}
	if body := htmlBody(t, s); !strings.Contains(body, `"dictation_engine":"local"`) {
		t.Errorf("engine must still switch for this run, got: %s", body)
	}
}

// Without a persist hook the endpoint still works — the choice is
// simply scoped to this preview run.
func TestDictationEngineEndpointWorksWithoutPersistHook(t *testing.T) {
	t.Parallel()
	s := startTestStack(t, http.NotFoundHandler(), http.NotFoundHandler())
	if resp := postEngine(t, s, "sekrit", `{"engine":"webspeech"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// The overlay names what "fully local" runs (the bundled Parakeet
// model vs. the user's exec command), keyed off the injected
// voice_engine kind.
func TestOverlayConfigNamesLocalEngineKind(t *testing.T) {
	t.Parallel()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><head></head><body>app</body></html>")
	})
	cases := []struct {
		name   string
		engine Engine
		want   string
	}{
		{"sherpa", &SherpaEngine{Bin: "clank-voice"}, `"voice_engine":"sherpa"`},
		{"exec", &ExecEngine{Cmdline: "cat"}, `"voice_engine":"exec"`},
		{"off", nil, `"voice_engine":""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := startTestStackOpts(t, upstream, http.NotFoundHandler(), func(o *Options) { o.Engine = c.engine })
			if body := htmlBody(t, s); !strings.Contains(body, c.want) {
				t.Errorf("injected config missing %s, got: %s", c.want, body)
			}
		})
	}
}
