package registry

import "testing"

// The TLS round trip — real HEAD against a TLS server, CA fetched from the
// real apiserver — lives in the controller envtest suite next to the Build
// controller that consumes it. Here: the pure ref parsing.
func TestParseRef(t *testing.T) {
	for _, tc := range []struct {
		in              string
		host, repo, tag string
		wantErr         bool
	}{
		{in: "orkano-registry.orkano-system.svc.cluster.local/api/web:0123abcd", host: "orkano-registry.orkano-system.svc.cluster.local", repo: "api/web", tag: "0123abcd"},
		{in: "127.0.0.1:39031/smoke/template:fixture", host: "127.0.0.1:39031", repo: "smoke/template", tag: "fixture"},
		{in: "host.example/app:tag", host: "host.example", repo: "app", tag: "tag"},
		{in: "no-slash:tag", wantErr: true},
		{in: "host.example/app", wantErr: true},
		{in: "host.example/app:", wantErr: true},
		{in: "host.example/:tag", wantErr: true},
		{in: "host.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wantErr: true},
		{in: "127.0.0.1:39031/app", wantErr: true},
		{in: "", wantErr: true},
	} {
		host, repo, tag, err := parseRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRef(%q) succeeded with (%q, %q, %q), want error", tc.in, host, repo, tag)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRef(%q): %v", tc.in, err)
			continue
		}
		if host != tc.host || repo != tc.repo || tag != tc.tag {
			t.Errorf("parseRef(%q) = (%q, %q, %q), want (%q, %q, %q)", tc.in, host, repo, tag, tc.host, tc.repo, tc.tag)
		}
	}
}
