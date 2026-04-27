package server

import (
	"embed"
	"io/fs"
)

//go:embed all:wwwroot
var embeddedWWW embed.FS

// staticFS returns a filesystem rooted at the wwwroot directory inside the
// embedded assets, so paths read as "index.html" rather than "wwwroot/index.html".
func staticFS() fs.FS {
	sub, err := fs.Sub(embeddedWWW, "wwwroot")
	if err != nil {
		return embeddedWWW
	}
	return sub
}
