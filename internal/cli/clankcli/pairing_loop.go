package clankcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// pairingLoop services the typed-code ceremony while a QR is on screen:
// it leases the pairing window open (the daemon accepts a phone's
// begin only while polled), prompts for the code when a phone is
// waiting, and completes the pairing. Runs until ctx cancels; both
// `clank pair` and `clank preview` start it in the background.
func pairingLoop(ctx context.Context, client *daemonclient.Client, in io.Reader, out io.Writer) {
	lines := make(chan string, 1)
	go readLines(in, lines)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	prompted := false
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			code := strings.TrimSpace(line)
			prompted = false // a fresh prompt is needed after any entry
			if code == "" {
				continue
			}
			tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			device, err := client.Bridge().PairComplete(tctx, code)
			cancel()
			if err != nil {
				fmt.Fprintf(out, "  That code didn't match a waiting phone (%v). Type it again: ", err)
				prompted = true
				continue
			}
			fmt.Fprintf(out, "✓ Approved %s — connecting…\n", device)
		case <-ticker.C:
			tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			pending, err := client.Bridge().PairPoll(tctx)
			cancel()
			if err != nil {
				continue
			}
			if len(pending) > 0 && !prompted {
				fmt.Fprintf(out, "\n📱 %s wants to connect. Type the code shown on it: ", pending[0])
				prompted = true
			}
		}
	}
}

// readLines forwards newline-delimited input to lines until EOF. A
// blocked Read on os.Stdin can't be canceled, but the process exits at
// that point anyway.
func readLines(in io.Reader, lines chan<- string) {
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		lines <- scanner.Text()
	}
	close(lines)
}
