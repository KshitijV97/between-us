// Package webui embeds the built React app (web/dist) into the binary so the
// local profile ships as a single file (ADR-0002).
//
// The Makefile copies web/dist into ./dist before `go build`. The directory is
// committed with only a .gitkeep so the package always compiles; in that case
// Built reports false and the server shows a "UI not built" page instead.
package webui

import (
	"embed"
	"io/fs"
)

// dist holds the static assets. The all: prefix keeps the .gitkeep placeholder
// so the embed pattern matches even before the UI has been built.
//
//go:embed all:dist
var dist embed.FS

// FS returns the embedded assets rooted at the dist directory.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: "dist" is a literal that the embed directive guarantees.
		panic(err)
	}
	return sub
}

// Built reports whether a production UI build is embedded.
func Built() bool {
	_, err := fs.Stat(FS(), "index.html")
	return err == nil
}
