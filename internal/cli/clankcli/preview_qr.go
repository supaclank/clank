package clankcli

import (
	"fmt"

	qrcode "github.com/skip2/go-qrcode"
)

// printQR renders a clank://link payload as half-block unicode,
// scannable in a normal-height terminal. Shared by the preview banner
// and `clank pair`.
func printQR(link string) {
	if qr, err := qrcode.New(link, qrcode.Low); err == nil {
		// ToSmallString packs two rows per line via half-block runes, so
		// the code stays scannable in a normal-height terminal.
		fmt.Print(qr.ToSmallString(false))
	}
}

// printPreviewBanner renders the pairing QR (the clank://link payload) as
// half-block unicode plus the human-readable details below it.
func printPreviewBanner(link, gatewayURL, previewURL string) {
	fmt.Println()
	printQR(link)
	fmt.Println()
	fmt.Println("Scan with the clank app (same Wi-Fi) to open this preview on your phone.")
	fmt.Println("If you're off the same Wi-Fi, connect your devices with Tailscale first.")
	fmt.Printf("  Gateway: %s\n", gatewayURL)
	fmt.Printf("  Metro:   %s\n", previewURL)
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop the preview and shut everything down.")
}
