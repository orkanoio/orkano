package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

const (
	canaryImage = doctor.CanaryImage

	probeManagedByLabel = "app.kubernetes.io/managed-by"
	probeManagedByValue = "orkano-preflight"
	probeRoleLabel      = "orkano.io/preflight-role"

	nodeOSLabel = "kubernetes.io/os"

	canaryExitExpectedBlocked int32 = 42
	canaryExitInvalid         int32 = 43
)

var (
	liveProbeWaitBudget   = 3 * time.Minute
	liveProbePollInterval = 2 * time.Second
	scratchCleanupTimeout = time.Minute
)

type scratchProbe func(context.Context, string) (check.Result, error)

// withScratchNamespace creates an isolated namespace for one live probe and
// makes cleanup part of the probe contract. A failed delete is indeterminate:
// the caller must not be told a probe passed while its canaries are left behind.
func withScratchNamespace(ctx context.Context, c client.Client, generateName, psaLevel string, run scratchProbe) (res check.Result, err error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: generateName,
		Labels: map[string]string{
			"app.kubernetes.io/name":             "orkano-preflight",
			probeManagedByLabel:                  probeManagedByValue,
			"pod-security.kubernetes.io/enforce": psaLevel,
		},
	}}
	if err := c.Create(ctx, ns); err != nil {
		return check.Result{}, fmt.Errorf("create scratch namespace: %w", err)
	}
	if ns.Name == "" {
		return check.Result{}, fmt.Errorf("create scratch namespace: API server returned no name")
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), scratchCleanupTimeout)
		defer cancel()
		cleanupErr := deleteScratchNamespace(cleanupCtx, c, ns)
		if cleanupErr == nil {
			return
		}
		if err != nil {
			err = fmt.Errorf("%w; delete scratch namespace %s: %w", err, ns.Name, cleanupErr)
			return
		}
		err = fmt.Errorf("delete scratch namespace %s: %w", ns.Name, cleanupErr)
	}()

	return run(ctx, ns.Name)
}

func deleteScratchNamespace(ctx context.Context, c client.Client, namespace *corev1.Namespace) error {
	if err := c.Delete(ctx, namespace); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	for {
		var current corev1.Namespace
		err := c.Get(ctx, client.ObjectKey{Name: namespace.Name}, &current)
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return fmt.Errorf("wait for deletion: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for deletion: %w", ctx.Err())
		case <-time.After(liveProbePollInterval):
		}
	}
}

// restrictedCanaryPod renders a small, short-lived pod that complies with the
// restricted Pod Security Standard. Individual probes deliberately relax only
// the one field they are testing.
func restrictedCanaryPod(namespace, generateName, role string, command []string, deadline int64) *corev1.Pod {
	no := false
	yes := true
	uid := int64(65534)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    namespace,
			GenerateName: generateName,
			Labels: map[string]string{
				probeManagedByLabel: probeManagedByValue,
				probeRoleLabel:      role,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: ptr.To(deadline),
			// Canary state is disposable; zero grace keeps namespace teardown
			// from obscuring a valid probe result with a normal 30s pod drain.
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
			AutomountServiceAccountToken:  &no,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				RunAsUser:      &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   canaryImage,
				Command: command,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

func waitForPod(ctx context.Context, c client.Client, namespace, name, goal string, complete func(*corev1.Pod) bool) (*corev1.Pod, error) {
	var last string
	for {
		var pod corev1.Pod
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
			last = "read failed: " + err.Error()
		} else {
			last = podDetail(&pod)
			if complete(&pod) {
				return &pod, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("canary pod %s/%s did not reach %s (last state: %s): %w", namespace, name, goal, last, ctx.Err())
		case <-time.After(liveProbePollInterval):
		}
	}
}

func waitForPodTerminal(ctx context.Context, c client.Client, namespace, name string) (*corev1.Pod, error) {
	return waitForPod(ctx, c, namespace, name, "a terminal phase", func(pod *corev1.Pod) bool {
		return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
	})
}

func terminalExitCode(pod *corev1.Pod) (int32, bool) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated != nil {
			return status.State.Terminated.ExitCode, true
		}
		if status.LastTerminationState.Terminated != nil {
			return status.LastTerminationState.Terminated.ExitCode, true
		}
	}
	return 0, false
}

