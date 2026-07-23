package doctor

import (
	"testing"

	"github.com/orkanoio/orkano/internal/install"
)

// TestComponentsMatchReadinessTargets keeps the components check's workload
// list identical to the orkano-system subset install waits on after a deploy,
// so init's critical path and doctor's report can never drift. cert-manager's
// own Deployments (in the cert-manager namespace) are deliberately not doctor's
// to check here — tls.certificate-expiry covers the PKI — so the cross-check is
// scoped to orkano-system.
func TestComponentsMatchReadinessTargets(t *testing.T) {
	want := map[string]bool{}
	for _, w := range install.DefaultReadinessTargets() {
		if w.Namespace != systemNamespace {
			continue
		}
		want[w.Kind+"/"+w.Name] = true
	}

	got := map[string]bool{}
	for _, name := range componentDeployments {
		got["deployment/"+name] = true
	}
	for _, name := range componentStatefulSets {
		got["statefulset/"+name] = true
	}

	if len(got) != len(want) {
		t.Fatalf("components check reads %d orkano-system workloads, DefaultReadinessTargets has %d", len(got), len(want))
	}
	for k := range want {
		if !got[k] {
			t.Errorf("DefaultReadinessTargets waits on %s but the components check does not read it", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("the components check reads %s but it is not in DefaultReadinessTargets", k)
		}
	}
}
