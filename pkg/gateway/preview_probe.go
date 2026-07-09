// preview_probe.go — async differential probe fired when the preview
// proxy hits an upstream error.
//
// A tunnel dial traverses gateway → provider edge → host agent →
// in-host loopback, and its failure alone can't say which hop broke.
// The probe immediately GETs the same host's front door (HostRef.URL
// + /ping, the host agent's cheapest endpoint) over the ordinary
// transport. The paired log lines separate the failure domains:
//
//	upstream error + probe OK     → provider proxy path (edge/agent)
//	upstream error + probe failed → host down/unreachable, or
//	                                provider-wide outage
//
// That pairing — with both latencies — is the evidence an infra
// support ticket needs.
//
// The probe runs after a dial already attempted to reach the host, so
// it adds no *new* wake side effect beyond what the failed request
// itself triggered (cf. GetHostByID's "don't wake on stale URLs" rule).
package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/preview/routestore"
)

const (
	// probeMinInterval rate-limits probes per host so an outage under
	// request load yields one diagnostic pair per interval, not a storm.
	probeMinInterval = 30 * time.Second
	// probeTimeout bounds the whole probe (host lookup + GET). Kept
	// short: the probe is diagnostic, never on a request's critical path.
	probeTimeout = 3 * time.Second
	// probePath is the host agent's health endpoint.
	probePath = "/ping"
)

// probeHostAfterUpstreamError schedules an async front-door probe of
// the route's host, rate-limited per host. Never blocks the caller.
func (s *previewState) probeHostAfterUpstreamError(route routestore.Route) {
	s.probeMu.Lock()
	last, seen := s.lastProbe[route.HostID]
	tooSoon := seen && s.now().Sub(last) < probeMinInterval
	if !tooSoon {
		s.lastProbe[route.HostID] = s.now()
	}
	s.probeMu.Unlock()
	if tooSoon {
		return
	}
	go s.runHostProbe(route.HostID)
}

// runHostProbe resolves the host and GETs its front-door health
// endpoint, logging outcome + latency for both phases.
func (s *previewState) runHostProbe(hostID string) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	lookupStart := time.Now()
	ref, err := s.gw.cfg.Provisioner.GetHostByID(ctx, hostID)
	if err != nil {
		s.log.Printf("preview probe: host %s lookup failed in %s: %v",
			hostID, time.Since(lookupStart).Round(time.Millisecond), err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(ref.URL, "/")+probePath, nil)
	if err != nil {
		s.log.Printf("preview probe: host %s build request: %v", hostID, err)
		return
	}
	client := &http.Client{Transport: ref.Transport}
	getStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		s.log.Printf("preview probe: host %s front door %s FAILED in %s: %v",
			hostID, probePath, time.Since(getStart).Round(time.Millisecond), err)
		return
	}
	_ = resp.Body.Close()
	s.log.Printf("preview probe: host %s front door %s status=%d in %s",
		hostID, probePath, resp.StatusCode, time.Since(getStart).Round(time.Millisecond))
}
