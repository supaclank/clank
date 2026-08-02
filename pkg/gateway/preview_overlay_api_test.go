package gateway

import (
	"net/http"
	"testing"
)

func TestOverlayAPIPathAllowed_ProfileWrites(t *testing.T) {
	t.Parallel()
	if !overlayAPIPathAllowed(http.MethodPost, "/presets") {
		t.Fatal("owner overlay must be allowed to save a custom profile")
	}
	if overlayAPIPathAllowed(http.MethodDelete, "/presets/custom") {
		t.Fatal("profile deletion is outside the overlay save-as-new flow")
	}
}
