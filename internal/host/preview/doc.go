// Package preview spawns and supervises per-worktree development
// servers (today: Expo's Metro) inside the sprite and reverse-proxies
// their HTTP back through clank-host.
//
// The goal is the "▶ Preview app" affordance in clank-mobile: tapping
// it brings up a running dev server in the user's sprite without a
// laptop in the loop. Only Expo is detected and exposed in v1 — other
// frameworks are explicit out-of-scope (we'd want a config schema
// first; see plans/i-like-this-plan-happy-biscuit.md).
//
// Lifetime: Manager is constructed once per host.Service. It holds an
// in-memory map of running servers keyed by worktree ID; sprite
// hibernation kills the children and Manager respawns on the next
// /start request. No persistent state.
//
// Mostly "instant legacy code", but let's try it. Long term we should
// probably provide a thin auth proxy verifying the JWT and just use
// the sandbox platform's native hole-punching mechanism — that lets
// Metro be exposed on its own URL with auth at the edge, avoiding the
// in-host reverse proxy and the manifest URL rewriter. Today's design
// is the right trade with one URL per sprite and no edge auth surface.
package preview
