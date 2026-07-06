package doctor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

func probeStoreHealth(t *testing.T, c client.Client, now time.Time) (check.Result, error) {
	t.Helper()
	for _, ck := range doctor.Checks(doctor.Options{Client: c, Now: func() time.Time { return now }}) {
		if ck.ID == doctor.IDSecretsStoreHealth {
			return ck.Probe(context.Background())
		}
	}
	t.Fatalf("check %s not registered", doctor.IDSecretsStoreHealth)
	return check.Result{}, nil
}

func esoObject(kind, name string, ready string, extra func(map[string]interface{})) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "external-secrets.io/v1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": "orkano-apps"},
		"spec":       map[string]interface{}{},
	}}
	if ready != "" {
		u.Object["status"] = map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": ready, "reason": "r", "message": "m"},
			},
		}
	}
	if extra != nil {
		extra(u.Object)
	}
	return u
}

func freshSync(name string, now time.Time, target string) *unstructured.Unstructured {
	return esoObject("ExternalSecret", name, "True", func(o map[string]interface{}) {
		o["spec"] = map[string]interface{}{
			"refreshInterval": "1h",
			"target":          map[string]interface{}{"name": target},
		}
		o["status"].(map[string]interface{})["refreshTime"] = now.Add(-10 * time.Minute).Format(time.RFC3339)
	})
}

func targetSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-apps", Name: name}}
}

