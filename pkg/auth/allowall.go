package auth

import (
	"fmt"
	"net/http"
)

// AllowAll accepts every request and resolves it to a fixed UserID.
// Used by the unix-socket listener (file permissions are the gate)
// and tests. Never use on a network-exposed listener.
type AllowAll struct {
	UserID string
}

// Verify always returns Principal{UserID: a.UserID}.
func (a *AllowAll) Verify(*http.Request) (Principal, error) {
	if a.UserID == "" {
		return Principal{}, fmt.Errorf("auth: AllowAll.UserID is empty")
	}
	return Principal{UserID: a.UserID}, nil
}
