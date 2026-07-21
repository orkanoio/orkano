package cluster

import (
	"context"
	"fmt"
	"strings"

	storagev1 "k8s.io/api/storage/v1"

	"github.com/orkanoio/orkano/api/check"
)

const (
	defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"
	// The pre-GA annotation some provisioners still stamp; the apiserver's
	// admission plugin honors both.
	betaDefaultStorageClassAnnotation = "storageclass.beta.kubernetes.io/is-default-class"
)

// storageClassDefaultCheck verifies a default StorageClass exists. The
// in-cluster registry claims a PVC directly and the platform Postgres plus
// every catalog database claim through StatefulSet volumeClaimTemplates — all
// without naming a class, so with no default they stay Pending forever and
// the install wedges at the readiness wait.
func storageClassDefaultCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDStorageClassDefault,
		Severity: check.SeverityCritical,
		Summary:  "a default StorageClass exists for the PersistentVolumeClaims the install creates",
		Remediation: "install a storage provisioner (CSI driver) if none exists, then mark one class default: " +
			"kubectl annotate storageclass <name> storageclass.kubernetes.io/is-default-class=true",
		Probe: func(ctx context.Context) (check.Result, error) {
			var scs storagev1.StorageClassList
			if err := opt.Client.List(ctx, &scs); err != nil {
				return check.Result{}, fmt.Errorf("list StorageClasses: %w", err)
			}
			if len(scs.Items) == 0 {
				return check.Result{
					Status: check.StatusFail,
					Message: "no StorageClass exists — the in-cluster registry, the platform Postgres and every " +
						"catalog database claim PersistentVolumes and would stay Pending forever",
				}, nil
			}

			// Several classes marked default is fine on every supported minor:
			// since v1.26 the apiserver assigns the newest default instead of
			// refusing the PVC, so ≥1 default passes and the message names
			// them all.
			var defaults []string
			for i := range scs.Items {
				if isDefaultStorageClass(&scs.Items[i]) {
					defaults = append(defaults, scs.Items[i].Name)
				}
			}
			if len(defaults) == 0 {
				return check.Result{
					Status: check.StatusFail,
					Message: fmt.Sprintf("%d StorageClass(es) exist but none is marked default — the install's "+
						"PersistentVolumeClaims name no class and would stay Pending", len(scs.Items)),
				}, nil
			}
			return check.Result{
				Status:  check.StatusPass,
				Message: fmt.Sprintf("default StorageClass %s (%d present)", strings.Join(defaults, ", "), len(scs.Items)),
			}, nil
		},
	}
}

func isDefaultStorageClass(sc *storagev1.StorageClass) bool {
	return sc.Annotations[defaultStorageClassAnnotation] == "true" ||
		sc.Annotations[betaDefaultStorageClassAnnotation] == "true"
}
