package server

import (
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readyNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion: "v1.35.5+k3s1",
				OSImage:        "Ubuntu 24.04 LTS",
				Architecture:   "arm64",
			},
		},
	}
}

func notReadyNode(name string) *corev1.Node {
	node := readyNode(name, nil)
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	node.Spec.Unschedulable = true
	return node
}

func TestListNodes(t *testing.T) {
	store := newFakeStore()
	ck := authedSession(t, store)
	s := apiServer(t, store,
		notReadyNode("node-b"),
		readyNode("node-a", map[string]string{
			"node-role.kubernetes.io/control-plane": "true",
			"node-role.kubernetes.io/etcd":          "true",
		}),
	)

	rec := apiReq(t, s, http.MethodGet, "/api/nodes", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []nodeResponse `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 || resp.Items[0].Name != "node-a" || resp.Items[1].Name != "node-b" {
		t.Fatalf("items not sorted by name: %+v", resp.Items)
	}
	server := resp.Items[0]
	if !server.Ready || server.Status != "Ready" || server.Unschedulable {
		t.Errorf("node-a state = %+v, want Ready and schedulable", server)
	}
	if len(server.Roles) != 2 || server.Roles[0] != "control-plane" || server.Roles[1] != "etcd" {
		t.Errorf("node-a roles = %v, want sorted [control-plane etcd]", server.Roles)
	}
	if server.KubeletVersion != "v1.35.5+k3s1" || server.InternalIP != "192.0.2.10" ||
		server.OSImage != "Ubuntu 24.04 LTS" || server.Architecture != "arm64" {
		t.Errorf("node-a detail drifted: %+v", server)
	}
	worker := resp.Items[1]
	if worker.Ready || worker.Status != "NotReady" || !worker.Unschedulable {
		t.Errorf("node-b state = %+v, want NotReady + unschedulable", worker)
	}
	if len(worker.Roles) != 1 || worker.Roles[0] != "worker" {
		t.Errorf("node-b roles = %v, want the worker fallback", worker.Roles)
	}
}

func TestListNodesRequiresSession(t *testing.T) {
	s := apiServer(t, newFakeStore(), readyNode("node-a", nil))
	rec := apiReq(t, s, http.MethodGet, "/api/nodes", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list nodes = %d, want 401", rec.Code)
	}
}
