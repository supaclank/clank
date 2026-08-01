// Package preview resolves, spawns, and supervises per-worktree development
// servers on a Clank host.
//
// Expo retains its built-in detection and bootstrap. Arbitrary web servers use
// the strict launch configuration loaded by internal/launchconfig; project
// discovery and command selection happen once in a connected-agent setup task,
// not in this package.
//
// Manager keeps running processes keyed by (worktree ID, configured service
// name). Start allocates a loopback port, exports it as PORT, spawns the command,
// and returns StateStarting while an HTTP probe transitions it to Ready or
// Failed. Stop, idle reaping, and Shutdown terminate the process group.
//
// When configured, GWClient registers the selected service and internal port
// with the cloud gateway. The gateway owns multi-tenant authentication and the
// external reverse-proxy route; clank-host never proxies preview traffic.
package preview
