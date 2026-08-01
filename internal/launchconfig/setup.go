package launchconfig

import "fmt"

// SetupPrompt returns the task a connected agent can use to create and verify
// a launch configuration. The user still chooses where the file is stored.
func SetupPrompt(paths Paths) string {
	return fmt.Sprintf(`Set up a Clank web preview for the project at %q.

First ask whether the configuration should be shared in the repository or kept on this Clank host:
- shared project file: %q
- host-only file: %q

Then:
1. Inspect the repository, its documentation, package scripts, lockfiles, Makefiles, and monorepo structure to identify its web development servers. You may use .claude/launch.json as a hint if present, but generate and own Clank's configuration independently.
2. Do not use Docker or Podman; Clank does not provide a container runtime.
3. Write strict YAML in this shape:

default: web-app
previews:
  web-app:
    directory: web
    command: npm run dev -- --host 127.0.0.1 --port "$PORT"
    ready:
      path: /
      expect: optional response substring

Every field shown is required except ready.expect. Each directory is relative to the project root and must remain inside it. Each command runs through sh -c from that directory, must consume the PORT environment variable, and should bind the development server to 127.0.0.1 rather than a LAN-facing address. Prefer a normal one-line command. If dependencies must be installed, use the project's existing lockfile and immutable/frozen install mode; do not create or rewrite lockfiles.
4. Start each configured command with an available PORT, verify its readiness path returns HTTP 200 and contains ready.expect when set, stop it, and correct the configuration if verification fails.
5. Report the written path and a concise summary of the verified previews.

Do not commit, push, or open a pull request unless the user explicitly asks.`, paths.ProjectRoot, paths.Project, paths.Host)
}
