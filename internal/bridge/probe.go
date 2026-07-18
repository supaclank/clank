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

// ProbeHandler answers the phone's identity challenge: the ONLY
// unauthenticated route on the bridge listener. The phone sends a
// random nonce and gets HMAC(identity_key, nonce) back, proving this
// address is really its laptop BEFORE it ever transmits the bearer —
// a remembered IP that got reassigned (or squatted) can't answer, so
// it never sees a credential.
func ProbeHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
		if err != nil || len(nonce) != probeNonceLen {
			http.Error(w, "nonce must be exactly 16 hex-encoded bytes", http.StatusBadRequest)
			return
		}
		proof, err := Proof(store.Root(), nonce)
		if err != nil {
			http.Error(w, "proof unavailable", http.StatusInternalServerError)
			return
		}
		hostname, _ := os.Hostname()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"proof": proof,
			"name":  hostname,
		})
	})
}
