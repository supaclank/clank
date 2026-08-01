package webpreview

import (
	"net/url"
	"strings"
	"testing"
)

func TestStartRejectsInvalidUpstreamURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		target  *url.URL
		wantErr string
	}{
		{name: "missing", wantErr: "upstream URL is required"},
		{name: "relative", target: &url.URL{Path: "app"}, wantErr: "absolute"},
		{name: "unsupported scheme", target: &url.URL{Scheme: "ftp", Host: "127.0.0.1:21"}, wantErr: "http or https"},
		{name: "remote host", target: &url.URL{Scheme: "https", Host: "example.test"}, wantErr: "loopback"},
		{name: "path", target: &url.URL{Scheme: "http", Host: "127.0.0.1:3000", Path: "/app"}, wantErr: "origin only"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Start(Options{
				UpstreamURL:      tt.target,
				DaemonSocketPath: "/tmp/unused.sock",
				Token:            "token",
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Start: err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
