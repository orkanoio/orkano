package bootstrap

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/orkanoio/orkano/internal/platformsecrets"
)

const ns = platformsecrets.Namespace

var allNames = []string{
	platformsecrets.NameSuperuser,
	platformsecrets.NameOperatorDB,
	platformsecrets.NameReceiverDB,
	platformsecrets.NameDashboardDB,
	platformsecrets.NameDashboardEncKey,
	platformsecrets.NameWebhook,
	platformsecrets.NameBootstrapToken,
	platformsecrets.NameGitHubApp,
	platformsecrets.NameOIDC,
}

func TestSeedCreatesEveryPlatformSecret(t *testing.T) {
	cs := fake.NewClientset()
	res, err := SeedSecrets(context.Background(), cs.CoreV1().Secrets(ns))
	if err != nil {
		t.Fatalf("SeedSecrets: %v", err)
	}
	if len(res.Created) != len(allNames) || len(res.Existed) != 0 {
		t.Fatalf("created %v, existed %v; want all %d created", res.Created, res.Existed, len(allNames))
	}

	stored := storedSecrets(t, cs)
	wantKeys := map[string][]string{
		platformsecrets.NameSuperuser:       {platformsecrets.KeyPassword, platformsecrets.KeyDSN},
		platformsecrets.NameOperatorDB:      {platformsecrets.KeyPassword, platformsecrets.KeyDSN},
		platformsecrets.NameReceiverDB:      {platformsecrets.KeyPassword, platformsecrets.KeyDSN},
		platformsecrets.NameDashboardDB:     {platformsecrets.KeyPassword, platformsecrets.KeyDSN},
		platformsecrets.NameDashboardEncKey: {platformsecrets.KeyEncKey},
		platformsecrets.NameWebhook:         {platformsecrets.KeyWebhookSecret},
		platformsecrets.NameBootstrapToken:  {platformsecrets.KeyTokenSHA256},
		platformsecrets.NameGitHubApp:       nil,
		platformsecrets.NameOIDC:            nil,
	}
	for name, keys := range wantKeys {
		sec, ok := stored[name]
		if !ok {
			t.Errorf("secret %s was not created", name)
			continue
		}
		if sec.Type != corev1.SecretTypeOpaque {
			t.Errorf("%s has type %q, want Opaque", name, sec.Type)
		}
		if len(sec.Data) != len(keys) {
			t.Errorf("%s has %d keys, want %d", name, len(sec.Data), len(keys))
		}
		for _, k := range keys {
			if len(sec.Data[k]) == 0 {
				t.Errorf("%s is missing a value for key %s", name, k)
			}
		}
	}

	hash := string(stored[platformsecrets.NameBootstrapToken].Data[platformsecrets.KeyTokenSHA256])
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
		t.Errorf("bootstrap token hash %q is not 64 lowercase hex chars", hash)
	}
}

func TestSeedPreservesExisting(t *testing.T) {
	cs := fake.NewClientset(sentinels(allNames...)...)
	res, err := SeedSecrets(context.Background(), cs.CoreV1().Secrets(ns))
	if err != nil {
		t.Fatalf("SeedSecrets: %v", err)
	}
	if len(res.Created) != 0 || len(res.Existed) != len(allNames) {
		t.Fatalf("created %v, existed %v; want all %d preserved", res.Created, res.Existed, len(allNames))
	}
	for name, sec := range storedSecrets(t, cs) {
		if string(sec.Data["sentinel"]) != "keep" {
			t.Errorf("%s was overwritten; generate-once must preserve an existing Secret", name)
		}
	}
}

func TestSeedPartialPreserve(t *testing.T) {
	pre := []string{platformsecrets.NameSuperuser, platformsecrets.NameWebhook, platformsecrets.NameOIDC}
	cs := fake.NewClientset(sentinels(pre...)...)
	res, err := SeedSecrets(context.Background(), cs.CoreV1().Secrets(ns))
	if err != nil {
		t.Fatalf("SeedSecrets: %v", err)
	}
	if len(res.Created) != len(allNames)-len(pre) {
		t.Errorf("created %v, want the %d missing Secrets", res.Created, len(allNames)-len(pre))
	}
	if len(res.Existed) != len(pre) {
		t.Errorf("existed %v, want the %d pre-seeded Secrets", res.Existed, len(pre))
	}
	stored := storedSecrets(t, cs)
	for _, name := range pre {
		if string(stored[name].Data["sentinel"]) != "keep" {
			t.Errorf("pre-existing %s was overwritten", name)
		}
	}
}

