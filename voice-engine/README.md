# voice-engine

`clank-voice` is the local dictation engine behind `clank preview`'s
push-to-talk: sherpa-onnx running Silero VAD + NVIDIA Parakeet
(`parakeet-tdt-0.6b-v3-int8`) — the exact stack clank-mobile runs
on-device — as a long-lived subprocess that clank's `internal/webpreview`
drives over stdin/stdout.

## Why a separate module

The sherpa-onnx Go bindings are cgo and vendor per-platform onnxruntime
libraries. Keeping them out of the main clank module means:

- `go build ./...`, CI, and the `CGO_ENABLED=0` cross-built fly.io
  `clank-host` artifact never touch cgo or download ~400 MB of libs.
- clank drives whatever `clank-voice` it finds (next to the `clank`
  binary, then `$PATH`) — the dependency is opt-in at runtime.

Do not add this module to a `go.work` at the repo root: that would pull
the cgo dependency back into every root build.

## Build & install

    brew install supaclank/tap/clank-voice   # macOS; source build via the tap

or from a checkout (`make voice` at the repo root, or):

    cd voice-engine
    go build -o $(dirname $(which clank))/clank-voice ./cmd/clank-voice

The tap formula copies the sherpa-onnx dylibs out of the Go module cache
and rewrites the binary's rpath, because the bindings link them via an
absolute `${SRCDIR}` rpath — a prebuilt clank-voice can't be shipped as
a Homebrew cask.

Model files (~670 MB, one-time) are auto-downloaded by `clank preview`
into `$CLANK_DIR/models/parakeet-tdt-0.6b-v3-int8` on first voice use.

## Protocol

stdin: `[1-byte type][4-byte LE length][payload]` frames — 0 = PCM
(s16le/16kHz/mono), 1 = end of utterance, 2 = cancel. stdout: one JSON
object per line: `ready`, `partial` (cumulative text), `final`, `error`.
Mirrored in `internal/webpreview/protocol.go` — keep the copies in sync
by hand; the modules are deliberately independent.
