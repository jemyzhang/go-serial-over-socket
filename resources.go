package resources

import "embed"

// Embed web resources into the binary
//
//go:embed web/templates/*.html
var TemplatesFS embed.FS

//go:embed web/static
var StaticFS embed.FS
