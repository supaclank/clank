package host

// Template is one operator-configured ("builtin") entry of the
// create-project catalog, injected at process start (clank-host
// --templates-json / $CLANK_TEMPLATES). The host owns the whole
// template surface — builtin entries here, the user's own GitHub
// template repos live via internal/host/github — so self-hosted and
// laptop deployments work without a gateway.
type Template struct {
	// ID is tolerated for config compatibility (older catalogs carried
	// ids) but no longer travels on the wire: a template's identity is
	// its clone URL.
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"display_name"`
	CloneURL    string `json:"clone_url"`
}

// Templates returns the operator-configured builtin templates.
func (s *Service) Templates() []Template {
	return s.templates
}
