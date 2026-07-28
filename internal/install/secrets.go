package install

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/orkanoio/orkano/internal/platformsecrets"
)

// systemNS is the platform namespace; platformsecrets owns the constant, this
// alias keeps the rest of the package (registry.go, the kubectl commands) reading
// as it did.
const systemNS = platformsecrets.Namespace

// ensureSecrets creates the install's generated Secrets in orkano-system,
// generate-once: a Secret that already exists is preserved untouched (a re-run
// must not rotate the superuser password baked into Postgres's data dir, or the
// role passwords the schema was set up with). It returns the plaintext bootstrap
// token only when this run created it (so the caller prints it exactly once),
// and whether any Secret was created.
func ensureSecrets(ctx context.Context, n *node, v platformsecrets.Values) (bootstrapToken string, changed bool, err error) {
	specs, err := platformsecrets.Specs(v)
	if err != nil {
		return "", false, err
	}
	for _, s := range specs {
		created, err := n.createSecretIfAbsent(ctx, s.Name, secretManifest(s.Name, s.Data))
		if err != nil {
			return bootstrapToken, changed, err
		}
		if created {
			changed = true
			n.logf("created secret %s", s.Name)
			if s.Name == platformsecrets.NameBootstrapToken {
				bootstrapToken = v.BootstrapToken
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
