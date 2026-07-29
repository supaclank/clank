package agent

// PinnedOpencodeVersion is the opencode version clank ships against on
// provisioned hosts, and the verified-surface floor for `opencode acp`
// on laptops (the ACP manager refuses older binaries with an upgrade
// hint). Bumping this constant is a deliberate, reviewable change — it
// determines what every fly.io provisioner installs onto a host (and
// what `clank-host print-pins` reports).
//
// Bumping this:
//  1. Update the constant (and re-verify the `opencode acp` surface).
//  2. `make install` — laptops get the new clank that knows the new pin.
//  3. Sprites probe-and-reinstall on next EnsureHost (~30-90s one-shot cost).
//  4. Laptops below the floor see the upgrade hint at first opencode use.
const PinnedOpencodeVersion = "1.17.18"

// OpencodeVersionAtLeast reports whether version v is >= floor. Used by
// the ACP path to gate `opencode acp` on a verified-surface floor.
// Returns an error when either version fails to parse.
func OpencodeVersionAtLeast(v, floor string) (bool, error) {
	return versionAtLeast(v, floor)
}
