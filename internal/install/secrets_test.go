package install

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestGenerateSecretValues(t *testing.T) {
	v, err := generateSecretValues()
	if err != nil {
		t.Fatalf("generateSecretValues: %v", err)
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]+$`)
	for name, val := range map[string]string{
		"superuser":  v.superuserPassword,
		"receiver":   v.receiverPassword,
		"dispatcher": v.dispatcherPassword,
		"dashboard":  v.dashboardPassword,
		"webhook":    v.webhookSecret,
	} {
		if val == "" {
			t.Errorf("%s value is empty", name)
		}
		if !hexRe.MatchString(val) {
			t.Errorf("%s value %q is not hex (must satisfy db.SetupRoles's safe charset)", name, val)
		}
	}
	if v.bootstrapToken == "" {
		t.Error("bootstrap token is empty")
	}
	// The dashboard encryption key is base64 of exactly 32 bytes (AES-256).
	key, err := base64.StdEncoding.DecodeString(v.dashboardEncKey)
	if err != nil {
		t.Errorf("dashboard enc key %q is not std-base64: %v", v.dashboardEncKey, err)
	}
	if len(key) != 32 {
		t.Errorf("dashboard enc key decodes to %d bytes, want 32", len(key))
	}
	// All role passwords distinct (no pointer in the generate loop reused another's
	// draw) — checked all-pairs via a set, so no collision slips through a chain.
	rolePasswords := []string{v.superuserPassword, v.receiverPassword, v.dispatcherPassword, v.dashboardPassword}
	seen := make(map[string]struct{}, len(rolePasswords))
	for _, p := range rolePasswords {
		seen[p] = struct{}{}
	}
	if len(seen) != len(rolePasswords) {
		t.Error("expected distinct role passwords")
	}
}

func TestApplyEnsuresSecretsAndReturnsToken(t *testing.T) {
	n := newFakeNode()
	res, err := Apply(context.Background(), n, Config{Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, name := range []string{secretSuperuser, secretOperator, secretReceiver, secretDashboard, secretDashboardEncKey, secretWebhook, secretBootstrap} {
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
	if !strings.Contains(n.secrets[secretBootstrap], "token-sha256: "+wantData) {
		t.Error("bootstrap-token Secret should store the sha256 of the returned token")
	}
	if strings.Contains(n.secrets[secretBootstrap], res.BootstrapToken) {
		t.Error("bootstrap-token Secret must not store the plaintext token")
	}

	// The role DSNs embed the matching roles and the platform Postgres host.
	if !strings.Contains(decodeSecretData(t, n.secrets[secretReceiver], "dsn"), "postgres://orkano_receiver:") {
		t.Error("receiver DSN should use the orkano_receiver role")
	}
	if !strings.Contains(decodeSecretData(t, n.secrets[secretOperator], "dsn"), "postgres://orkano_dispatcher:") {
		t.Error("operator DSN should use the orkano_dispatcher role")
	}
	if !strings.Contains(decodeSecretData(t, n.secrets[secretDashboard], "dsn"), "postgres://orkano_dashboard:") {
		t.Error("dashboard DSN should use the orkano_dashboard role")
	}
}

func TestApplyPreservesExistingSecrets(t *testing.T) {
	n := newFakeNode()
	// Pre-existing secrets (a prior install): mark all present.
	for _, name := range []string{secretSuperuser, secretOperator, secretReceiver, secretDashboard, secretDashboardEncKey, secretWebhook, secretBootstrap} {
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
	if n.secrets[secretSuperuser] != "preexisting" {
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