func TestSecretsStoreHealth(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	t.Run("eso absent skips", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
					if u, ok := list.(*unstructured.UnstructuredList); ok {
						return &meta.NoKindMatchError{GroupKind: u.GroupVersionKind().GroupKind()}
					}
					return nil
				},
			}).Build()
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusSkip || !strings.Contains(res.Message, "--secrets-vault") {
			t.Fatalf("got %+v, %v — want skip pointing at --secrets-vault", res, err)
		}
	})

	t.Run("nothing configured skips", func(t *testing.T) {
		res, err := probeStoreHealth(t, fakeClient(t), now)
		if err != nil || res.Status != check.StatusSkip {
			t.Fatalf("got %+v, %v — want skip", res, err)
		}
	})

	t.Run("healthy passes", func(t *testing.T) {
		c := fakeClient(t,
			esoObject("SecretStore", "team-vault", "True", nil),
			freshSync("api-stripe", now, "api-stripe"),
			targetSecret("api-stripe"),
		)
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusPass {
			t.Fatalf("got %+v, %v — want pass", res, err)
		}
	})

	t.Run("store not ready fails", func(t *testing.T) {
		c := fakeClient(t, esoObject("SecretStore", "team-vault", "False", nil))
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusFail || !strings.Contains(res.Message, "team-vault") {
			t.Fatalf("got %+v, %v — want fail naming the store", res, err)
		}
	})

	t.Run("store without status fails as unknown", func(t *testing.T) {
		c := fakeClient(t, esoObject("SecretStore", "team-vault", "", nil))
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusFail || !strings.Contains(res.Message, "Unknown") {
			t.Fatalf("got %+v, %v — want fail with Unknown", res, err)
		}
	})

	t.Run("sync not ready fails", func(t *testing.T) {
		c := fakeClient(t,
			esoObject("SecretStore", "team-vault", "True", nil),
			esoObject("ExternalSecret", "api-stripe", "False", nil),
		)
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusFail || !strings.Contains(res.Message, "api-stripe") {
			t.Fatalf("got %+v, %v — want fail naming the sync", res, err)
		}
	})

	t.Run("stale sync fails", func(t *testing.T) {
		stale := esoObject("ExternalSecret", "api-stripe", "True", func(o map[string]interface{}) {
			o["spec"] = map[string]interface{}{"refreshInterval": "1h"}
			o["status"].(map[string]interface{})["refreshTime"] = now.Add(-3 * time.Hour).Format(time.RFC3339)
		})
		c := fakeClient(t, stale, targetSecret("api-stripe"))
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusFail || !strings.Contains(res.Message, "last synced") {
			t.Fatalf("got %+v, %v — want stale fail", res, err)
		}
	})

	t.Run("exactly at the grace boundary passes", func(t *testing.T) {
		edge := esoObject("ExternalSecret", "api-stripe", "True", func(o map[string]interface{}) {
			o["spec"] = map[string]interface{}{"refreshInterval": "1h"}
			o["status"].(map[string]interface{})["refreshTime"] = now.Add(-2 * time.Hour).Format(time.RFC3339)
		})
		c := fakeClient(t, edge, targetSecret("api-stripe"))
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusPass {
			t.Fatalf("got %+v, %v — the 2x boundary itself must pass", res, err)
		}
	})

	t.Run("missing target secret fails", func(t *testing.T) {
		c := fakeClient(t, freshSync("api-stripe", now, "api-stripe"))
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusFail || !strings.Contains(res.Message, "target Secret") {
			t.Fatalf("got %+v, %v — want missing-target fail", res, err)
		}
	})

	t.Run("sync-once refreshInterval 0s passes without freshness", func(t *testing.T) {
		once := esoObject("ExternalSecret", "once", "True", func(o map[string]interface{}) {
			o["spec"] = map[string]interface{}{"refreshInterval": "0s"}
			// refreshTime frozen at the single sync, long ago — healthy.
			o["status"].(map[string]interface{})["refreshTime"] = now.Add(-72 * time.Hour).Format(time.RFC3339)
		})
		res, err := probeStoreHealth(t, fakeClient(t, once, targetSecret("once")), now)
		if err != nil || res.Status != check.StatusPass {
			t.Fatalf("got %+v, %v — refreshInterval 0s is sync-once, not stale", res, err)
		}
	})

	t.Run("CreatedOnce and OnChange policies pass with frozen refreshTime", func(t *testing.T) {
		for _, policy := range []string{"CreatedOnce", "OnChange"} {
			frozen := esoObject("ExternalSecret", "frozen", "True", func(o map[string]interface{}) {
				o["spec"] = map[string]interface{}{
					"refreshInterval": "1h0m0s",
					"refreshPolicy":   policy,
				}
				o["status"].(map[string]interface{})["refreshTime"] = now.Add(-72 * time.Hour).Format(time.RFC3339)
			})
			res, err := probeStoreHealth(t, fakeClient(t, frozen, targetSecret("frozen")), now)
			if err != nil || res.Status != check.StatusPass {
				t.Fatalf("policy %s: got %+v, %v — a frozen refreshTime is by design", policy, res, err)
			}
		}
	})

	t.Run("negative refreshInterval is a probe error", func(t *testing.T) {
		neg := esoObject("ExternalSecret", "neg", "True", func(o map[string]interface{}) {
			o["spec"] = map[string]interface{}{"refreshInterval": "-1h"}
		})
		if _, err := probeStoreHealth(t, fakeClient(t, neg), now); err == nil ||
			strings.Contains(err.Error(), "%!w") {
			t.Fatalf("want a clean probe error for a negative interval, got %v", err)
		}
	})

	t.Run("absent refreshInterval uses the 1h default", func(t *testing.T) {
		stale := esoObject("ExternalSecret", "defaulted", "True", func(o map[string]interface{}) {
			o["status"].(map[string]interface{})["refreshTime"] = now.Add(-3 * time.Hour).Format(time.RFC3339)
		})
		res, err := probeStoreHealth(t, fakeClient(t, stale, targetSecret("defaulted")), now)
		if err != nil || res.Status != check.StatusFail {
			t.Fatalf("got %+v, %v — 3h old under the 1h default must be stale", res, err)
		}
	})

	t.Run("custom target name is the secret checked", func(t *testing.T) {
		c := fakeClient(t, freshSync("api-stripe", now, "custom-target"))
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusFail || !strings.Contains(res.Message, "custom-target") {
			t.Fatalf("got %+v, %v — want missing custom-target fail", res, err)
		}
		c = fakeClient(t, freshSync("api-stripe", now, "custom-target"), targetSecret("custom-target"))
		if res, err := probeStoreHealth(t, c, now); err != nil || res.Status != check.StatusPass {
			t.Fatalf("got %+v, %v — want pass with the custom target present", res, err)
		}
	})

	t.Run("multiple failures aggregate into one message", func(t *testing.T) {
		c := fakeClient(t,
			esoObject("SecretStore", "vault-a", "False", nil),
			esoObject("SecretStore", "vault-b", "True", nil),
			esoObject("ExternalSecret", "broken-sync", "False", nil),
		)
		res, err := probeStoreHealth(t, c, now)
		if err != nil || res.Status != check.StatusFail {
			t.Fatalf("got %+v, %v — want fail", res, err)
		}
		for _, want := range []string{"vault-a", "broken-sync"} {
			if !strings.Contains(res.Message, want) {
				t.Errorf("aggregated message missing %s: %s", want, res.Message)
			}
		}
	})

	t.Run("ready without refreshTime is a probe error", func(t *testing.T) {
		c := fakeClient(t, esoObject("ExternalSecret", "api-stripe", "True", nil))
		if _, err := probeStoreHealth(t, c, now); err == nil {
			t.Fatal("want probe error for Ready-without-refreshTime")
		}
	})

	t.Run("malformed refreshTime is a probe error", func(t *testing.T) {
		bad := esoObject("ExternalSecret", "api-stripe", "True", func(o map[string]interface{}) {
			o["status"].(map[string]interface{})["refreshTime"] = "yesterday"
		})
		if _, err := probeStoreHealth(t, fakeClient(t, bad), now); err == nil {
			t.Fatal("want probe error for malformed refreshTime")
		}
	})

	t.Run("list error is a probe error", func(t *testing.T) {
		for _, failKind := range []string{"SecretStoreList", "ExternalSecretList"} {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == failKind {
							return apierrors.NewServiceUnavailable("apiserver wobble")
						}
						return cl.List(ctx, list, opts...)
					},
				}).Build()
			if _, err := probeStoreHealth(t, c, now); err == nil {
				t.Fatalf("want probe error when the %s list is refused", failKind)
			}
		}
	})
}