func TestSeedTreatsAlreadyExistsAsPreserved(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.CreateAction).GetObject().(*corev1.Secret).Name
		if name != platformsecrets.NameWebhook {
			return false, nil, nil
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, name)
	})

	res, err := SeedSecrets(context.Background(), cs.CoreV1().Secrets(ns))
	if err != nil {
		t.Fatalf("SeedSecrets: %v", err)
	}
	if len(res.Existed) != 1 || res.Existed[0] != platformsecrets.NameWebhook {
		t.Errorf("existed %v, want just %s", res.Existed, platformsecrets.NameWebhook)
	}
	if len(res.Created) != len(allNames)-1 {
		t.Errorf("created %v, want the other %d", res.Created, len(allNames)-1)
	}
}

// TestSeedNeverReadsOrMutates is the mechanical proof behind the Job's
// create-only Role: no verb other than create is ever issued, so the seeder can
// neither read a live credential nor rotate one.
func TestSeedNeverReadsOrMutates(t *testing.T) {
	cs := fake.NewClientset(sentinels(platformsecrets.NameSuperuser)...)
	cs.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() != "create" {
			t.Errorf("seeder issued %s on %s; the Job's Role grants create only", action.GetVerb(), action.GetResource().Resource)
		}
		return false, nil, nil
	})
	if _, err := SeedSecrets(context.Background(), cs.CoreV1().Secrets(ns)); err != nil {
		t.Fatalf("SeedSecrets: %v", err)
	}
}

func TestSeedTwiceIsIdentical(t *testing.T) {
	cs := fake.NewClientset()
	if _, err := SeedSecrets(context.Background(), cs.CoreV1().Secrets(ns)); err != nil {
		t.Fatalf("first SeedSecrets: %v", err)
	}
	first := snapshot(storedSecrets(t, cs))

	res, err := SeedSecrets(context.Background(), cs.CoreV1().Secrets(ns))
	if err != nil {
		t.Fatalf("second SeedSecrets: %v", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("second run created %v; a re-run must rotate nothing", res.Created)
	}
	second := snapshot(storedSecrets(t, cs))
	for name, data := range first {
		if second[name] != data {
			t.Errorf("%s changed across two seeder runs", name)
		}
	}
}

// TestSeedErrorCarriesNoValue pins that a failed create surfaces the Secret name
// and nothing generated: the error text reaches the Job's logs.
func TestSeedErrorCarriesNoValue(t *testing.T) {
	v, err := platformsecrets.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cs := fake.NewClientset()
	cs.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.CreateAction).GetObject().(*corev1.Secret).Name
		if name != platformsecrets.NameDashboardEncKey {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, name, nil)
	})

	res, err := seed(context.Background(), cs.CoreV1().Secrets(ns), v)
	if err == nil {
		t.Fatal("expected the injected create failure to surface")
	}
	if !strings.Contains(err.Error(), platformsecrets.NameDashboardEncKey) {
		t.Errorf("error %q should name the Secret that failed", err)
	}
	if res == nil || len(res.Created) == 0 {
		t.Error("a mid-loop failure should still report the Secrets already created")
	}
	for field, val := range map[string]string{
		"SuperuserPassword":  v.SuperuserPassword,
		"ReceiverPassword":   v.ReceiverPassword,
		"DispatcherPassword": v.DispatcherPassword,
		"DashboardPassword":  v.DashboardPassword,
		"WebhookSecret":      v.WebhookSecret,
		"BootstrapToken":     v.BootstrapToken,
		"DashboardEncKey":    v.DashboardEncKey,
	} {
		if strings.Contains(err.Error(), val) {
			t.Errorf("error text leaks the generated %s", field)
		}
	}
}

func sentinels(names ...string) []runtime.Object {
	objs := make([]runtime.Object, 0, len(names))
	for _, n := range names {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"sentinel": []byte("keep")},
		})
	}
	return objs
}

func storedSecrets(t *testing.T, cs *fake.Clientset) map[string]corev1.Secret {
	t.Helper()
	list, err := cs.CoreV1().Secrets(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	out := make(map[string]corev1.Secret, len(list.Items))
	for _, s := range list.Items {
		out[s.Name] = s
	}
	return out
}

// snapshot flattens each Secret's data into one comparable string.
func snapshot(secrets map[string]corev1.Secret) map[string]string {
	out := make(map[string]string, len(secrets))
	for name, sec := range secrets {
		keys := make([]string, 0, len(sec.Data))
		for k := range sec.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k)
			b.WriteString("=")
			b.Write(sec.Data[k])
			b.WriteString(";")
		}
		out[name] = b.String()
	}
	return out
}
