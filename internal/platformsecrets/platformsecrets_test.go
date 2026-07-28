package platformsecrets

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	v, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]+$`)
	for name, val := range map[string]string{
		"superuser":  v.SuperuserPassword,
		"receiver":   v.ReceiverPassword,
		"dispatcher": v.DispatcherPassword,
		"dashboard":  v.DashboardPassword,
		"webhook":    v.WebhookSecret,
	} {
		if val == "" {
			t.Errorf("%s value is empty", name)
		}
		if !hexRe.MatchString(val) {
			t.Errorf("%s value %q is not hex (must satisfy db.SetupRoles's safe charset)", name, val)
		}
	}
	tok, err := base64.RawURLEncoding.DecodeString(v.BootstrapToken)
	if err != nil {
		t.Errorf("bootstrap token %q is not base64url: %v", v.BootstrapToken, err)
	}
	if len(tok) != 32 {
		t.Errorf("bootstrap token decodes to %d bytes, want 32", len(tok))
	}
	// The dashboard encryption key is base64 of exactly 32 bytes (AES-256).
	key, err := base64.StdEncoding.DecodeString(v.DashboardEncKey)
	if err != nil {
		t.Errorf("dashboard enc key %q is not std-base64: %v", v.DashboardEncKey, err)
	}
	if len(key) != 32 {
		t.Errorf("dashboard enc key decodes to %d bytes, want 32", len(key))
	}
	// All role passwords distinct (no pointer in the generate loop reused
	// another's draw): checked all-pairs via a set, so no collision slips
	// through a chain.
	rolePasswords := []string{v.SuperuserPassword, v.ReceiverPassword, v.DispatcherPassword, v.DashboardPassword}
	seen := make(map[string]struct{}, len(rolePasswords))
	for _, p := range rolePasswords {
		seen[p] = struct{}{}
	}
	if len(seen) != len(rolePasswords) {
		t.Error("expected distinct role passwords")
	}
}

func TestSpecs(t *testing.T) {
	v, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	specs, err := Specs(v)
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}

	// The order and the key sets are frozen: every consuming manifest binds by
	// name+key, and the SSH path's partial-token contract depends on the
	// bootstrap token being created before the two placeholders.
	want := []struct {
		name string
		keys []string
	}{
		{NameSuperuser, []string{KeyPassword, KeyDSN}},
		{NameOperatorDB, []string{KeyPassword, KeyDSN}},
		{NameReceiverDB, []string{KeyPassword, KeyDSN}},
		{NameDashboardDB, []string{KeyPassword, KeyDSN}},
		{NameDashboardEncKey, []string{KeyEncKey}},
		{NameWebhook, []string{KeyWebhookSecret}},
		{NameBootstrapToken, []string{KeyTokenSHA256}},
		{NameGitHubApp, nil},
		{NameOIDC, nil},
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	for i, w := range want {
		got := specs[i]
		if got.Name != w.name {
			t.Errorf("spec %d is %s, want %s", i, got.Name, w.name)
			continue
		}
		if len(got.Data) != len(w.keys) {
			t.Errorf("%s has keys %v, want %v", got.Name, keysOf(got.Data), w.keys)
			continue
		}
		for _, k := range w.keys {
			if _, ok := got.Data[k]; !ok {
				t.Errorf("%s is missing key %s", got.Name, k)
			}
		}
	}

	byName := make(map[string]Spec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}
	for role, name := range map[string]string{
		"orkano_dispatcher": NameOperatorDB,
		"orkano_receiver":   NameReceiverDB,
		"orkano_dashboard":  NameDashboardDB,
	} {
		dsn := byName[name].Data[KeyDSN]
		if !strings.HasPrefix(dsn, "postgres://"+role+":") {
			t.Errorf("%s DSN %q should use the %s role", name, dsn, role)
		}
		if !strings.HasSuffix(dsn, "@orkano-postgres.orkano-system.svc:5432/orkano?sslmode=disable") {
			t.Errorf("%s DSN %q should point at the platform Postgres", name, dsn)
		}
	}

	if got := byName[NameBootstrapToken].Data[KeyTokenSHA256]; got != HashToken(v.BootstrapToken) {
		t.Errorf("bootstrap-token Secret stores %q, want the sha256 of the generated token", got)
	}
	// The plaintext token is never part of any Spec: only its hash is stored.
	for _, s := range specs {
		for k, val := range s.Data {
			if strings.Contains(val, v.BootstrapToken) {
				t.Errorf("%s/%s carries the plaintext bootstrap token", s.Name, k)
			}
		}
	}
}

func TestSpecsRejectsIncompleteValues(t *testing.T) {
	// Every field is load-bearing, but two are catastrophic under generate-once:
	// HashToken("") is a public constant the dashboard's redeem endpoint would
	// accept from anyone, and an empty enc key crashes the dashboard at startup.
	// Neither is correctable by a re-run, since an existing Secret is preserved.
	for _, tc := range []struct {
		field string
		blank func(*Values)
	}{
		{"SuperuserPassword", func(v *Values) { v.SuperuserPassword = "" }},
		{"ReceiverPassword", func(v *Values) { v.ReceiverPassword = "" }},
		{"DispatcherPassword", func(v *Values) { v.DispatcherPassword = "" }},
		{"DashboardPassword", func(v *Values) { v.DashboardPassword = "" }},
		{"WebhookSecret", func(v *Values) { v.WebhookSecret = "" }},
		{"BootstrapToken", func(v *Values) { v.BootstrapToken = "" }},
		{"DashboardEncKey", func(v *Values) { v.DashboardEncKey = "" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			v, err := Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			tc.blank(&v)
			specs, err := Specs(v)
			if err == nil {
				t.Fatalf("Specs accepted an empty %s", tc.field)
			}
			if specs != nil {
				t.Error("Specs must return no table when validation fails")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q should name the empty field %s", err, tc.field)
			}
		})
	}
}

func TestHashToken(t *testing.T) {
	// Pinned vector: the dashboard compares this exact encoding (lowercase hex),
	// so a drift to uppercase or to raw bytes would 401 every minted token.
	const (
		in   = "orkano"
		want = "6894b856fe3403a928bf51ba9b39c4d14af35e624db699c59a202d692992d775"
	)
	got := HashToken(in)
	if len(got) != 64 {
		t.Fatalf("HashToken(%q) = %q, want 64 hex chars", in, got)
	}
	if got != want {
		t.Errorf("HashToken(%q) = %q, want %q", in, got, want)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
