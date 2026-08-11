package sharetunnel

import (
	"fmt"
	"os/exec"
)

// BinaryName is the tunnel client `clank preview --share` shells out to.
const BinaryName = "cloudflared"

// FindBinary locates cloudflared on PATH. The error carries install
// guidance so --share can fail fast, before any preview work starts.
func FindBinary() (string, error) {
	path, err := exec.LookPath(BinaryName)
	if err != nil {
		return "", fmt.Errorf("--share needs %s on PATH; install it with `brew install cloudflared` (other platforms: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/)", BinaryName)
	}
	return path, nil
}
