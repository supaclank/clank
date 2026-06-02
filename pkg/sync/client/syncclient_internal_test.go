package syncclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDefaultHTTPClient_HasResponseHeaderTimeout pins the contract that
// New() with no caller-supplied client gives back a control-plane
// transport with a bounded response-header timeout, so a stuck gateway
// can't hang the CLI/TUI indefinitely on the register / presign /
// commit JSON calls.
func TestDefaultHTTPClient_HasResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	c := defaultHTTPClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Fatal("default client Transport.ResponseHeaderTimeout is zero — calls can hang forever")
	}
}

// TestBlobClient_NoResponseHeaderTimeout is the regression for the
// `clank push` hang: a blob PUT's response headers arrive only after the
// server has received the whole body, so a ResponseHeaderTimeout bounds
// total upload time. A large bundle over a slow tunnel then dies with
// "http2: timeout awaiting response headers". The blob client must carry
// no such cap; ctx is the only deadline.
func TestBlobClient_NoResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	const serverDelay = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(serverDelay) // headers withheld until the "object" is stored
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Reproduce the failure: a client whose ResponseHeaderTimeout is
	// shorter than the server's header delay aborts the upload.
	capped := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 20 * time.Millisecond}}
	if err := uploadBytes(context.Background(), capped, srv.URL, []byte("payload"), "application/octet-stream", nil); err == nil {
		t.Fatal("expected the capped client to time out awaiting response headers")
	}

	// The fix: New()'s blobClient drops the cap and succeeds.
	c, err := New(Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tr, ok := c.blobClient.Transport.(*http.Transport); !ok || tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("blobClient must have ResponseHeaderTimeout==0, got %+v", c.blobClient.Transport)
	}
	if err := uploadBytes(context.Background(), c.blobClient, srv.URL, []byte("payload"), "application/octet-stream", nil); err != nil {
		t.Fatalf("blob client should not cap upload on response-header timeout: %v", err)
	}
}
