package bridge

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestDesiredBindsPolicy(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	if err := s.TrustNetwork("fp-home", "home"); err != nil {
		t.Fatal(err)
	}
	tn := &Tailnet{IP: "100.99.1.2"}
	lan := net.ParseIP("192.168.1.20")
	cgnat := net.ParseIP("100.77.1.1")

	cases := []struct {
		name    string
		tn      *Tailnet
		lanIP   net.IP
		network Network
		want    []string
	}{
		{"loopback only", nil, nil, Network{}, []string{"127.0.0.1"}},
		{"tailnet always binds", tn, nil, Network{}, []string{"127.0.0.1", "100.99.1.2"}},
		{"untrusted network excludes LAN", tn, lan, Network{Fingerprint: "fp-cafe"}, []string{"127.0.0.1", "100.99.1.2"}},
		{"trusted network includes LAN", tn, lan, Network{Fingerprint: "fp-home"}, []string{"127.0.0.1", "100.99.1.2", "192.168.1.20"}},
		{"unidentified network excludes LAN", nil, lan, Network{}, []string{"127.0.0.1"}},
		{"cgnat lanIP is not a LAN", nil, cgnat, Network{Fingerprint: "fp-home"}, []string{"127.0.0.1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := desiredBinds(s, tc.tn, tc.lanIP, tc.network)
			if len(got) != len(tc.want) {
				t.Fatalf("binds = %+v, want IPs %v", got, tc.want)
			}
			for i, d := range got {
				if d.IP != tc.want[i] {
					t.Fatalf("bind[%d] = %s, want %s (all: %+v)", i, d.IP, tc.want[i], got)
				}
			}
		})
	}
}

// freePort grabs an ephemeral port for the lifecycle test — the fixed
// production port would collide across parallel test runs.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestListenersLifecycle(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	port := freePort(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	l := NewListeners(ListenerOptions{
		Port:    port,
		Handler: handler,
		Store:   s,
		LANIP:   func() (net.IP, error) { return nil, fmt.Errorf("no lan") },
		Tailnet: func(context.Context) *Tailnet { return nil },
		Network: func(context.Context) Network { return Network{} },
	})
	defer l.Close()

	status := l.Refresh(context.Background())
	if len(status.Binds) != 1 || status.Binds[0].IP != "127.0.0.1" || status.Binds[0].Err != "" {
		t.Fatalf("status.Binds = %+v, want clean loopback bind", status.Binds)
	}

	// The handler is actually served.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/anything", port))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	// Idempotent refresh: same policy, same bind, no error.
	status = l.Refresh(context.Background())
	if len(status.Binds) != 1 || status.Binds[0].Err != "" {
		t.Fatalf("second refresh: %+v", status.Binds)
	}

	// Close stops serving.
	l.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port)); err != nil {
			return // connection refused — listener is down
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("listener still serving after Close")
}

func TestListenersBindFailureIsRecordedNotFatal(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	port := freePort(t)

	// Occupy the port so the loopback bind fails.
	occupier, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer occupier.Close()

	l := NewListeners(ListenerOptions{
		Port:    port,
		Handler: http.NewServeMux(),
		Store:   s,
		LANIP:   func() (net.IP, error) { return nil, fmt.Errorf("no lan") },
		Tailnet: func(context.Context) *Tailnet { return nil },
		Network: func(context.Context) Network { return Network{} },
	})
	defer l.Close()

	status := l.Refresh(context.Background())
	if len(status.Binds) != 1 || status.Binds[0].Err == "" {
		t.Fatalf("expected recorded bind error, got %+v", status.Binds)
	}
}
