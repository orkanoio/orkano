package cluster

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/orkanoio/orkano/api/check"
)

// The supported window is the last three Kubernetes minors ending at the
// frozen client version table (k8s.io/* v0.36 — PLANNING's spike-2 table).
// Bump both bounds together with that table; the contract test pins them so
// the bump is deliberate.
const (
	supportedMajor    = 1
	minSupportedMinor = 34
	maxSupportedMinor = 36
)

// versionSupportedCheck gates on the API server's minor being inside the
// supported window. Both directions fail: an older cluster is untested and
// likely missing API surface Orkano uses, and a newer one is outside what the
// conformance matrix has proven — "supported" is a tested claim, so the
// remediation differs (upgrade the cluster vs upgrade Orkano) but the verdict
// does not.
func versionSupportedCheck(opt Options) check.Check {
	window := fmt.Sprintf("v%d.%d–v%d.%d", supportedMajor, minSupportedMinor, supportedMajor, maxSupportedMinor)
	return check.Check{
		ID:       IDVersionSupported,
		Severity: check.SeverityCritical,
		Summary:  fmt.Sprintf("cluster version is inside the supported window %s", window),
		Remediation: fmt.Sprintf("run Orkano on a cluster inside %s: upgrade the cluster if it is older, "+
			"or upgrade Orkano if the cluster is newer than this release's window", window),
		Probe: func(_ context.Context) (check.Result, error) {
			if opt.ServerVersion == nil {
				return check.Result{}, errors.New("no server-version reader configured")
			}
			info, err := opt.ServerVersion()
			if err != nil {
				return check.Result{}, fmt.Errorf("read server version: %w", err)
			}
			// Managed distributions suffix the fields ("36+" on GKE), so parse
			// the leading integer; a field with none is indeterminate, never a
			// verdict.
			major, err := leadingInt(info.Major)
			if err != nil {
				return check.Result{}, fmt.Errorf("parse server major version %q: %w", info.Major, err)
			}
			minor, err := leadingInt(info.Minor)
			if err != nil {
				return check.Result{}, fmt.Errorf("parse server minor version %q: %w", info.Minor, err)
			}

			switch {
			case major < supportedMajor || (major == supportedMajor && minor < minSupportedMinor):
				return check.Result{
					Status: check.StatusFail,
					Message: fmt.Sprintf("cluster %s (v%d.%d) is older than the supported window %s — upgrade the cluster",
						info.GitVersion, major, minor, window),
				}, nil
			case major > supportedMajor || minor > maxSupportedMinor:
				return check.Result{
					Status: check.StatusFail,
					Message: fmt.Sprintf("cluster %s (v%d.%d) is newer than this Orkano release's tested window %s — upgrade Orkano",
						info.GitVersion, major, minor, window),
				}, nil
			}
			return check.Result{
				Status:  check.StatusPass,
				Message: fmt.Sprintf("cluster %s (v%d.%d) is inside the supported window %s", info.GitVersion, major, minor, window),
			}, nil
		},
	}
}

// leadingInt parses the leading decimal digits of s ("36+" → 36).
func leadingInt(s string) (int, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("no leading integer in %q", s)
	}
	return strconv.Atoi(s[:i])
}
