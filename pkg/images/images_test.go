package images_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/blobstore"
	"github.com/acksell/clank/pkg/images"
)

func TestKeyForImage(t *testing.T) {
	t.Parallel()
	got, err := images.KeyForImage("user-A", "01JABCDEF0123456789ABCDEFG")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "user-A/images/01JABCDEF0123456789ABCDEFG"; got != want {
		t.Fatalf("KeyForImage mismatch: got %q want %q", got, want)
	}

	bad := []struct{ userID, imageID string }{
		{"", "img"},
		{"..", "img"},
		{"u/v", "img"},
		{"user-A", ""},
		{"user-A", "../escape"},
		{"user-A", "a/b"},
	}
	for _, c := range bad {
		if _, err := images.KeyForImage(c.userID, c.imageID); !errors.Is(err, blobstore.ErrInvalidPathComponent) {
			t.Errorf("KeyForImage(%q,%q): want ErrInvalidPathComponent, got %v", c.userID, c.imageID, err)
		}
	}
}

type presignResp struct {
	ImageID   string `json:"image_id"`
	PutURL    string `json:"put_url"`
	GetURL    string `json:"get_url"`
	ExpiresAt string `json:"expires_at"`
}

func presign(t *testing.T, h http.Handler, userID, body string) (*httptest.ResponseRecorder, presignResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(body))
	if userID != "" {
		req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{UserID: userID}))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var resp presignResp
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return rr, resp
}

func TestPresignImage_RoundTrip(t *testing.T) {
	t.Parallel()
	mem := blobstore.NewMemory()
	defer mem.Close()
	srv, err := images.NewServer(images.Config{Storage: mem})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	rr, resp := presign(t, h, "user-A", `{"mime":"image/png","filename":"shot.png"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("presign: got %d, body=%s", rr.Code, rr.Body.String())
	}
	if resp.ImageID == "" || resp.PutURL == "" || resp.GetURL == "" {
		t.Fatalf("presign returned empty fields: %+v", resp)
	}
	// The blob must be scoped under the caller's tenant prefix.
	if !strings.Contains(resp.PutURL, "user-A/images/"+resp.ImageID) {
		t.Fatalf("put_url not tenant-scoped: %s", resp.PutURL)
	}

	// Upload the bytes via the presigned PUT URL.
	imgBytes := []byte("\x89PNG\r\n\x1a\n fake image bytes")
	putReq, _ := http.NewRequest(http.MethodPut, resp.PutURL, bytes.NewReader(imgBytes))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT got %d", putResp.StatusCode)
	}

	// Download via the presigned GET URL — the sprite's path.
	getResp, err := http.Get(resp.GetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET got %d", getResp.StatusCode)
	}
	got, _ := io.ReadAll(getResp.Body)
	if !bytes.Equal(got, imgBytes) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, imgBytes)
	}
}

func TestPresignImage_RejectsBadMime(t *testing.T) {
	t.Parallel()
	mem := blobstore.NewMemory()
	defer mem.Close()
	srv, _ := images.NewServer(images.Config{Storage: mem})
	h := srv.Handler()

	for _, mime := range []string{"", "application/pdf", "text/plain", "image/svg+xml"} {
		rr, _ := presign(t, h, "user-A", `{"mime":"`+mime+`"}`)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("mime %q: want 400, got %d", mime, rr.Code)
		}
	}
}

func TestPresignImage_RequiresPrincipal(t *testing.T) {
	t.Parallel()
	mem := blobstore.NewMemory()
	defer mem.Close()
	srv, _ := images.NewServer(images.Config{Storage: mem})
	h := srv.Handler()

	rr, _ := presign(t, h, "", `{"mime":"image/png"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: want 401, got %d", rr.Code)
	}
}

func TestNewServer_RequiresStorage(t *testing.T) {
	t.Parallel()
	if _, err := images.NewServer(images.Config{}); err == nil {
		t.Fatal("expected error when Storage is nil")
	}
}
