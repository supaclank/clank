package images

import "io"

// SetEntropyForTest injects a custom entropy reader so tests can exercise the
// ulid.New error path without touching the global crypto/rand reader.
func (s *Server) SetEntropyForTest(r io.Reader) { s.entropySource = r }
