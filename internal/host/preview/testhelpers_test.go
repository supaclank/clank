//go:build unix

package preview

import "fmt"

// fakeMetroScript returns an argv that spawns a Python HTTP server
// mimicking Metro just enough for the readiness probe and the proxy
// chain. The %d placeholder in the last arg receives the port from
// renderArgs.
//
// extras is shell that runs BEFORE the python server (separated by " ; ").
// Use "(sleep 30 &)" to test process-group cleanup, "" for default.
//
// The script:
//   - listens on the allocated port
//   - responds 200 to /status with "packager-status:running"
//     (Metro's actual readiness contract)
//   - echoes the path back on every other GET as "path=<path>"
//   - serves forever (no idle exit) until killed by the test
//
// We do the threading-before-print dance from the earlier proxy test
// to ensure /status is actually accepting before the probe fires —
// otherwise the probe and the listener race under -race.
func fakeMetroScript(extras string) []string {
	if extras == "" {
		extras = "true"
	}
	const py = `python3 -u -c "
import http.server, sys, threading, time
port = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        if self.path == '/status':
            self.send_header('Content-Type', 'text/plain')
            self.end_headers()
            self.wfile.write(b'packager-status:running')
            return
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(('path=' + self.path).encode())
    def log_message(self, *args): pass
srv = http.server.HTTPServer(('127.0.0.1', port), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
while True: time.sleep(60)
" %d`
	return []string{"sh", "-c", fmt.Sprintf("%s ; %s", extras, py)}
}
