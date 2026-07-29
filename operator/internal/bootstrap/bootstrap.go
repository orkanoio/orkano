// Package bootstrap seeds the platform's generate-once Secrets and initial
// repository allowlist from inside the cluster, for the Helm chart's one-shot
// bootstrap Jobs (ADR-0019 decision 6). It is the in-cluster counterpart of
// `orkano init`'s seed path, written through the API instead of over SSH.
//
// Every write is a bare Create. There is no Get, Update, Patch or Delete
// anywhere in this package, so the Job's Role needs only create on Secrets and
// ConfigMaps. Generate-once is enforced by the grant, not by a code path: a
// Helm upgrade cannot rotate a live credential or reset Settings edits even if
// this code were wrong.
package bootstrap

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/orkanoio/orkano/internal/platformsecrets"
)

// Result carries Secret NAMES only. By construction no credential and no
// bootstrap token can leave this package: SeedSecrets returns nothing else and
// writes nothing anywhere, so ADR-0019's "never printed to pod logs" holds
// structurally rather than by convention.
type Result struct {
	Created []string
	Existed []string
}

// SeedSecrets creates every platform Secret that does not already exist in the
// namespace the caller bound the client to. A Secret that exists is left
// byte-untouched and its data is never read.
//
// The plaintext bootstrap token generated here is used only to derive the
// stored sha256 and is then discarded: nobody, including this Job, ever holds a
// usable install token. The real one is minted later by `orkano bootstrap-token`.
func SeedSecrets(ctx context.Context, secrets typedcorev1.SecretInterface) (*Result, error) {
	v, err := platformsecrets.Generate()
	if err != nil {
		return nil, err
	}
	return seed(ctx, secrets, v)
}

func seed(ctx context.Context, secrets typedcorev1.SecretInterface, v platformsecrets.Values) (*Result, error) {
	specs, err := platformsecrets.Specs(v)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for _, s := range specs {
		data := make(map[string][]byte, len(s.Data))
		for k, val := range s.Data {
			data[k] = []byte(val)
		}
		obj := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: s.Name},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		switch _, err := secrets.Create(ctx, obj, metav1.CreateOptions{}); {
		case err == nil:
			res.Created = append(res.Created, s.Name)
		case apierrors.IsAlreadyExists(err):
			res.Existed = append(res.Existed, s.Name)
		default:
			// The partial result carries names only, so the caller can print the
			// progress a retried pod already made.
			return res, fmt.Errorf("create secret %s: %w", s.Name, err)
		}
	}
	return res, nil
}
