// Package web embeds the dashboard's single-page app so the Go binary serves
// the UI with no separate web server and no Node runtime in production
// (ADR-0008).
//
// Two embed modes, selected by the webdist build tag (the imagepins pattern:
// tagged files are invisible to make lint/test, so `make verify-web` builds
// and vets them explicitly). Release and CI builds pass -tags webdist and
// embed dist/ — the Vite output `make web` produces, gitignored and never
// committed — while a plain `go build` embeds a committed placeholder page so
// a fresh clone compiles and tests with no Node toolchain.
package web

import "io/fs"

// Assets returns the embedded SPA file tree. A failure to sub-root is a
// build-time bug (the tree is embedded by web_dist.go / web_placeholder.go),
// so it panics rather than burdening every caller with an impossible error.
func Assets() fs.FS {
	sub, err := fs.Sub(assets, assetsRoot)
	if err != nil {
		panic("dashboard/web: embedded asset subtree missing: " + err.Error())
	}
	return sub
}
