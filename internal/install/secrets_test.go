package install

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/internal/platformsecrets"
)

func TestApplyEnsuresSecretsAndReturnsToken(t *testing.T) {
	n := newFakeNode()
	res, err := Apply(context.Background(), n, Config{Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, name := range []string{platformsecrets.NameSuperuser, platformsecrets.NameOperatorDB, platformsecrets.NameReceiverDB, platformsecrets.NameDashboardDB, platformsecrets.NameDashboardEncKey, platformsecrets.NameWebhook, platformsecrets.NameBootstrapToken, platformsecrets.NameGitHubApp, platformsecrets.NameOIDC} {
		if _, ok := n.secrets[name]; !ok {
			t.Errorf("expected secret %s to be created", name)
		}
	}
	if res.BootstrapToken == "" {
		t.Fatal("expected a freshly generated bootstrap token to be returned")
	}

	// The stored value is the sha256 hash of the returned token, never the token.
	sum := sha256.Sum256([]byte(res.BootstrapToken))
	wantData := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(sum[:])))
	if !strings.Contains(n.secrets[platformsecrets.NameBootstrapToken], "token-sha256: "+wantData) {
		t.Error("bootstrap-token Secret should store the sha256 of the returned token")
	}
	if strings.Contains(n.secrets[platformsecrets.NameBootstrapToken], res.BootstrapToken) {
		t.Error("bootstrap-token Secret must not store the plaintext token")
	}

	// The GitHub App and OIDC Secrets are empty placeholders — no credential
	// value, just targets for the M2.6 wizard's value-blind updates. An empty
	// orkano-oidc resolves nothing through the Deployment's per-key refs.
	for _, name := range []string{platformsecrets.NameGitHubApp, platformsecrets.NameOIDC} {
		if got := n.secrets[name]; !strings.Contains(got, "data: {}") {
			t.Errorf("%s placeholder should carry empty data, got: %s", name, got)
		}
	}
	if strings.Contains(n.secrets[platformsecrets.NameGitHubApp], "private-key.pem") {
		t.Error("github-app placeholder must not carry a private key at install time")
	}

	// The role DSNs embed the matching roles and the platform Postgres host.
	if !strings.Contains(decodeSecretData(t, n.secrets[platformsecrets.NameReceiverDB], "dsn"), "postgres://orkano_receiver:") {
		t.Error("receiver DSN should use the orkano_receiver role")
	}
	if !strings.Contains(decodeSecretData(t, n.secrets[platformsecrets.NameOperatorDB], "dsn"), "postgres://orkano_dispatcher:") {
		t.Error("operator DSN should use the orkano_dispatcher role")
	}
	if !strings.Contains(decodeSecretData(t, n.secrets[platformsecrets.NameDashboardDB], "dsn"), "postgres://orkano_dashboard:") {
		t.Error("dashboard DSN should use the orkano_dashboard role")
	}
}

func TestApplyPreservesExistingSecrets(t *testing.T) {
	n := newFakeNode()
	// Pre-existing secrets (a prior install): mark all present.
	for _, name := range []string{platformsecrets.NameSuperuser, platformsecrets.NameOperatorDB, platformsecrets.NameReceiverDB, platformsecrets.NameDashboardDB, platformsecrets.NameDashboardEncKey, platformsecrets.NameWebhook, platformsecrets.NameBootstrapToken, platformsecrets.NameGitHubApp, platformsecrets.NameOIDC} {
		n.secrets[name] = "preexisting"
	}

	res, err := Apply(context.Background(), n, Config{Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.BootstrapToken != "" {
		t.Error("a re-run must not regenerate or return the bootstrap token")
	}
	if hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "kubectl apply -f -") }) {
		t.Error("no secret should be re-applied when all already exist")
	}
	// Untouched.
	if n.secrets[platformsecrets.NameSuperuser] != "preexisting" {
		t.Error("existing superuser secret must be preserved untouched")
	}
}

func TestApplyWaitsForNamespace(t *testing.T) {
	defer swapPollInterval(time.Millisecond)()

	n := newFakeNode()
	n.noNS = true // namespace never appears

	_, err := Apply(context.Background(), n, Config{Version: "1.0.0", WaitTimeout: 30 * time.Millisecond})
	if err == nil {
		t.Fatal("expected a timeout waiting for the namespace")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("error should mention the namespace, got: %v", err)
	}
}

// decodeSecretData extracts and base64-decodes one data value from a rendered
// Secret manifest.
func decodeSecretData(t *testing.T, manifest, key string) string {
	t.Helper()
	for _, line := range strings.Split(manifest, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, key+": ") {
			dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, key+": "))
			if err != nil {
				t.Fatalf("decode %s: %v", key, err)
			}
			return string(dec)
		}
	}
	t.Fatalf("key %s not found in manifest", key)
	return ""
}
