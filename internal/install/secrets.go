package install

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const (
	systemNS = "orkano-system"

	// postgresDSNHost is the in-cluster address of the platform Postgres
	// (headless Service). Connections are plaintext within the cluster
	// (sslmode=disable), reachable only via the receiver/operator egress.
	postgresDSNHost = "orkano-postgres.orkano-system.svc"

	// These are Secret object names, not credentials; gosec G101 false-positives
	// on the words "superuser"/"secret"/"token" in the values.
	secretSuperuser       = "orkano-postgres-superuser" //nolint:gosec // G101: Secret name, not a credential.
	secretOperator        = "orkano-operator-db"
	secretReceiver        = "orkano-receiver-db"
	secretDashboard       = "orkano-dashboard-db"
	secretDashboardEncKey = "orkano-dashboard-enc-key"
	secretWebhook         = "orkano-webhook-secret"  //nolint:gosec // G101: Secret name, not a credential.
	secretBootstrap       = "orkano-bootstrap-token" //nolint:gosec // G101: Secret name, not a credential.
	// secretGitHubApp is created EMPTY: the GitHub App credential does not exist
	// until an admin runs the M2.6 onboarding manifest flow, which fills app-id +
	// private-key.pem via a value-blind UPDATE (the dashboard's orkano-system grant
	// is update-only, so the placeholder must already exist for the update to land).
	// Generate-once like the rest, so a re-install never wipes a connected App.
	secretGitHubApp = "orkano-github-app" //nolint:gosec // G101: Secret name, not a credential.
	// secretOIDC is the same shape for the wizard's OIDC connect step: created
	// EMPTY (the Deployment's per-key optional secretKeyRefs resolve nothing
	// from it), filled by a value-blind UPDATE, preserved on re-install so a
	// connected IdP survives.
	secretOIDC = "orkano-oidc" //nolint:gosec // G101: Secret name, not a credential.
)

// secretValues are the credentials generated once per install. They are written
// into Kubernetes Secrets (etcd, encrypted at rest), never to a file on disk and
// never logged. The bootstrap token is the sole value surfaced to the operator,
// and only its sha256 hash is stored.
type secretValues struct {
	superuserPassword  string
	receiverPassword   string
	dispatcherPassword string
	dashboardPassword  string
	webhookSecret      string
	bootstrapToken     string // plaintext, returned for one-time printing
	// dashboardEncKey is the AES-256 key (base64 of 32 bytes) the dashboard uses
	// to encrypt TOTP seeds at rest. Generate-once: rotating it would make every
	// stored seed undecryptable, locking every user out of 2FA.
	dashboardEncKey string
}

