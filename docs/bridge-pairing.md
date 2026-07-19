# Bridge Pairing Specification

**Status:** draft · **Version:** `v1` (wire), `clank-sas-v1` / `clank-sig-v1` (crypto transcripts)

This document specifies how a phone (the clank mobile app) pairs with, and then
authenticates to, a laptop's `clankd` daemon over the local network — the
"bridge". It is the authority for the wire format and the cryptographic
constructions; the code is the reference implementation, cited under **Golden**.

The bridge is a **laptop-mode-only** surface. Cloud/self-hosted (TCP) daemons
have no bridge — no listeners, no `bridge.json`, no pairing (see
`internal/cli/daemoncli/bridge_runtime.go`, `setupBridge`).

---

## 1. Model

The bridge borrows **SSH's trust model**, expressed in web-native parts. Nothing
secret ever crosses the wire.

| SSH | Bridge |
|---|---|
| Host key + `known_hosts` | The laptop's Ed25519 **host key**; its public half rides the QR (`hk`) and the phone pins it. |
| `ssh-keygen` on the client | The phone mints its own Ed25519 **device key** on first use. |
| `authorized_keys` | The daemon's **device registry** — one approved phone public key per line. |
| Client public-key auth | The phone **signs every request** with its device key. |
| Host-key verification | The **identity probe**: the laptop signs the phone's nonce; the phone checks it against `hk`. |
| Deleting an `authorized_keys` line | Per-device **revoke**. |
| The encrypted channel | **Out of scope for v1** — see [§10](#10-non-goals--future-work). Confidentiality is delegated to the transport (Tailscale/WireGuard on the tailnet; plain HTTP on a consented LAN). |

- **[PAIR-001] (MUST)** The bridge MUST NOT transmit any secret (root secret,
  bearer, private key) during pairing or thereafter. Authentication is by
  signature; enrollment records a **public** key. **Why:** a passive sniffer on
  a plain LAN, or a leaked `bridge.json`, must never yield a credential that
  impersonates a phone. **Golden:** `internal/bridge/sas.go`,
  `internal/bridge/sign.go`.

---

## 2. Keys and storage

### 2.1 Host key (laptop)

- **[PAIR-010] (MUST)** The daemon MUST hold exactly one Ed25519 **host
  keypair**. The 32-byte seed is the only secret in `bridge.json`, stored at
  `0600`, plaintext at rest (the daemon must sign probe challenges with it — the
  ssh-host-key posture; a leaked file is recovered by rotating the key, which
  forces every phone to re-pair). **Golden:** `internal/bridge/store.go`
  (`mintHostKeyLocked`, `HostPublicKey`, `SignNonce`).
- **[PAIR-011] (MUST)** `bridge.json` from the retired shared-secret model MUST
  migrate on load: the old `secret` is dropped, network-trust consents survive,
  and a host key is minted. **Golden:** `store.go` (`OpenStore`),
  `TestOpenStoreMigratesSharedSecretFile`.

### 2.2 Device key (phone)

- **[PAIR-012] (MUST)** The phone MUST mint one Ed25519 **device keypair** on
  first use and store the seed in the platform secure store (Keychain /
  Keystore), never in app-readable storage, never logged, never transmitted.
  **Golden:** `clank-mobile/src/lib/deviceKey.ts` (`getDeviceKeypair`).

### 2.3 Registry (laptop)

- **[PAIR-013] (MUST)** The daemon MUST keep a **device registry** in
  `bridge.json`: an array of `{pubkey, name, added_at, last_seen}`. `pubkey`
  (base64url Ed25519) is the identity; `name` is cosmetic attribution and MUST
  NOT gate any decision. **Golden:** `internal/bridge/devices.go`.

### 2.4 Phone-side pairing slot

- **[PAIR-014] (MUST)** The phone's stored record of a paired laptop MUST
  contain only public data: the laptop's host public key, the candidate
  addresses, and a display label. It MUST NOT contain any secret. A slot whose
  host key does not parse is treated as unpaired and dropped. **Golden:**
  `clank-mobile/src/store/settings.ts` (`PairingSlot`, `validHostKey`).

---

## 3. The QR link (`clank://link`)

The laptop renders a QR encoding a URL. It is **entirely public** — safe to
screen-share, photograph, or leave in terminal scrollback.

```
clank://link?v=1
  &gw=<primary gateway base URL>          (required)
  &alt=<comma-separated fallback URLs>    (optional)
  &hk=<host public key, base64url>        (required)
  &name=<laptop display name>             (optional)
  &url=<Metro dev-server URL>             (preview only)
  &wid=<preview key>                      (preview only)
  &lp=<laptop folder path>               (preview only)
  &bk=<agent backend>                     (preview only)
  &sid=<session id to attach>             (preview only)
```

- **[PAIR-020] (MUST)** The link MUST be a URL (not JSON). **Why:** a URL is
  routable by the operating system, so the QR can drive the app from the system
  camera. **Golden:** `internal/cli/clankcli/preview_link.go`.
- **[PAIR-021] (MUST)** `gw` and `hk` are REQUIRED; `Encode` MUST error if
  either is absent, and `ParsePreviewLink` MUST reject a link missing either.
  **Why:** a link with no gateway names nowhere to reach; a link with no host
  key cannot be verified against a probe, so the phone could not tell the real
  laptop from an impostor. **Golden:** `preview_link.go` (`Encode`,
  `ParsePreviewLink`), `TestParsePreviewLinkRejectsForeignScheme`.
- **[PAIR-022] (MUST)** The link MUST NOT carry any secret param. A round-trip
  test MUST assert no `tok=` (or equivalent) is ever emitted. **Golden:**
  `TestPreviewLinkCarriesNoSecret`.
- **[PAIR-023] (MUST)** Both the in-app scanner and the operating-system camera
  MUST route a scanned `clank://link` into the pairing flow, sharing one
  implementation. **Why:** the QR's URL form exists precisely so a system-camera
  scan works without opening the app first ([PAIR-020]); a link that only the
  in-app scanner handles defeats that. The signed-out → sign-in guard MUST
  exempt the pairing route, since pairing can itself establish the session
  ([PAIR-014] — a paired gateway has no static token, so "paired" is the
  signed-in state). **Golden:** `clank-mobile/app/link.tsx` (deep-link route),
  `clank-mobile/app/scan.tsx` (camera), `clank-mobile/src/lib/bridgeLink.ts`
  (shared parse), `clank-mobile/app/_layout.tsx` (`AuthGate` exemption).

---

## 4. The identity probe

Before a returning phone sends anything, and before a new phone pairs, it
verifies the address really belongs to its laptop.

```
GET {base}/bridge/ping?nonce=<16 random bytes, hex>
→ 200 { "sig": <base64url Ed25519 sig over the nonce>, "name": <hostname> }
```

- **[PAIR-030] (MUST)** `/bridge/ping` is unauthenticated and MUST answer with
  the host key's signature over the exact nonce bytes. The nonce MUST be exactly
  16 bytes; other lengths MUST 400. **Golden:** `internal/bridge/probe.go`.
- **[PAIR-031] (MUST)** The phone MUST verify the signature against the `hk` it
  learned from the QR (for a new laptop) or its stored host key (returning)
  **before** trusting the address, and MUST walk candidate addresses best-first,
  taking the first that verifies. An address that cannot answer (reassigned IP,
  squatter, different laptop) is silently skipped. **Golden:**
  `clank-mobile/src/lib/bridgeProbe.ts` (`probeLaptop`),
  `clank-mobile/src/lib/bridgeConnect.ts`.

---

## 5. The pairing ceremony (SAS handshake)

A brand-new phone — one whose device key the laptop does not yet trust — enrolls
through a **commit-then-reveal Short Authentication String** handshake. The
6-digit code the user reads to the laptop *authenticates which device key is
enrolled*; it does not merely select an attempt. This closes the active
man-in-the-middle hole at pairing without a TLS pin.

### 5.1 Sequence

```
Phone                          clankd (bridge listener)         clank pair / preview (terminal)
  │  scan QR (addresses + hk)                                          │  QR up, window leased by polling
  │  probe hk  ───────────────────────►  verify §4                     │
  │                                                                    │
  │  commit = H(device_pub ‖ nonce_P)                                  │
  │  POST /bridge/pair/begin {name, commit} ─►  open attempt,          │
  │                                             pick nonce_D           │
  │  ◄─ {attempt_id, nonce_D, reply_sig}     sign (id‖commit‖nonce_D)  │
  │  VERIFY reply_sig against hk ✗→abort                               │
  │                                                                    │
  │  POST /bridge/pair/reveal {device_pub, nonce_P} ─► check commit,   │
  │  ◄─ {ok}                                          derive SAS       │
  │                                                                    │
  │  derive SAS, DISPLAY 6 digits ──────────── user reads them ───────►│  "type the code shown on the phone"
  │                                            approve matching  ◄──── POST /v1/bridge/pair/complete {code}
  │  GET /bridge/pair/attempt?id ─► approved   attempt, record key      (unix socket, physically local)
  │  first signed request ────────────────►  authenticates             │
```

### 5.2 Messages

**Begin** (pre-auth, on the bridge listener):

```
POST {base}/bridge/pair/begin   { "device": <name>, "commit": <hex> }
→ 200 { "attempt_id": <32 hex>, "nonce_d": <16 bytes hex>, "reply_sig": <base64url> }
→ 409 window closed  ·  429 too many pending / locked out  ·  400 bad commit
```

**Reveal** (pre-auth):

```
POST {base}/bridge/pair/reveal  { "attempt_id": <hex>, "device_pub": <base64url>, "nonce_p": <hex> }
→ 200 { "ok": "true" }   ·   400 commit mismatch / bad input   ·   404 unknown attempt
```

**Attempt poll** (pre-auth):

```
GET {base}/bridge/pair/attempt?id=<attempt_id>
→ 200 { "state": "pending" | "approved" | "expired" }
```

**Window lease + approve** (admin, **unix socket only** — physically local):

```
POST /v1/bridge/pair/poll      → { "pending": [ <device names of REVEALED attempts> ] }
POST /v1/bridge/pair/complete  { "code": <typed SAS> }  → { "device": <name> } | { "error": … }
```

- **[PAIR-040] (MUST)** `begin` MUST be refused unless a CLI is currently leasing
  the window (a `poll` within the lease). **Why:** pairing is only possible while
  the human is deliberately showing the QR. **Golden:** `internal/bridge/pairing.go`
  (`Begin`, `RefreshWindow`), `TestPairingWindowGating`.
- **[PAIR-041] (MUST)** The daemon MUST sign the `begin` reply with the host key
  over the canonical reply string ([§7.2](#72-transcripts)) and the phone MUST
  verify it against `hk` before revealing or displaying anything. **Why:** this
  authenticates the daemon→phone direction and — because the signature covers the
  phone's `commit` — forces any interposer to relay the real commit unchanged.
  **Golden:** `store.go` (`SignSASReply`), `bridgeCrypto.ts` (`verifySASReply`).
- **[PAIR-042] (MUST)** On `reveal`, the daemon MUST recompute the commit from
  the revealed `device_pub` + `nonce_p` and reject (burning the attempt) if it
  does not equal the stored commit. **Why:** collision resistance binds the
  enrolled key — an interposer cannot open the commit to a substituted key.
  **Golden:** `pairing.go` (`Reveal`), `TestPairingRevealMustOpenCommit`.
- **[PAIR-043] (MUST)** Both sides MUST derive the SAS from the full transcript
  ([§7.2](#72-transcripts)). The phone MUST display it and MUST NOT transmit it.
  **Golden:** `sas.go` (`DeriveSAS`), `bridgeCrypto.ts` (`deriveSAS`),
  `bridgePair.test.ts` ("never sends the SAS on the wire").
- **[PAIR-044] (MUST)** `complete` MUST approve the revealed attempt whose
  derived SAS equals the typed code, recording its `device_pub` in the registry
  **before** returning success. **Why:** an approval the registry never saw would
  leave the phone signing into 401s. **Golden:** `pairing.go` (`Complete`).
- **[PAIR-045] (MUST)** If a typed code matches **more than one** revealed
  attempt, the daemon MUST expire all matching attempts and refuse (the phones
  rescan). **Why:** a derived SAS cannot be redrawn to break a tie the way a
  random code could. **Golden:** `pairing.go` (`Complete`, `ErrPairAmbiguous`),
  `TestPairingAmbiguousSASExpiresBoth`.
- **[PAIR-046] (MUST)** Only **revealed** attempts are promptable: `poll` MUST
  return names of, and `complete` MUST match against, attempts past reveal only.
  **Why:** before reveal there is no SAS for the user to type. **Golden:**
  `pairing.go` (`promptable`), `TestPairingOnlyPromptableAfterReveal`.
- **[PAIR-047] (MUST)** Approval MUST carry no payload; the poll response on
  approval is `{"state":"approved"}` with no secret. The phone's own signed
  request is what proves the new trust. **Golden:** `TestBridgePairingCeremony`.

### 5.3 Returning phones

- **[PAIR-048] (MUST)** A phone that already holds a pairing slot for the scanned
  laptop (its stored host key equals the QR's `hk`) MUST skip the ceremony
  entirely: probe the candidates, and on success reconnect. Only a phone with no
  matching slot runs [§5.1](#51-sequence). **Golden:** `bridgeConnect.ts`
  (`connectFromLink`, the `pair-needed` vs instant paths).

---

## 6. Per-request authentication

Once enrolled, the phone authenticates **every** bridge request by signing it.
There is no bearer, no session, no server-side auth state beyond a short nonce
cache.

```
X-Clank-Key:    <device public key, base64url>
X-Clank-Ts:     <unix seconds>
X-Clank-Nonce:  <16 random bytes, hex>
X-Clank-Sig:    <base64url Ed25519 sig over the canonical request>
```

- **[PAIR-050] (MUST)** The daemon MUST accept a request iff: the key is in the
  registry, the timestamp is within ±2 minutes of the daemon clock, the nonce is
  unseen (replay cache), and the signature verifies over the canonical request
  ([§7.2](#72-transcripts)). Every failure MUST be an indistinguishable `401`.
  **Why:** an unpaired prober must learn nothing about which check tripped.
  **Golden:** `internal/bridge/auth.go` (`Verify`).
- **[PAIR-051] (MUST)** The canonical request MUST cover method, request-target
  (path + query), and a hash of the exact body, so tampering with any of them
  breaks the signature. The verifier MUST read and restore the body for the
  downstream handler. **Golden:** `sign.go` (`CanonicalRequest`), `auth.go`.
- **[PAIR-052] (MUST)** The nonce MUST be reserved before the signature check
  (so two concurrent replays can't both pass) and MUST expire after the skew
  window closes on both sides. **Golden:** `auth.go` (`reserveNonce`).
- **[PAIR-053] (MUST)** The client MUST use a fresh nonce and current timestamp
  per request, and sign the exact bytes it sends. **Golden:**
  `clank-mobile/src/lib/deviceKey.ts` (`signedRequestHeaders`),
  `clank-mobile/src/api/client.ts` (`signRequest`).

---

## 7. Cryptographic constructions

### 7.1 Primitives

- **[PAIR-060] (MUST)** Signatures are **Ed25519**. Hashing is **SHA-256**. Keys
  and signatures on the wire are **base64url without padding**; nonces are
  **lowercase hex**. There is no ECDH, no HKDF, no bearer derivation. **Why:** the
  ceremony authenticates *data* (which public key to trust), not a channel, so no
  key agreement is needed; fewer primitives, fewer ways to diverge.

### 7.2 Transcripts

All transcripts are newline-joined ASCII fields, hashed or signed verbatim.
`b64(k)` is base64url-nopad of a 32-byte key; `hex(n)` is lowercase hex.

```
Request signature   (clank-sig-v1):
  "clank-sig-v1" ‖ ts ‖ nonce_hex ‖ METHOD ‖ request_uri ‖ hex(sha256(body))

Pairing commit      (clank-sas-v1):
  H = sha256("clank-sas-v1" ‖ "commit" ‖ b64(device_pub) ‖ hex(nonce_P))
  commit = hex(H)

Pairing reply sig   (clank-sas-v1):
  sig = Ed25519_sign(host_key,
        "clank-sas-v1" ‖ "reply" ‖ attempt_id ‖ commit ‖ hex(nonce_D))

SAS                 (clank-sas-v1):
  s = sha256("clank-sas-v1" ‖ "sas" ‖ attempt_id ‖ commit ‖ hex(nonce_D)
             ‖ b64(device_pub) ‖ hex(nonce_P) ‖ b64(host_pub))
  SAS = uint32_be(s[0:4]) mod 1_000_000, zero-padded to 6 digits
```

(`‖` denotes joining with a single `\n`.)

- **[PAIR-061] (MUST)** These transcripts are the cross-implementation contract;
  the Go and TypeScript sides MUST produce byte-identical results and MUST change
  only together, with the shared vectors ([§7.3](#73-test-vectors)) updated in the
  same change. **Golden:** `internal/bridge/sas.go`, `sign.go` ↔
  `clank-mobile/src/lib/bridgeCrypto.ts`.

### 7.3 Test vectors

Pinned identically in `internal/bridge/{sas,sign}_test.go` and
`clank-mobile/src/lib/__tests__/bridgeCrypto.test.ts`:

| Input | Value |
|---|---|
| host seed | 32 × `0x01` → pub `iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w` |
| device seed | 32 × `0x02` → pub `gTl3Dqh9F19Wo1Rmw0x-zMuNipG07jeiXfYPW4_Js5Q` |
| `nonce_P` | `10111213…1e1f` (16 bytes) |
| `nonce_D` | `20212223…2e2f` (16 bytes) |
| `attempt_id` | `00112233445566778899aabbccddeeff` |
| commit | `4f90f3d2e8c244cfa2cb176a526759748d41357c1faf417603e4f9647f4fad82` |
| reply sig | `H2RaZLBIAsDFMdbjN1mFitaatTQtZaLGirLsKSVFupEXWXtlRechPcy9u-6ZH_2hcfVrjmX3ACwsxNITW65MCA` |
| **SAS** | **`626680`** |

---

## 8. Guardrails

| Parameter | Value | Purpose |
|---|---|---|
| Window lease | 30 s after last `poll` | pairing possible only while the QR is shown |
| Attempt TTL | 2 min | a displayed code stops being claimable |
| Max pending | 3 | bounds concurrent unapproved attempts |
| Max wrong codes | 5 → 30 s lockout | typo hygiene / online-guess cap (time-based, not reset by the per-second poll) |
| Request skew | ±2 min | bounds replay-cache size; clients resync from the response `Date` |

**Golden:** `internal/bridge/pairing.go` (constants), `auth.go` (`sigSkewWindow`).

---

## 9. Native preview overlay — the one exception

The native preview window's HTTP stack holds a static bearer and cannot run the
device-key signer (the host JS context is torn down at the bundle swap). It is
the **only** bearer in the system.

- **[PAIR-070] (MUST)** A session token MUST be mintable **only** by a signed
  request (`POST /bridge/session-token`); a bearer MUST NOT mint another token.
  The token MUST be short-lived (24 h), bound to the minting device, and
  invalidated when that device is revoked. **Why:** it is a scoped, revocable,
  self-expiring capability, not a standing credential. **Golden:**
  `auth.go` (`MintSessionToken`, `verifySessionToken`),
  `bridge_runtime.go` (`sessionTokenHandler`), `TestBridgeSessionToken`.
- **[PAIR-071] (SHOULD)** If minting fails the phone SHOULD fall back to the
  overlay's read-only state rather than block the preview. **Golden:**
  `clank-mobile/app/preview/index.tsx`.

---

## 10. Revocation

- **[PAIR-080] (MUST)** `clank pair revoke <device>` MUST remove one device by
  public key; `clank pair revoke` (no arg) MUST remove all. The host key MUST
  survive either — returning phones still recognize the laptop and simply
  re-pair. **Golden:** `internal/cli/clankcli/pair.go`, `devices.go`
  (`RemoveDevice`, `RemoveAllDevices`), `TestBridgeRevokeSingleDevice`.
- **[PAIR-081] (MUST)** A revoked device's next signed request, and any session
  token it minted, MUST fail `401` immediately. **Golden:** `auth.go`,
  `TestBridgeSessionToken`.

---

## 11. Security properties & threat model

**In scope.** A LAN attacker who can reach the bridge port; a passive sniffer; an
active man-in-the-middle present during the pairing window.

**Out of scope for v1.** Channel confidentiality on an untrusted plain LAN (see
below); a compromised laptop; a compromised phone secure store.

- **Passive sniffer** gains nothing: no secret is ever sent ([PAIR-001]). It can
  read plaintext API traffic on a plain LAN (a known v1 limitation, gated behind
  an explicit "trust this network?" consent and absent entirely on the tailnet),
  but it can forge nothing — every request is signed ([PAIR-050]).
- **Active MITM at pairing** is defeated by two independent channels:
  1. The host-key **signature** on the `begin` reply authenticates daemon→phone
     and, by covering the phone's commit, forces the attacker to relay the real
     commit ([PAIR-041]); commit binding then forces the real key to be enrolled
     ([PAIR-042]).
  2. The human-typed **SAS** authenticates phone→laptop: the user types the code
     from *their* phone's screen into *their* laptop, so only their attempt's
     transcript can match.
  The SAS is thus defense-in-depth over the signature, not the sole barrier.
- **Why 6 digits suffice.** Commit-then-reveal locks each side's contribution
  before it sees the other's, so an attacker cannot grind its contribution to
  force a SAS collision after the fact. Grinding is therefore **online only** —
  one guess per `begin`, throttled by the window lease + pending cap + wrong-code
  lockout ([§8](#8-guardrails)) — against a 10⁻⁶ target, while the user is
  actively at the pairing screen. Infeasible.
- **Reassigned / squatted address** cannot harvest anything: the probe
  ([PAIR-031]) rejects it before the phone transmits, and a phone transmits only
  signatures anyway.

---

## 12. Non-goals & future work

- **Transport confidentiality on untrusted LANs.** v1 delegates encryption to the
  transport: WireGuard end-to-end on the tailnet, or plain HTTP behind an explicit
  per-network consent on a LAN. A pinned self-signed TLS front door (the QR could
  carry the certificate fingerprint alongside `hk`) is the planned next layer; it
  composes with this spec unchanged — the ceremony is the consent + enrollment
  layer, TLS is confidentiality. This is why `hk` is a public key the future cert
  can wrap, and why no format migration is needed to add it.
- **Discovery across networks.** A phone that has all candidate addresses fail
  (laptop moved networks, no tailnet) rescans the QR today. An opaque encrypted
  "mailbox" that carries only an address note is the planned freshness upgrade;
  it changes discovery, never trust.

---

## Changelog

- **v1 (2026-07)** — initial per-device trust model: Ed25519 host + device keys,
  signed requests, SAS pairing ceremony. Supersedes the retired shared-root-secret
  model (single key handed out in the QR).
