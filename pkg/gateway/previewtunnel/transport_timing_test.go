package previewtunnel

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTunnel_DialErrorCarriesElapsedAndBudget(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("simulated dial failure")
	prov := &dialProvisioner{dialErr: sentinel}
	tun, err := New(prov, "host-a", 42, Config{DialTimeout: 123 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tun.Close()

	_, err = (&http.Client{Transport: tun}).Get("http://placeholder/")
	if err == nil {
		t.Fatal("expected error from failed dial, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"dial host host-a port 42", "failed after", "budget 123ms"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if !strings.Contains(msg, sentinel.Error()) {
		t.Errorf("error %q lost the underlying cause", msg)
	}
}

func TestTunnel_SlowDialLogged(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var lines []string
	logf := func(format string, v ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, v...)))
	}

	prov := &dialProvisioner{
		target: strings.TrimPrefix(srv.URL, "http://"),
		delay:  30 * time.Millisecond,
	}
	tun, err := New(prov, "host-b", 7, Config{Log: logf, SlowDialWarn: 1 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tun.Close()

	resp, err := (&http.Client{Transport: tun}).Get("http://placeholder/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("slow-dial log lines = %d, want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "slow dial host host-b port 7") {
		t.Errorf("unexpected log line: %s", lines[0])
	}
}

func TestTunnel_FastDialNotLogged(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var lines []string
	logf := func(format string, v ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, v...))
	}

	prov := &dialProvisioner{target: strings.TrimPrefix(srv.URL, "http://")}
	// Default SlowDialWarn (1s) — an instant local dial must stay quiet.
	tun, err := New(prov, "host-c", 7, Config{Log: logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tun.Close()

	resp, err := (&http.Client{Transport: tun}).Get("http://placeholder/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 0 {
		t.Errorf("fast dial should not log, got: %v", lines)
	}
}