// generateSecretValues produces fresh, URL-safe credentials. The role passwords
// satisfy db.SetupRoles's safe-character rule (hex), and the DSNs that embed
// them need no escaping. The bootstrap token is high-entropy base64url.
func generateSecretValues() (secretValues, error) {
	var v secretValues
	var err error
	for _, p := range []*string{&v.superuserPassword, &v.receiverPassword, &v.dispatcherPassword, &v.dashboardPassword, &v.webhookSecret} {
		if *p, err = randomHex(24); err != nil {
			return secretValues{}, err
		}
	}
	if v.bootstrapToken, err = randomToken(32); err != nil {
		return secretValues{}, err
	}
	// The dashboard's NewCipher decodes base64 of exactly 32 bytes (AES-256).
	if v.dashboardEncKey, err = randomKeyB64(32); err != nil {
		return secretValues{}, err
	}
	return v, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("install: generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("install: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomKeyB64 returns base64.StdEncoding of n random bytes — the form the
// dashboard's NewCipher decodes back into a raw AES key.
func randomKeyB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("install: generate key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// roleDSN builds a libpq URL for a role with an in-cluster, no-TLS connection.
// The password is hex (URL-safe), so no escaping is needed.
func roleDSN(role, password string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:5432/orkano?sslmode=disable", role, password, postgresDSNHost)
}

// ensureSecrets creates the install's generated Secrets in orkano-system,
// generate-once: a Secret that already exists is preserved untouched (a re-run
// must not rotate the superuser password baked into Postgres's data dir, or the
// role passwords the schema was set up with). It returns the plaintext bootstrap
// token only when this run created it (so the caller prints it exactly once),
// and whether any Secret was created.
func ensureSecrets(ctx context.Context, n *node, v secretValues) (bootstrapToken string, changed bool, err error) {
	tokenHash := sha256.Sum256([]byte(v.bootstrapToken))
	specs := []struct {
		name string
		data map[string]string
	}{
		{secretSuperuser, map[string]string{
			"password": v.superuserPassword,
			"dsn":      roleDSN("orkano", v.superuserPassword),
		}},
		{secretOperator, map[string]string{
			"password": v.dispatcherPassword,
			"dsn":      roleDSN("orkano_dispatcher", v.dispatcherPassword),
		}},
		{secretReceiver, map[string]string{
			"password": v.receiverPassword,
			"dsn":      roleDSN("orkano_receiver", v.receiverPassword),
		}},
		{secretDashboard, map[string]string{
			"password": v.dashboardPassword,
			"dsn":      roleDSN("orkano_dashboard", v.dashboardPassword),
		}},
		{secretDashboardEncKey, map[string]string{"key": v.dashboardEncKey}},
		{secretWebhook, map[string]string{"secret": v.webhookSecret}},
		{secretBootstrap, map[string]string{"token-sha256": hex.EncodeToString(tokenHash[:])}},
		// Empty placeholders for the M2.6 onboarding flows to fill (see the
		// secretGitHubApp/secretOIDC consts). No generated values — the GitHub
		// credentials come from GitHub, the OIDC configuration from the wizard.
		{secretGitHubApp, map[string]string{}},
		{secretOIDC, map[string]string{}},
	}

	for _, s := range specs {
		created, err := n.createSecretIfAbsent(ctx, s.name, secretManifest(s.name, s.data))
		if err != nil {
			return bootstrapToken, changed, err
		}
		if created {
			changed = true
			n.logf("created secret %s", s.name)
			if s.name == secretBootstrap {
				bootstrapToken = v.bootstrapToken
			}
		}
	}
	return bootstrapToken, changed, nil
}

// createSecretIfAbsent creates the Secret only when it does not already exist,
// reporting whether it created it. The manifest is piped in base64-decoded so no
// secret value ever appears in the command line.
func (n *node) createSecretIfAbsent(ctx context.Context, name string, manifest []byte) (bool, error) {
	got, err := n.r.Run(ctx, fmt.Sprintf("%s%s kubectl -n %s get secret %s -o name", n.sudo, k3sBin, systemNS, name))
	if err != nil {
		return false, fmt.Errorf("install: check secret %s: %w", name, err)
	}
	if got.ExitStatus == 0 {
		return false, nil // exists — preserve
	}
	enc := base64.StdEncoding.EncodeToString(manifest)
	cmd := fmt.Sprintf("printf %%s '%s' | base64 -d | %s%s kubectl apply -f -", enc, n.sudo, k3sBin)
	if err := n.runOK(ctx, cmd, "create secret "+name); err != nil {
		return false, err
	}
	return true, nil
}

// secretManifest renders an Opaque Secret with base64-encoded data. The name and
// keys are fixed constants, never input, so the YAML cannot be injected.
func secretManifest(name string, data map[string]string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\n", name, systemNS)
	if len(data) == 0 {
		// An explicit empty map (placeholder Secret), not `data:` with a null value
		// which kubectl rejects.
		b.WriteString("data: {}\n")
		return []byte(b.String())
	}
	b.WriteString("data:\n")
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", k, base64.StdEncoding.EncodeToString([]byte(data[k])))
	}
	return []byte(b.String())
}
