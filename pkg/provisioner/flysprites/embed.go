package flysprites

import _ "embed"

// clankHostBinary is the linux/amd64 clank-host the provisioner pushes
// into a Sprite. A gitignored build artifact: every build regenerates it
// from the checked-out source via `make embed-host`, so the embedded
// binary can't drift behind the gateway (the apply/protocol mismatch this
// guards against). Embedded (not downloaded) so self-hosters need no
// external release infrastructure.
//
//go:embed clank-host-linux-amd64
var clankHostBinary []byte
