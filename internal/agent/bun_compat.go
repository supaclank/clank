package agent

// PinnedBunVersion is the bun version the host image installs and uses
// to install the pinned agent CLIs. bun is infra (installer + JS
// runtime for the CLIs), pinned here so image builds read it from
// source alongside the CLI pins — see `clank-host print-pins`.
const PinnedBunVersion = "1.3.14"
