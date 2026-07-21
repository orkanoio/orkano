package doctor

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
)

// IDEtcdSnapshotAge is PERMANENT — it appears in --json output and CI configs.
const IDEtcdSnapshotAge = "backup.etcd-snapshot-age"

// DefaultMaxSnapshotAge is the oldest a cluster's newest usable etcd snapshot
// may be before the check fails: two of `orkano init`'s default 12-hour
// snapshot windows plus an hour of slack, so a single missed cron (node busy,
// brief downtime) never fires but a broken schedule does. The hour of slack
// also absorbs modest node-vs-doctor clock drift (ages compare a node-stamped
// CR time against the doctor host's clock). Options.MaxSnapshotAge widens it
// for a customized snapshot cron — Go surface only for now; neither init nor
// doctor exposes a flag for either knob today.
const DefaultMaxSnapshotAge = 25 * time.Hour

// etcdSnapshotFileListGVK reads k3s's cluster-scoped snapshot records as
// unstructured — no k3s API dep. k3s maintains one ETCDSnapshotFile per
// snapshot it has taken (scheduled and on-demand).
var etcdSnapshotFileListGVK = schema.GroupVersionKind{Group: "k3s.cattle.io", Version: "v1", Kind: "ETCDSnapshotFileList"}

// etcdMemberAnnotation marks a node that runs an embedded-etcd member. The
// signal is authoritative, not advisory: k3s's etcd metadata controller
// continuously stamps it on every embedded-etcd server and explicitly strips
// it when the node runs without etcd (kine/sqlite/external datastore) — so
// its absence from ALL nodes means there genuinely is no etcd to snapshot
// (a legitimately-inapplicable skip, unlike the tls check's always-installed
// cert-manager), and hand-tampering cannot fake it for long.
const etcdMemberAnnotation = "etcd.k3s.cattle.io/node-name"

// etcdSnapshotAgeCheck verifies the scheduled etcd snapshots — the install's
// only built-in backup — are actually being taken. `orkano init` configures a
// 12-hour schedule on every install (single-node runs embedded etcd too), and
// nothing alerts when the schedule silently stops working; this check is that
// alert. Warning severity per the repo rule the tls check states: the
// critical tier (and doctor's CI exit gate) is reserved for the security
// invariants, and backup age is availability/DR, not an INV item — it drags
// the hardening score instead.
func etcdSnapshotAgeCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDEtcdSnapshotAge,
		Severity: check.SeverityWarning,
		// "etcd snapshots", not "scheduled": an on-demand `etcd-snapshot save`
		// is indistinguishable in the CR and is a real backup all the same.
		Summary: "etcd snapshots are recent",
		// Absolute k3s path on purpose: RHEL-family sudo secure_path excludes
		// /usr/local/bin, so a bare `sudo k3s …` is command-not-found there
		// (the internal/k3s k3sBin precedent).
		Remediation: "on a server node, list snapshots with `sudo /usr/local/bin/k3s etcd-snapshot ls`, check `journalctl -u k3s` for snapshot errors, " +
			"and take one now with `sudo /usr/local/bin/k3s etcd-snapshot save`; confirm etcd-snapshot-schedule-cron is set in /etc/rancher/k3s/config.yaml",
		Probe: func(ctx context.Context) (check.Result, error) {
			now := opt.now()
			maxAge := opt.maxSnapshotAge()

			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(etcdSnapshotFileListGVK)
			err := opt.Client.List(ctx, list)
			switch {
			case meta.IsNoMatchError(err):
				// Modern k3s installs this CRD unconditionally (any datastore
				// backend), so this branch is reachable only on a non-k3s
				// cluster or a user-pinned k3s predating the CR — both places
				// where snapshot age simply cannot be observed via the API
				// while backups may still work. Unlike cert-manager (vendored,
				// unconfigurable, always present — its absence FAILs the tls
				// check), that is legitimately inapplicable, so skip.
				return check.Result{
					Status:  check.StatusSkip,
					Message: "the k3s ETCDSnapshotFile CRD is not installed (non-k3s cluster, or k3s too old to record snapshots as CRs) — snapshot age is not observable via the API",
				}, nil
			case err != nil:
				return check.Result{}, fmt.Errorf("list ETCDSnapshotFiles: %w", err)
			}

			if len(list.Items) == 0 {
				return noSnapshotsResult(ctx, opt, now, maxAge)
			}

			var (
				newest     time.Time
				newestName string
				newestNode string
				usable     int
				lastErr    string
			)
			for i := range list.Items {
				s := &list.Items[i]
				if msg, failed, err := snapshotError(s); err != nil {
					return check.Result{}, fmt.Errorf("snapshot %s: %w", s.GetName(), err)
				} else if failed {
					lastErr = msg
					continue
				}
				created, ok, err := nestedTime(s, "status", "creationTime")
				if err != nil {
					return check.Result{}, fmt.Errorf("snapshot %s: %w", s.GetName(), err)
				}
				if !ok {
					// No creation time and no error: the snapshot is still in
					// flight; it neither proves nor breaks freshness.
					continue
				}
				usable++
				if created.After(newest) {
					newest = created
					newestName = s.GetName()
					newestNode, _, _ = unstructured.NestedString(s.Object, "spec", "nodeName")
				}
			}

			if usable == 0 {
				msg := fmt.Sprintf("none of the %d etcd snapshot record(s) is usable", len(list.Items))
				if lastErr != "" {
					// A sample, not "the latest": list order carries no
					// chronology and failed records may lack timestamps.
					msg += " — sample error: " + lastErr
				}
				return check.Result{Status: check.StatusFail, Message: msg}, nil
			}
			if age := now.Sub(newest); age > maxAge {
				return check.Result{
					Status: check.StatusFail,
					Message: fmt.Sprintf("newest usable etcd snapshot %s (node %s) is %s old — older than the %s limit, the schedule has stopped working",
						newestName, newestNode, fmtDuration(age), fmtDuration(maxAge)),
				}, nil
			}
			return check.Result{
				Status: check.StatusPass,
				Message: fmt.Sprintf("newest of %d usable etcd snapshot(s) is %s (node %s), taken %s ago",
					usable, newestName, newestNode, fmtDuration(now.Sub(newest))),
			}, nil
		},
	}
}

