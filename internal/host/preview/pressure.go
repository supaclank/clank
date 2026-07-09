// pressure.go — one-line kernel PSI snapshot for preview lifecycle
// logs. I/O pressure on the host is the recurring suspect when
// spawn→ready runs long or the gateway's tunnel dials time out;
// stamping it into the lines we already log makes the correlation
// readable straight off a log pull.
package preview

import (
	"os"
	"strings"
)

// ioPressurePath is the kernel's PSI file for block I/O. Var (not
// const) so tests can point it at a fixture; absent on non-Linux,
// which degrades to an empty suffix.
var ioPressurePath = "/proc/pressure/io"

// ioPressureSuffix returns ` io_psi="some …; full …"` from
// /proc/pressure/io, or "" when the file is unreadable (non-Linux,
// restricted /proc). Never errors — this is log garnish, not data.
func ioPressureSuffix() string {
	b, err := os.ReadFile(ioPressurePath)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	trimmed := make([]string, 0, len(lines))
	for _, l := range lines {
		// Drop the cumulative "total=…" counter — only the moving
		// averages read meaningfully in a single log line.
		if i := strings.Index(l, " total="); i >= 0 {
			l = l[:i]
		}
		trimmed = append(trimmed, strings.TrimSpace(l))
	}
	if len(trimmed) == 0 {
		return ""
	}
	return ` io_psi="` + strings.Join(trimmed, "; ") + `"`
}
