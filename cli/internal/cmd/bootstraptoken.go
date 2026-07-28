package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/orkanoio/orkano/internal/platformsecrets"
)

// dashboardDeployment reads the stored hash into its environment once, at
// container start, so a rotation only takes effect after the pod restarts.
const dashboardDeployment = "orkano-dashboard"

// The rollout wait is bounded: a dashboard that is slow to come back must not
// hold the token hostage, so a timeout degrades to a printed caveat. Package
// vars so the command tests do not sleep.
var (
	rolloutWaitTimeout  = 2 * time.Minute
	rolloutPollInterval = 2 * time.Second
)

type bootstrapTokenOptions struct {
	kubeconfig string
}

func newBootstrapTokenCommand() *cobra.Command {
	opt := &bootstrapTokenOptions{}
	cmd := &cobra.Command{
		Use:   "bootstrap-token",
		Short: "Mint a new dashboard bootstrap token (printed once)",
		Long: "Mint the token that redeems the dashboard's first admin account. The token " +
			"is generated on this machine; only its sha256 hash is written to the " +
			"orkano-bootstrap-token Secret in orkano-system, and the plaintext is printed " +
			"exactly once: it cannot be recovered afterwards. Any bootstrap token issued " +
			"earlier stops working. The dashboard reads the hash once at startup, so this " +
			"also restarts deploy/orkano-dashboard and waits for that rollout.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBootstrapToken(cmd.Context(), cmd.OutOrStdout(), opt)
		},
	}

	cmd.Flags().StringVar(&opt.kubeconfig, "kubeconfig", "", "path to the cluster kubeconfig (default: $KUBECONFIG, then ./orkano.kubeconfig)")

	return cmd
}

// newBootstrapTokenClient builds the typed client this command writes through.
// Its own seam (not doctor's): a shared one would double-parse the kubeconfig
// and cross-couple two unrelated commands. A typed clientset hits fixed
// core/v1 and apps/v1 paths, so no discovery round-trip and no scheme.
var newBootstrapTokenClient = func(kubeconfigPath string) (kubernetes.Interface, error) {
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfigPath, err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build cluster client: %w", err)
	}
	return cs, nil
}

// runBootstrapToken's ordering is load-bearing: nothing may fail after the
// plaintext is printed, and the plaintext must never be printed unless its hash
// was stored. So the Secret write is the only fatal step; the dashboard restart
// and the rollout wait degrade to a printed caveat, because exiting non-zero
// next to a stored hash would invite the user to discard a working credential.
func runBootstrapToken(ctx context.Context, out io.Writer, opt *bootstrapTokenOptions) error {
	kubeconfig := resolveKubeconfig(opt.kubeconfig, os.Getenv("KUBECONFIG"))
	cs, err := newBootstrapTokenClient(kubeconfig)
	if err != nil {
		return err
	}

	token, err := platformsecrets.GenerateBootstrapToken()
	if err != nil {
		return err
	}
	if err := storeTokenHash(ctx, cs, platformsecrets.HashToken(token)); err != nil {
		return err
	}
	writef(out, "stored the new token hash in %s/%s\n", platformsecrets.Namespace, platformsecrets.NameBootstrapToken)

	switch err := restartDashboard(ctx, cs); {
	case apierrors.IsNotFound(err):
		writef(out, "deploy/%s not found; it will read the new hash when it first starts\n", dashboardDeployment)
	case err != nil:
		writef(out, "could not restart deploy/%s: %v\n"+
			"Restart it yourself, or the dashboard keeps comparing against the previous hash:\n"+
			"  kubectl -n %s rollout restart deploy/%s\n", dashboardDeployment, err, platformsecrets.Namespace, dashboardDeployment)
	case waitDashboardRollout(ctx, cs):
		writef(out, "dashboard restarted and ready; it now compares against the new hash\n")
	default:
		writef(out, "dashboard restart is still in progress; the new token is rejected until the restarted pod is Ready\n")
	}

	writef(out, "\nBootstrap token (shown once; store it now):\n  %s\n\n"+
		"Only its sha256 hash is stored, so the plaintext token cannot be recovered.\n"+
		"Redeem it at first dashboard login. Any bootstrap token issued earlier no longer\n"+
		"works. If an admin account already exists, redeeming returns \"already\n"+
		"bootstrapped\" and no token can create a second one; recover that account instead.\n", token)
	return nil
}

// storeTokenHash writes the hash into the bootstrap-token Secret, creating it if
// this is a chart install whose seeder has not run (or an `orkano init` that
// never got that far). On an existing Secret only the one key is set, so a key a
// future release adds to this Secret cannot be destroyed by a rotation.
func storeTokenHash(ctx context.Context, cs kubernetes.Interface, hash string) error {
	secrets := cs.CoreV1().Secrets(platformsecrets.Namespace)
	_, err := secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: platformsecrets.NameBootstrapToken, Namespace: platformsecrets.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{platformsecrets.KeyTokenSHA256: []byte(hash)},
	}, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create secret %s: %w", platformsecrets.NameBootstrapToken, err)
	}

	// A concurrent writer is unlikely (this is an interactive command), but a
	// lost update here would leave the printed token unredeemable.
	const attempts = 3
	for i := 0; i < attempts; i++ {
		sec, err := secrets.Get(ctx, platformsecrets.NameBootstrapToken, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read secret %s: %w", platformsecrets.NameBootstrapToken, err)
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[platformsecrets.KeyTokenSHA256] = []byte(hash)
		if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return fmt.Errorf("update secret %s: %w", platformsecrets.NameBootstrapToken, err)
		}
	}
	return fmt.Errorf("update secret %s: conflicted %d times", platformsecrets.NameBootstrapToken, attempts)
}

// restartDashboard stamps the same annotation `kubectl rollout restart` writes,
// so the two mechanisms are interchangeable.
func restartDashboard(ctx context.Context, cs kubernetes.Interface) error {
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339))
	_, err := cs.AppsV1().Deployments(platformsecrets.Namespace).Patch(
		ctx, dashboardDeployment, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// waitDashboardRollout reports whether the restarted pods became Ready. The old
// pod keeps serving the OLD hash until then, so a user who pastes the token
// immediately would get a rejection and re-run the command, rotating again.
func waitDashboardRollout(ctx context.Context, cs kubernetes.Interface) bool {
	deadline := time.Now().Add(rolloutWaitTimeout)
	for {
		dep, err := cs.AppsV1().Deployments(platformsecrets.Namespace).Get(ctx, dashboardDeployment, metav1.GetOptions{})
		if err == nil && rolloutComplete(dep) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(rolloutPollInterval):
		}
	}
}

// rolloutComplete mirrors `kubectl rollout status`, and the third clause is the
// load-bearing one. The dashboard runs a single replica under the default
// RollingUpdate strategy, so maxSurge rounds to 1 and maxUnavailable to 0: the
// new pod is created BEFORE the old one goes away. In that window
// updatedReplicas and availableReplicas both read 1 while the only Ready
// endpoint is still the OLD pod, serving the OLD hash. Requiring every old
// replica to be gone (replicas <= updatedReplicas) is what makes "ready" mean
// "the new hash is live".
func rolloutComplete(dep *appsv1.Deployment) bool {
	if dep.Spec.Replicas == nil || dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	return dep.Status.UpdatedReplicas >= *dep.Spec.Replicas &&
		dep.Status.Replicas <= dep.Status.UpdatedReplicas &&
		dep.Status.AvailableReplicas >= dep.Status.UpdatedReplicas
}
