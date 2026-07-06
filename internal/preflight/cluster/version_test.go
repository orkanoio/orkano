package cluster_test

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/version"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/preflight/cluster"
)

func stubVersion(major, minor, git string) func() (*version.Info, error) {
	return func() (*version.Info, error) {
		return &version.Info{Major: major, Minor: minor, GitVersion: git}, nil
	}
}

func TestVersionSupported(t *testing.T) {
	cases := []struct {
		name         string
		major, minor string
		want         check.Status
		wantContains string
	}{
		{name: "oldest supported minor passes", major: "1", minor: "34", want: check.StatusPass},
		{name: "middle of the window passes", major: "1", minor: "35", want: check.StatusPass},
		{name: "newest supported minor passes", major: "1", minor: "36", want: check.StatusPass},
		{name: "GKE-style suffixed minor passes", major: "1", minor: "36+", want: check.StatusPass},
		{name: "older than the window fails toward the cluster", major: "1", minor: "33", want: check.StatusFail, wantContains: "upgrade the cluster"},
		{name: "newer than the window fails toward Orkano", major: "1", minor: "37", want: check.StatusFail, wantContains: "upgrade Orkano"},
		{name: "a newer major fails toward Orkano", major: "2", minor: "0", want: check.StatusFail, wantContains: "upgrade Orkano"},
		{name: "an older major fails toward the cluster", major: "0", minor: "36", want: check.StatusFail, wantContains: "upgrade the cluster"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt := cluster.Options{ServerVersion: stubVersion(tc.major, tc.minor, "v"+tc.major+"."+tc.minor+".0")}
			res, err := probeCheck(t, opt, cluster.IDVersionSupported)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if res.Status != tc.want {
				t.Fatalf("status = %q (%s), want %q", res.Status, res.Message, tc.want)
			}
			if tc.wantContains != "" && !strings.Contains(res.Message, tc.wantContains) {
				t.Errorf("message %q should contain %q", res.Message, tc.wantContains)
			}
		})
	}

	t.Run("unparseable minor is a probe error", func(t *testing.T) {
		opt := cluster.Options{ServerVersion: stubVersion("1", "x", "vX")}
		if _, err := probeCheck(t, opt, cluster.IDVersionSupported); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("unparseable major is a probe error", func(t *testing.T) {
		opt := cluster.Options{ServerVersion: stubVersion("", "36", "v?.36.0")}
		if _, err := probeCheck(t, opt, cluster.IDVersionSupported); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("server-version read failure is a probe error", func(t *testing.T) {
		opt := cluster.Options{ServerVersion: func() (*version.Info, error) {
			return nil, errors.New("apiserver unreachable")
		}}
		if _, err := probeCheck(t, opt, cluster.IDVersionSupported); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("missing server-version reader is a probe error", func(t *testing.T) {
		if _, err := probeCheck(t, cluster.Options{}, cluster.IDVersionSupported); err == nil {
			t.Fatal("expected a probe error")
		}
	})
}
