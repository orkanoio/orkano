// Package web embeds the dashboard's single-page app so the Go binary serves
// the UI with no separate web server and no Node runtime in production
// (ADR-0008). The React/Vite build lands in dist/ in M2.6; until then dist holds
// a placeholder so the binary builds and the SPA-serving path is exercised.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets returns the embedded SPA file tree rooted at dist/. A failure to
// sub-root is a build-time bug (dist is embedded above), so it panics rather
// than burdening every caller with an impossible error.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("dashboard/web: embedded dist subtree missing: " + err.Error())
	}
	return sub
}
