//go:build !webdist

package web

import "embed"

// Node-free builds embed a committed placeholder page instead of the Vite
// output, so `go build ./...` and `go test ./...` work on a fresh clone
// without a JS toolchain. Only dev builds ever see it: goreleaser passes
// -tags webdist, and the placeholder page itself says how to get the real UI.
//
//go:embed all:placeholder
var assets embed.FS

const assetsRoot = "placeholder"