func podDetail(pod *corev1.Pod) string {
	detail := "phase " + string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		detail += "/" + pod.Status.Reason
	}
	if pod.Status.Message != "" {
		detail += ": " + pod.Status.Message
	}
	for _, status := range pod.Status.ContainerStatuses {
		switch {
		case status.State.Waiting != nil:
			detail += ", container " + status.Name + " waiting " + status.State.Waiting.Reason
			if status.State.Waiting.Message != "" {
				detail += ": " + status.State.Waiting.Message
			}
		case status.State.Terminated != nil:
			detail += fmt.Sprintf(", container %s exited %d", status.Name, status.State.Terminated.ExitCode)
			if status.State.Terminated.Reason != "" {
				detail += " (" + status.State.Terminated.Reason + ")"
			}
			if status.State.Terminated.Message != "" {
				detail += ": " + status.State.Terminated.Message
			}
		}
	}
	return detail
}

type buildNode struct {
	name string
}

type nodeCanary struct {
	node buildNode
	pod  *corev1.Pod
}

func createNodeCanaries(ctx context.Context, c client.Client, namespace string, nodes []buildNode, build func(buildNode) *corev1.Pod) ([]nodeCanary, error) {
	canaries := make([]nodeCanary, 0, len(nodes))
	for _, node := range nodes {
		pod := build(node)
		pod.Namespace = namespace
		pinCanaryToNode(pod, node)
		if err := c.Create(ctx, pod); err != nil {
			return nil, fmt.Errorf("create canary for node %s: %w", node.name, err)
		}
		canaries = append(canaries, nodeCanary{node: node, pod: pod})
	}
	return canaries, nil
}

func pinCanaryToNode(pod *corev1.Pod, node buildNode) {
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{
						Key:      "metadata.name",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{node.name},
					}},
				}},
			},
		},
	}
}

// eligibleBuildNodes is the set a normal Linux Build pod can currently reach:
// Ready, schedulable nodes without a hard taint. The probes pin each canary to
// one of these nodes so a green result cannot hide a bad secondary worker.
func eligibleBuildNodes(ctx context.Context, c client.Client) ([]buildNode, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes for build canaries (preflight identity needs node list): %w", err)
	}

	eligible := make([]buildNode, 0, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !nodeReady(node) || node.Spec.Unschedulable || hasHardSchedulingTaint(node) {
			continue
		}
		os, ok := node.Labels[nodeOSLabel]
		if !ok || os == "" {
			return nil, fmt.Errorf("node %s is Ready and schedulable but has no %s label", node.Name, nodeOSLabel)
		}
		if os != "linux" {
			continue
		}
		eligible = append(eligible, buildNode{name: node.Name})
	}

	sort.Slice(eligible, func(i, j int) bool { return eligible[i].name < eligible[j].name })
	return eligible, nil
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hasHardSchedulingTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

func appArmorFailureEvidence(c client.Client, namespace, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err == nil {
		if detail := podDetail(&pod); strings.Contains(strings.ToLower(detail), "apparmor") {
			return detail, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read AppArmor canary: %w", err)
	}

	var events corev1.EventList
	if err := c.List(ctx, &events, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list events for AppArmor canary: %w", err)
	}
	for _, event := range events.Items {
		if event.InvolvedObject.Name != name {
			continue
		}
		detail := strings.TrimSpace(event.Reason + ": " + event.Message)
		if strings.Contains(strings.ToLower(detail), "apparmor") {
			return detail, nil
		}
	}
	return "", nil
}

func nodeNames(nodes []buildNode) string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.name)
	}
	return strings.Join(names, ", ")
}