// noSnapshotsResult judges a cluster with zero snapshot records: fine on a
// cluster without embedded etcd or younger than its first snapshot window,
// broken on one that should have snapshotted by now.
func noSnapshotsResult(ctx context.Context, opt Options, now time.Time, maxAge time.Duration) (check.Result, error) {
	var nodes corev1.NodeList
	if err := opt.Client.List(ctx, &nodes); err != nil {
		return check.Result{}, fmt.Errorf("list Nodes: %w", err)
	}
	etcdMembers := 0
	for i := range nodes.Items {
		if _, ok := nodes.Items[i].Annotations[etcdMemberAnnotation]; ok {
			etcdMembers++
		}
	}
	if etcdMembers == 0 {
		return check.Result{
			Status:  check.StatusSkip,
			Message: "no node runs an embedded-etcd member — there is no etcd to snapshot",
		}, nil
	}

	var kubeSystem corev1.Namespace
	if err := opt.Client.Get(ctx, client.ObjectKey{Name: "kube-system"}, &kubeSystem); err != nil {
		return check.Result{}, fmt.Errorf("read namespace kube-system: %w", err)
	}
	// <= so a cluster exactly at the limit counts as within it, matching the
	// snapshots-present path (which fails only when age exceeds maxAge).
	if age := now.Sub(kubeSystem.CreationTimestamp.Time); age <= maxAge {
		return check.Result{
			Status:  check.StatusSkip,
			Message: fmt.Sprintf("no snapshots yet, but the cluster is only %s old — the first scheduled window has not elapsed", fmtDuration(age)),
		}, nil
	}
	return check.Result{
		Status: check.StatusFail,
		Message: fmt.Sprintf("%d embedded-etcd member(s) and zero snapshots on a cluster older than %s — scheduled snapshots are not running",
			etcdMembers, fmtDuration(maxAge)),
	}, nil
}

// snapshotError reads an ETCDSnapshotFile's status.error; a present error
// marks the snapshot attempt as failed (its message is best-effort).
func snapshotError(s *unstructured.Unstructured) (msg string, failed bool, err error) {
	errObj, ok, err := unstructured.NestedMap(s.Object, "status", "error")
	if err != nil {
		return "", false, fmt.Errorf("read status.error: %w", err)
	}
	if !ok {
		return "", false, nil
	}
	msg, _, _ = unstructured.NestedString(errObj, "message")
	return msg, true, nil
}
