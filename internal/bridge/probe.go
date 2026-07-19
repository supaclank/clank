package bridge

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
)

// probeNonceLen is the exact nonce size a probe must present —
// mirrored in clank-mobile's bridgeProbe.
const probeNonceLen = 16

// ProbeHandler answers the phone's identity challenge: an
// unauthenticated route on the bridge listener. The phone sends a
// random nonce and gets the host key's signature over it, proving this
// address is really its laptop BEFORE it ever transmits anything — a
// remembered IP that got reassigned (or squatted) can't answer, so the
// phone walks away.
func ProbeHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
		if err != nil || len(nonce) != probeNonceLen {
			http.Error(w, "nonce must be exactly 16 hex-encoded bytes", http.StatusBadRequest)
			return
		}
		hostname, _ := os.Hostname()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sig":  EncodeSig(store.SignNonce(nonce)),
			"name": hostname,
		})
	})
}
