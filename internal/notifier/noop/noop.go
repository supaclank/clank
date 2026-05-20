// Package noop implements notifier.Provider as a logger-only sink. It
// exists so cmd/clank-host can wire `--notifier-provider=noop` in dev
// or tests without sending real traffic, and so tests of the notifier
// package itself can construct a Loop without a transport.
package noop

import (
	"context"
	"log"
	"os"

	"github.com/acksell/clank/internal/notifier"
)

// Provider satisfies notifier.Provider. Every Send is logged and
// discarded; Close is a no-op.
type Provider struct {
	log *log.Logger
}

// New constructs a Provider. A nil logger uses a stderr-prefixed
// default.
func New(lg *log.Logger) *Provider {
	if lg == nil {
		lg = log.New(os.Stderr, "[notifier-noop] ", log.LstdFlags|log.Lmsgprefix)
	}
	return &Provider{log: lg}
}

// Send logs and discards n.
func (p *Provider) Send(_ context.Context, n notifier.Notification) error {
	p.log.Printf("notify %s session=%s title=%q", n.Kind, n.SessionID, n.Title)
	return nil
}

// Close is a no-op.
func (p *Provider) Close(_ context.Context) error { return nil }
