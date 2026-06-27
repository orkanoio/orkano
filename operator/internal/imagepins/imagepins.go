// Package imagepins verifies that every product image the operator hardcodes is
// pinned to a multi-arch image index, not a single-platform manifest — a stray
// single-platform pin would silently break one of Orkano's two supported
// architectures (PLANNING: "amd64 and arm64 from day one"). The Docker-free
// manifest parsing lives here and is unit-tested; the live registry check
// (docker buildx imagetools inspect) is the build-tagged registry_test.go, run
// by `make verify-image-pins` in its own CI job (off the make all / make test
// path, so it never adds registry traffic to the normal suite).
package imagepins

import (
	"encoding/json"
	"fmt"
)

// The two index media types: an OCI image index and the older Docker manifest
// list. Either is a valid multi-arch pin; a plain manifest type is not.
const (
	ociIndexMediaType   = "application/vnd.oci.image.index.v1+json"
	dockerListMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// requiredPlatforms are the os/arch pairs every product image must carry.
var requiredPlatforms = []string{"linux/amd64", "linux/arm64"}

type rawIndex struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

// VerifyMultiArch parses the raw manifest JSON (as printed by
// `docker buildx imagetools inspect <ref> --raw`) and returns an error unless it
// is a multi-arch index covering every requiredPlatforms entry.
func VerifyMultiArch(raw []byte) error {
	var idx rawIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if idx.MediaType != ociIndexMediaType && idx.MediaType != dockerListMediaType {
		return fmt.Errorf("not a multi-arch index: mediaType %q must be an OCI image index or a Docker manifest list", idx.MediaType)
	}
	have := make(map[string]bool, len(idx.Manifests))
	for _, m := range idx.Manifests {
		have[m.Platform.OS+"/"+m.Platform.Architecture] = true
	}
	for _, p := range requiredPlatforms {
		if !have[p] {
			return fmt.Errorf("index is missing required platform %s", p)
		}
	}
	return nil
}
