//go:build imagepins

package controller

import "slices"

// PinnedPostgresImages returns the digest-pinned Postgres image for every
// supported version (sorted for stable subtest ordering), for the multi-arch
// image-pin guard (operator/internal/imagepins). Build-tagged so it adds no
// surface to normal builds — its only caller is the imagepins registry check,
// compiled under the same tag.
func PinnedPostgresImages() []string {
	out := make([]string, 0, len(postgresImages))
	for _, img := range postgresImages {
		out = append(out, img)
	}
	slices.Sort(out)
	return out
}
