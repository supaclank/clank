package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/blobstore"
	"github.com/acksell/clank/pkg/images"
	"github.com/acksell/clank/pkg/provisioner"
)

// TestImagesRoute_PresignsThroughGateway verifies POST /v1/images is
// served locally by the embedded images server (not proxied to the host)
// and that the storage key is scoped to the authenticated principal. The
// host URL is bogus on purpose: if the route were proxied, the call would
// fail instead of returning a presign payload.
func TestImagesRoute_PresignsThroughGateway(t *testing.T) {
	t.Parallel()
	mem := blobstore.NewMemory()
	t.Cleanup(mem.Close)
	imgSrv, err := images.NewServer(images.Config{Storage: mem})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	prov := &stubProvisioner{ref: provisioner.HostRef{URL: "http://127.0.0.1:1"}}
	g, err := NewGateway(Config{Provisioner: prov, Images: imgSrv}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	srv := httptest.NewServer(localAuth(g.Handler(), "tester"))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/images", "application/json", strings.NewReader(`{"mime":"image/png"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var out struct {
		ImageID string `json:"image_id"`
		PutURL  string `json:"put_url"`
		GetURL  string `json:"get_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ImageID == "" || out.PutURL == "" || out.GetURL == "" {
		t.Fatalf("incomplete presign response: %+v", out)
	}
	if !strings.Contains(out.PutURL, "tester/images/"+out.ImageID) {
		t.Fatalf("put_url not scoped to authed principal: %s", out.PutURL)
	}
}
