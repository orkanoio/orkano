// Package platformsecrets holds the coordinates and the generation of the
// platform's generate-once Secrets: the object names, the frozen data keys, the
// credential shapes, and the ordered table both install paths seed.
//
// It is deliberately a stdlib-only leaf. The operator image (which is also the
// migrate Job, the source-fetch init container and the bootstrap Job) reaches
// this package; importing internal/install instead would drag the ~2 MB of
// embedded manifests and the SSH transport into every one of them.
package platformsecrets

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Namespace is where every platform Secret lives. The whole platform is pinned
// to it in every manifest, so it is a constant, not a knob.
const Namespace = "orkano-system"

// SeedSubcommand is the operator argv[1] that runs the in-cluster seeder. The
// chart's bootstrap Job passes exactly this string; a mismatch would fall
// through the operator's dispatch and start the controller manager instead of
// failing loudly, so both sides bind to this const.
const SeedSubcommand = "bootstrap-secrets"

// postgresDSNHost is the in-cluster address of the platform Postgres (headless
// Service). Connections are plaintext within the cluster (sslmode=disable),
// reachable only via the receiver/operator egress.
const postgresDSNHost = "orkano-postgres.orkano-system.svc"

// These are Secret OBJECT NAMES, not credentials.
const (
	NameSuperuser       = "orkano-postgres-superuser"
	NameOperatorDB      = "orkano-operator-db"
	NameReceiverDB      = "orkano-receiver-db"
	NameDashboardDB     = "orkano-dashboard-db"
	NameDashboardEncKey = "orkano-dashboard-enc-key"
	NameWebhook         = "orkano-webhook-secret"
	NameBootstrapToken  = "orkano-bootstrap-token"
	// NameGitHubApp is created EMPTY: the GitHub App credential does not exist
	// until an admin runs the onboarding manifest flow, which fills app-id +
	// private-key.pem via a value-blind UPDATE (the dashboard's orkano-system
	// grant is update-only, so the placeholder must already exist for the update
	// to land). Generate-once like the rest, so a re-install never wipes a
	// connected App.
	NameGitHubApp = "orkano-github-app"
	// NameOIDC is the same shape for the wizard's OIDC connect step: created
	// EMPTY (the Deployment's per-key optional secretKeyRefs resolve nothing
	// from it), filled by a value-blind UPDATE, preserved on re-install so a
	// connected IdP survives.
	NameOIDC = "orkano-oidc"
)

// These are Secret DATA KEYS, frozen: every consuming manifest binds by
// name+key, so renaming one breaks a deployed workload's env resolution.
const (
	KeyPassword      = "password"
	KeyDSN           = "dsn"
	KeyEncKey        = "key"
	KeyWebhookSecret = "secret"
	KeyTokenSHA256   = "token-sha256"
)

// Values are the credentials generated once per install. They are written into
// Kubernetes Secrets (etcd, encrypted at rest), never to a file on disk and
// never logged. The bootstrap token is the sole value surfaced to the operator,
// and only its sha256 hash is stored.
type Values struct {
	SuperuserPassword  string
	ReceiverPassword   string
	DispatcherPassword string
	DashboardPassword  string
	WebhookSecret      string
	BootstrapToken     string // plaintext, returned for one-time printing
	// DashboardEncKey is the AES-256 key (base64 of 32 bytes) the dashboard uses
	// to encrypt TOTP seeds at rest. Generate-once: rotating it would make every
	// stored seed undecryptable, locking every user out of 2FA.
	DashboardEncKey string
}

// Generate produces fresh, URL-safe credentials. The role passwords satisfy
// db.SetupRoles's safe-character rule (hex), and the DSNs that embed them need
// no escaping. The bootstrap token is high-entropy base64url.
func Generate() (Values, error) {
	var v Values
	var err error
	for _, p := range []*string{&v.SuperuserPassword, &v.ReceiverPassword, &v.DispatcherPassword, &v.DashboardPassword, &v.WebhookSecret} {
		if *p, err = randomHex(24); err != nil {
			return Values{}, err
		}
	}
	if v.BootstrapToken, err = GenerateBootstrapToken(); err != nil {
		return Values{}, err
	}
	// The dashboard's NewCipher decodes base64 of exactly 32 bytes (AES-256).
	if v.DashboardEncKey, err = randomKeyB64(32); err != nil {
		return Values{}, err
	}
	return v, nil
}

