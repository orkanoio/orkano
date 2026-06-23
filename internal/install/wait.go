package install

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// waitPollInterval is how often waitReady re-checks; tests shrink it.
var waitPollInterval = 5 * time.Second

// waitReady polls each target until it reports at least one ready replica, or
// the timeout elapses. A not-found result (k3s has not applied the manifest
// yet) and a transient transport error both count as "keep waiting", never a
// hard failure — only the timeout fails, listing whatever is still pending.
func (n *node) waitReady(ctx context.Context, targets []Workload, timeout time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	pending := append([]Workload(nil), targets...)
	for {
		var still []Workload
		for _, w := range pending {
			ready, err := n.workloadReady(wait, w)
			switch {
			case err != nil:
				n.logf("waiting for %s/%s: %v", w.Kind, w.Name, err)
				still = append(still, w)
			case ready:
				n.logf("%s/%s is ready", w.Kind, w.Name)
			default:
				still = append(still, w)
			}
		}
		if len(still) == 0 {
			return nil
		}
		pending = still

		select {
		case <-wait.Done():
			names := make([]string, len(pending))
			for i, w := range pending {
				names[i] = w.Namespace + "/" + w.Kind + "/" + w.Name
			}
			return fmt.Errorf("install: components not ready within %s: %s", timeout, strings.Join(names, ", "))
		case <-ticker.C:
		}
	}
}

// waitNamespace polls until the namespace exists, so the generated Secrets can
// be created into it after k3s applies the namespace manifest. A transient
// transport error or not-found both mean keep waiting; only the timeout fails.
func (n *node) waitNamespace(ctx context.Context, ns string, timeout time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	for {
		res, err := n.r.Run(wait, fmt.Sprintf("%s%s kubectl get namespace %s -o name", n.sudo, k3sBin, ns))
		if err == nil && res.ExitStatus == 0 {
			return nil
		}

		select {
		case <-wait.Done():
			return fmt.Errorf("install: namespace %s not created within %s", ns, timeout)
		case <-ticker.C:
		}
	}
}

// workloadReady reports whether the workload has at least one ready replica.
// A not-found resource (still being applied) is not-ready, not an error; only a
// transport failure is an error.
func (n *node) workloadReady(ctx context.Context, w Workload) (bool, error) {
	cmd := fmt.Sprintf("%s%s kubectl -n %s get %s %s -o jsonpath='{.status.readyReplicas}'",
		n.sudo, k3sBin, w.Namespace, w.Kind, w.Name)
	res, err := n.r.Run(ctx, cmd)
	if err != nil {
		return false, err
	}
	if res.ExitStatus != 0 {
		return false, nil // not applied yet
	}
	count, _ := strconv.Atoi(strings.TrimSpace(res.Stdout))
	return count >= 1, nil
}
