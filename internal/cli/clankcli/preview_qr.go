package clankcli

import (
	"fmt"

	qrcode "github.com/skip2/go-qrcode"
)

// printPreviewBanner renders the pairing QR (the clank://link payload) as
// half-block unicode plus the human-readable details below it.
func printPreviewBanner(link, gatewayURL, previewURL string) {
	fmt.Println()
	if qr, err := qrcode.New(link, qrcode.Low); err == nil {
		// ToSmallString packs two rows per line via half-block runes, so
		// the code stays scannable in a normal-height terminal.
		fmt.Print(qr.ToSmallString(false))
	}
	fmt.Println()
	fmt.Println("Scan with the clank app (same Wi-Fi) to open this preview on your phone.")
	fmt.Printf("  Gateway: %s\n", gatewayURL)
	fmt.Printf("  Metro:   %s\n", previewURL)
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop the preview and shut everything down.")
}
