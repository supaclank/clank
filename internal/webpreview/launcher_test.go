package webpreview

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLauncherAcknowledgementPersistsAndUpdatesInjectedConfig(t *testing.T) {
	t.Parallel()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head></head><body>app</body></html>")
	})
	var persisted atomic.Int32
	s := startTestStackOpts(t, upstream, http.NotFoundHandler(), func(o *Options) {
		o.PersistLauncherSeen = func() error {
			persisted.Add(1)
			return nil
		}
	})

	if body := htmlBody(t, s); !strings.Contains(body, `"launcher_seen":false`) {
		t.Fatalf("initial config must identify first use, got: %s", body)
	}
	req, _ := http.NewRequest(http.MethodPost, s.URL+LauncherSeenPath, nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("acknowledge status = %d, want 204", resp.StatusCode)
	}
	if persisted.Load() != 1 {
		t.Fatalf("persist calls = %d, want 1", persisted.Load())
	}
	if body := htmlBody(t, s); !strings.Contains(body, `"launcher_seen":true`) {
		t.Fatalf("reload must see acknowledged launcher, got: %s", body)
	}
}

func TestLauncherAcknowledgementRequiresPreviewToken(t *testing.T) {
	t.Parallel()
	s := startTestStack(t, http.NotFoundHandler(), http.NotFoundHandler())
	resp, err := http.Post(s.URL+LauncherSeenPath, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
