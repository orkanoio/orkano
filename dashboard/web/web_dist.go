//go:build webdist

package web

import "embed"

// The real UI: Vite's build output, produced into dist/ by `make web`. The
// directory is gitignored — anything shipped to a user must run the web build
// first (goreleaser's before hook and the CI web job both do).
//
//go:embed all:dist
var assets embed.FS

const assetsRoot = "dist"
