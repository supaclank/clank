package daemoncli

import (
	"testing"

	"github.com/acksell/clank/internal/config"
)

func TestFlySpritesOptionsForwardsPreviewWebhook(t *testing.T) {
	t.Setenv("CLANK_PREVIEW_WEBHOOK_URL", "https://gateway.example/webhooks/preview")
	opts := flySpritesOptions(config.FlySpritesPreference{APIToken: "sprite-token"}, nil)
	if opts.PreviewWebhookURL != "https://gateway.example/webhooks/preview" {
		t.Errorf("PreviewWebhookURL = %q", opts.PreviewWebhookURL)
	}
}