// GenerateBootstrapToken mints one ADR-0003 bootstrap token: 256 bits of
// entropy in base64url. It is the single definition of the token's shape,
// shared by the install paths and by `orkano bootstrap-token`.
func GenerateBootstrapToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("platformsecrets: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns hex(sha256(raw)): the only form of a bootstrap token that
// is ever stored, and the form the dashboard compares a redemption against.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// RoleDSN builds a libpq URL for a role with an in-cluster, no-TLS connection.
// The password is hex (URL-safe), so no escaping is needed.
func RoleDSN(role, password string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:5432/orkano?sslmode=disable", role, password, postgresDSNHost)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("platformsecrets: generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// randomKeyB64 returns base64.StdEncoding of n random bytes: the form the
// dashboard's NewCipher decodes back into a raw AES key.
func randomKeyB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("platformsecrets: generate key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Spec is one platform Secret: its object name and its plaintext data. Both
// install paths write exactly this set, in this order.
type Spec struct {
	Name string
	Data map[string]string
}

// Specs renders the frozen, ordered Secret table from generated values. The
// bootstrap-token Secret carries only the sha256 of the token; the plaintext
// never appears in a Spec.
//
// It validates first because a single empty value here is unrecoverable under
// generate-once semantics: an empty BootstrapToken would store
// HashToken(""), a publicly known constant, and the dashboard's redeem
// endpoint would then accept the empty token from anyone who can reach it. An
// empty DashboardEncKey crashes the dashboard at startup. Neither can be
// corrected by a re-run, because a Secret that exists is never rewritten.
func Specs(v Values) ([]Spec, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	return []Spec{
		{NameSuperuser, map[string]string{
			KeyPassword: v.SuperuserPassword,
			KeyDSN:      RoleDSN("orkano", v.SuperuserPassword),
		}},
		{NameOperatorDB, map[string]string{
			KeyPassword: v.DispatcherPassword,
			KeyDSN:      RoleDSN("orkano_dispatcher", v.DispatcherPassword),
		}},
		{NameReceiverDB, map[string]string{
			KeyPassword: v.ReceiverPassword,
			KeyDSN:      RoleDSN("orkano_receiver", v.ReceiverPassword),
		}},
		{NameDashboardDB, map[string]string{
			KeyPassword: v.DashboardPassword,
			KeyDSN:      RoleDSN("orkano_dashboard", v.DashboardPassword),
		}},
		{NameDashboardEncKey, map[string]string{KeyEncKey: v.DashboardEncKey}},
		{NameWebhook, map[string]string{KeyWebhookSecret: v.WebhookSecret}},
		{NameBootstrapToken, map[string]string{KeyTokenSHA256: HashToken(v.BootstrapToken)}},
		// Empty placeholders for the onboarding flows to fill (see the
		// NameGitHubApp/NameOIDC consts). No generated values: the GitHub
		// credentials come from GitHub, the OIDC configuration from the wizard.
		{NameGitHubApp, map[string]string{}},
		{NameOIDC, map[string]string{}},
	}, nil
}

// validate names the empty field and never its value.
func (v Values) validate() error {
	for _, f := range []struct {
		name  string
		value string
	}{
		{"SuperuserPassword", v.SuperuserPassword},
		{"ReceiverPassword", v.ReceiverPassword},
		{"DispatcherPassword", v.DispatcherPassword},
		{"DashboardPassword", v.DashboardPassword},
		{"WebhookSecret", v.WebhookSecret},
		{"BootstrapToken", v.BootstrapToken},
		{"DashboardEncKey", v.DashboardEncKey},
	} {
		if f.value == "" {
			return fmt.Errorf("platformsecrets: %s is empty", f.name)
		}
	}
	return nil
}
