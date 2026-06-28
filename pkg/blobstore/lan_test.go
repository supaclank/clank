package blobstore

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestLAN(t *testing.T) *LAN {
	t.Helper()
	// Bind loopback in tests — we only need a real reachable server, not a
	// LAN-visible one, and "127.0.0.1" is a valid advertise host.
	l, err := NewLAN("127.0.0.1:0", "127.0.0.1", []byte("test-sign-key"))
	if err != nil {
		t.Fatalf("NewLAN: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func putBytes(t *testing.T, url string, data []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new PUT request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	return resp
}

func TestLANRoundTrip(t *testing.T) {
	t.Parallel()
	l := newTestLAN(t)
	ctx := context.Background()
	key := "alice/images/01HIMAGE" // mirrors images.KeyForImage shape (has slashes)
	want := []byte("\x89PNG fake image bytes")

	putURL, err := l.PresignPut(ctx, key, time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if resp := putBytes(t, putURL, want); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}

	ok, err := l.Exists(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Exists after PUT = (%v, %v), want (true, nil)", ok, err)
	}

	getURL, err := l.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET %s: %v", getURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, want) {
		t.Fatalf("GET body = %q, want %q", got, want)
	}
}

func TestLANRejectsTamperedSignature(t *testing.T) {
	t.Parallel()
	l := newTestLAN(t)
	getURL, err := l.PresignGet(context.Background(), "alice/images/x", time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	tampered := strings.Replace(getURL, "/alice/images/x?", "/bob/images/x?", 1) // signature was for alice's key
	resp, err := http.Get(tampered)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered-key GET status = %d, want 403", resp.StatusCode)
	}
}

func TestLANRejectsExpiredURL(t *testing.T) {
	t.Parallel()
	l := newTestLAN(t)
	// Negative TTL → already expired the instant it's minted.
	getURL, err := l.PresignGet(context.Background(), "alice/images/x", -time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired GET status = %d, want 403", resp.StatusCode)
	}
}

func TestLANDeletePrefix(t *testing.T) {
	t.Parallel()
	l := newTestLAN(t)
	ctx := context.Background()
	putURL, _ := l.PresignPut(ctx, "alice/images/one", time.Minute)
	putBytes(t, putURL, []byte("data"))

	if err := l.DeletePrefix(ctx, "alice/"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if ok, _ := l.Exists(ctx, "alice/images/one"); ok {
		t.Fatal("blob still exists after DeletePrefix")
	}
}
