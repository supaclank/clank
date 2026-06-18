package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveImage_DownloadsAndValidates(t *testing.T) {
	t.Parallel()
	want := []byte("\x89PNG\r\n\x1a\n fake png bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(want)
	}))
	defer srv.Close()

	got, err := resolveImage(context.Background(), Attachment{ImageID: "i1", Mime: "image/png", GetURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("body mismatch: got %q want %q", got, want)
	}
}

func TestResolveImage_RejectsBadMime(t *testing.T) {
	t.Parallel()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "application/pdf", GetURL: "http://example.test"}); err == nil {
		t.Fatal("expected error for disallowed mime")
	}
}

func TestResolveImage_RequiresGetURL(t *testing.T) {
	t.Parallel()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "image/png"}); err == nil {
		t.Fatal("expected error for missing get_url")
	}
}

func TestResolveImage_RejectsOversize(t *testing.T) {
	t.Parallel()
	big := make([]byte, maxImageBytes+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "image/png", GetURL: srv.URL}); err == nil {
		t.Fatal("expected error for oversized image")
	}
}

func TestResolveImage_ErrorsOnExpiredURL(t *testing.T) {
	t.Parallel()
	// 403 is what S3 returns for an expired presigned URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := resolveImage(context.Background(), Attachment{Mime: "image/png", GetURL: srv.URL}); err == nil {
		t.Fatal("expected error for 403")
	}
}

func TestResolveAttachments_FailsFast(t *testing.T) {
	t.Parallel()
	want := []byte("img")
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(want)
	}))
	defer good.Close()

	got, err := resolveAttachments(context.Background(), []Attachment{
		{ImageID: "a", Mime: "image/png", GetURL: good.URL},
		{ImageID: "b", Mime: "image/png", GetURL: ""}, // missing get_url → whole call fails
	})
	if err == nil {
		t.Fatalf("expected fail-fast error, got %v", got)
	}
}

func TestDataURL(t *testing.T) {
	t.Parallel()
	got := dataURL("image/png", []byte("hi"))
	if want := "data:image/png;base64,aGk="; got != want {
		t.Fatalf("dataURL mismatch: got %q want %q", got, want)
	}
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("bad data URL prefix: %s", got)
	}
}
