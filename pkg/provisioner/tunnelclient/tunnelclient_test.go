package tunnelclient_test

import (
	"context"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/provisioner/transport"
	"github.com/acksell/clank/pkg/provisioner/tunnelclient"
)

// A relative baseURL (missing scheme and host) parses without error
// but yields an empty url.URL.Host, so the tunnel dial would otherwise
// fail with a confusing empty-host error instead of naming the real
// problem.
func TestDial_RejectsNonAbsoluteBaseURL(t *testing.T) {
	t.Parallel()
	cases := []string{"/just/a/path", "justapath", "relative/path"}
	for _, baseURL := range cases {
		_, err := tunnelclient.Dial(context.Background(), baseURL, &transport.BearerInjector{Token: "t"}, 8080)
		if err == nil {
			t.Fatalf("Dial(%q): want error, got nil", baseURL)
		}
		if !strings.Contains(err.Error(), "absolute URL") {
			t.Fatalf("Dial(%q): want absolute-URL error, got: %v", baseURL, err)
		}
	}
}
