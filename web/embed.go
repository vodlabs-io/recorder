// Package web embeds the built browser Web UI (the Vite output in dist/) so the
// daemon can serve it itself. Run `bun run web:build` to produce it before
// building the daemon; a checkout without that build serves a placeholder page.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist returns the built Web UI rooted at its index.html, or nil when this
// build has none: dist/ holds only a placeholder so the package compiles.
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
