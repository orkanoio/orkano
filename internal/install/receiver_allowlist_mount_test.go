package install

import (
	"strings"
	"testing"

	"github.com/orkanoio/orkano/internal/repoallowlist"
)

func TestRenderComponentsReceiverProjectsRuntimeAllowlist(t *testing.T) {
	receiver := renderByName(t, Config{Version: "1.0.0"})["components-receiver.yaml"]
	for _, want := range []string{
		"name: ORKANO_REPO_ALLOWLIST_FILE",
		`value: "` + repoallowlist.DefaultFilePath + `"`,
		"name: " + repoallowlist.ConfigMapName,
		"key: " + repoallowlist.DataKey,
		"mountPath: /etc/orkano/repo-allowlist",
		"readOnly: true",
		"automountServiceAccountToken: false",
	} {
		if !strings.Contains(receiver, want) {
			t.Errorf("receiver manifest missing %q", want)
		}
	}
	if strings.Contains(receiver, "- name: ORKANO_REPO_ALLOWLIST\n") {
		t.Error("receiver manifest still injects the startup-only allowlist environment variable")
	}
	if strings.Contains(receiver, "\n              subPath:") {
		t.Error("receiver must mount the whole ConfigMap directory; subPath volumes never receive updates")
	}
}
