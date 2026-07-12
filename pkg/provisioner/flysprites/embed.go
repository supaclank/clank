package flysprites

import _ "embed"

// clankHostBinary is the linux/amd64 clank-host the provisioner pushes
// into a Sprite. TRACKED in VCS (the deploy embeds whatever is committed)
// and verified current by CI, which rebuilds it via `make embed-host` and
// fails on any drift — so a stale binary can't ship onto sprites while the
// gateway moves ahead (the apply/protocol mismatch this guards against).
// Rebuild + commit it alongside clank-host changes. Embedded (not
// downloaded) so self-hosters need no external release infrastructure.
//
//go:embed clank-host-linux-amd64
var clankHostBinary []byte
