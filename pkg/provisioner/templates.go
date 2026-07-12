package provisioner

import "encoding/json"

// Template is one operator-configured ("builtin") entry of the
// create-project catalog, passed to a provider via its Options. The
// provider forwards these to the sandbox's clank-host, which serves
// them from GET /templates merged with the user's own GitHub template
// repos.
//
// This is the control-plane-side type; it is the wire-compatible pair
// of internal/host.Template (the sandbox-side parse target). A
// template's identity is its clone URL — there is no id.
//
// Env-var config is a caller concern: a daemon (clankd, or an
// embedder's binary) that wants CLANK_TEMPLATES-style config unmarshals
// the JSON into []Template itself, then passes the strong type here.
// The library API stays typed.
type Template struct {
	DisplayName string `json:"display_name"`
	CloneURL    string `json:"clone_url"`
}

// TemplatesEnvValue marshals a catalog to the JSON string clank-host
// reads from CLANK_TEMPLATES / --templates-json. Empty catalog → "".
// json.Marshal of []Template (strings only) cannot fail, so the shape
// is total; providers call this when building the sandbox env/args.
func TemplatesEnvValue(templates []Template) string {
	if len(templates) == 0 {
		return ""
	}
	b, _ := json.Marshal(templates)
	return string(b)
}
