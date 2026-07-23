package agent

import "errors"

// ErrUnsupported marks an operation a backend does not implement (e.g.
// fork on a backend without fork support). The HTTP layer maps it to 501
// with code "unsupported" so clients can degrade gracefully instead of
// treating it as an internal error.
var ErrUnsupported = errors.New("operation not supported by this backend")
