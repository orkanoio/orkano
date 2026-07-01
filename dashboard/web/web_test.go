package web

import (
	"io/fs"
	"testing"
)

// Runs in both embed modes: untagged it proves the committed placeholder tree
// is intact; under -tags webdist (make verify-web) it proves the Vite build
// landed where web_dist.go embeds it — guarding an outDir misconfiguration
// that would otherwise ship a UI-less release binary.
func TestAssetsContainIndexHTML(t *testing.T) {
	b, err := fs.ReadFile(Assets(), "index.html")
	if err != nil {
		t.Fatalf("index.html missing from embedded assets: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("embedded index.html is empty")
	}
}
