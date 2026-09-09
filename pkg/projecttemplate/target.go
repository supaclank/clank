// Package projecttemplate defines template catalog metadata shared by hosts and provisioners.
package projecttemplate

// Target identifies the app a template creates.
type Target string

const (
	Web    Target = "web"
	Mobile Target = "mobile"
)
