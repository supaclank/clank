package clankcli

// First-run connect: an entry point that is about to need an agent asks
// once, up front, when this machine has no agent signed in at all. The
// preview overlay's whole point is summoning an agent, so discovering
// there is none only when you press the hotkey is the wrong order.

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
)

// connectProviderListTimeout caps the catalog read that decides whether
// to ask. The local host answers in milliseconds; a slow read must not
// stall an entry point that would otherwise work fine.
const connectProviderListTimeout = 10 * time.Second

// agentConnectState is what an entry point knows about this machine's
// agents after the first-run check. Distinguishing "we asked and there
// is none" from "we couldn't ask" keeps callers from warning about a
// state they never actually observed.
type agentConnectState int

const (
	// agentConnectUnknown means the provider catalog couldn't be read.
	agentConnectUnknown agentConnectState = iota
	agentConnected
	agentNotConnected
)

// ensureAgentConnected offers the connect flow when no backend on this
// machine has a connected provider, and reports the state it leaves
// behind.
//
// backendFlag, when set, scopes the offer to that backend — the caller
// already said which agent they want, so asking again is noise.
//
// Nothing here blocks the caller: a non-interactive terminal, a catalog
// that can't be read, and a declined prompt all return without an error.
func ensureAgentConnected(ctx context.Context, client *daemonclient.Client, backendFlag agent.BackendType, in io.Reader, out io.Writer) agentConnectState {
	listCtx, cancel := context.WithTimeout(ctx, connectProviderListTimeout)
	defer cancel()
	providers, err := client.Host(host.HostLocal).ListAuthProviders(listCtx, "")
	if err != nil {
		// Say nothing: whatever broke the host will surface through the
		// caller's own work with a far better error than a guess here.
		return agentConnectUnknown
	}
	if agent.IsAnyProviderConnected(providers) {
		return agentConnected
	}

	fmt.Fprintln(out, "No coding agent is connected on this machine yet.")
	if !isInteractiveTerminal(in, out) {
		fmt.Fprintf(out, "To use one, %s.\n", connectHint)
		return agentNotConnected
	}

	result, err := showConnectUI(ctx, client, backendFlag, in, out)
	if err != nil {
		fmt.Fprintf(out, "Could not connect an agent: %v\n", err)
		return agentNotConnected
	}
	if !result.IsConnected {
		fmt.Fprintf(out, "Continuing without an agent — %s any time.\n", connectHint)
		return agentNotConnected
	}
	fmt.Fprintf(out, "Connected %s.\n", result.Backend)
	return agentConnected
}

// offerPreviewAgentConnect is `clank preview`'s first-run hook. It maps
// the --backend flag onto the connect offer and errors only on a flag
// value that could never resolve — an unconnected agent is a warning,
// not a reason to refuse to serve the app.
func offerPreviewAgentConnect(ctx context.Context, client *daemonclient.Client, backendFlag string, in io.Reader, out io.Writer) error {
	var backend agent.BackendType
	if backendFlag != "" {
		parsed, err := agent.ParseBackend(backendFlag)
		if err != nil {
			return err
		}
		backend = parsed
	}
	if ensureAgentConnected(ctx, client, backend, in, out) == agentNotConnected {
		fmt.Fprintln(out, "The preview still serves your app; its agent hotkey won't work until then.")
	}
	return nil
}
