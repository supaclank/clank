package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveImage_HTTPDownloads(t *testing.T) {
	t.Parallel()
	want := []byte("\x89PNG\r\n\x1a\n fake png bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(want)
	}))
	defer srv.Close()

	got, err := resolveImage(context.Background(), Attachment{Mime: "image/png", Source: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("body mismatch: got %q want %q", got, want)
	}
}

func TestResolveImage_DataURL(t *testing.T) {
	t.Parallel()
	raw := []byte("inline-bytes")
	got, err := resolveImage(context.Background(), Attachment{Mime: "image/png", Source: DataURL("image/png", raw)})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("data URL decode mismatch: got %q want %q", got, raw)
	}
}

func TestResolveImage_FileURL_GatedByAllow(t *testing.T) {
	// Not parallel: mutates the AllowLocalFileAttachments global.
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("FILEDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	att := Attachment{Mime: "image/png", Source: "file://" + path}

	prev := AllowLocalFileAttachments
	defer func() { AllowLocalFileAttachments = prev }()

	AllowLocalFileAttachments = false
	if _, err := resolveImage(context.Background(), att); err == nil {
		t.Fatal("file:// must be rejected when not allowed")
	}

	AllowLocalFileAttachments = true
	got, err := resolveImage(context.Background(), att)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "FILEDATA" {
		t.Fatalf("file read mismatch: got %q", got)
	}
}

func TestResolveImage_RejectsBadMime(t *testing.T) {
	t.Parallel()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "application/pdf", Source: "data:application/pdf;base64,AA=="}); err == nil {
		t.Fatal("expected error for disallowed mime")
	}
}

func TestResolveImage_RequiresSource(t *testing.T) {
	t.Parallel()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "image/png"}); err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestResolveImage_RejectsUnknownScheme(t *testing.T) {
	t.Parallel()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "image/png", Source: "ftp://host/x.png"}); err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

func TestResolveImage_RejectsOversizeHTTP(t *testing.T) {
	t.Parallel()
	big := make([]byte, maxImageBytes+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "image/png", Source: srv.URL}); err == nil {
		t.Fatal("expected error for oversized image")
	}
}

func TestResolveImage_ErrorsOnExpiredURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "image/png", Source: srv.URL}); err == nil {
		t.Fatal("expected error for 403")
	}
}

func TestResolveAttachments_FailsFast(t *testing.T) {
	t.Parallel()
	good := DataURL("image/png", []byte("img"))
	_, err := resolveAttachments(context.Background(), []Attachment{
		{Mime: "image/png", Source: good},
		{Mime: "image/png", Source: ""}, // missing source → whole call fails
	})
	if err == nil {
		t.Fatal("expected fail-fast error")
	}
}

func TestDataURL(t *testing.T) {
	t.Parallel()
	if got, want := DataURL("image/png", []byte("hi")), "data:image/png;base64,aGk="; got != want {
		t.Fatalf("DataURL mismatch: got %q want %q", got, want)
	}
}
