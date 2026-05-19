// Package noop provides a Listener that does nothing. Used in laptop
// mode where there is no provider to keep alive, and as a debug
// stand-in for integration tests.
package noop

import (
	"context"
	"time"
)

// Listener implements keepalive.Listener with no-op methods.
type Listener struct{}

// Tick does nothing.
func (Listener) Tick(_ context.Context, _ time.Time) {}

// Close does nothing.
func (Listener) Close(_ context.Context) error { return nil }
