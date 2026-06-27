package imagepins_test

import (
	"strings"
	"testing"

	"github.com/orkanoio/orkano/operator/internal/imagepins"
)

func TestVerifyMultiArch(t *testing.T) {
	const ociIndexBoth = `{
		"mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": [
			{"platform": {"os": "linux", "architecture": "amd64"}},
			{"platform": {"os": "linux", "architecture": "arm64"}},
			{"platform": {"os": "linux", "architecture": "ppc64le"}}
		]
	}`
	const dockerListBoth = `{
		"mediaType": "application/vnd.docker.distribution.manifest.list.v2+json",
		"manifests": [
			{"platform": {"os": "linux", "architecture": "amd64"}},
			{"platform": {"os": "linux", "architecture": "arm64"}}
		]
	}`
	const singleManifest = `{"mediaType": "application/vnd.oci.image.manifest.v1+json"}`
	const missingArm = `{
		"mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": [{"platform": {"os": "linux", "architecture": "amd64"}}]
	}`
	const missingAmd = `{
		"mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": [{"platform": {"os": "linux", "architecture": "arm64"}}]
	}`
	const emptyMediaType = `{
		"manifests": [
			{"platform": {"os": "linux", "architecture": "amd64"}},
			{"platform": {"os": "linux", "architecture": "arm64"}}
		]
	}`

	tests := []struct {
		name    string
		raw     string
		wantErr string // substring; "" means accept
	}{
		{"oci index with both arches", ociIndexBoth, ""},
		{"docker manifest list with both arches", dockerListBoth, ""},
		{"single-platform manifest", singleManifest, "not a multi-arch index"},
		{"index missing arm64", missingArm, "missing required platform linux/arm64"},
		{"index missing amd64", missingAmd, "missing required platform linux/amd64"},
		{"empty mediaType", emptyMediaType, "not a multi-arch index"},
		{"malformed json", "{", "parse manifest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := imagepins.VerifyMultiArch([]byte(tc.raw))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want accept, got error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got accept", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("want error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}
