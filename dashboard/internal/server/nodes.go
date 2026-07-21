package server

import (
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodeRolePrefix is the upstream convention for role labels — k3s stamps
// control-plane/master/etcd on servers; agents carry none.
const nodeRolePrefix = "node-role.kubernetes.io/"

// nodeResponse is the read DTO for the Settings page's node inventory. It is
// derived entirely from the Node object's public identity and status — no
// secrets, no kubelet credentials, nothing an operator could not read with
// kubectl get nodes.
type nodeResponse struct {
	Name              string      `json:"name"`
	Roles             []string    `json:"roles"`
	Ready             bool        `json:"ready"`
	Status            string      `json:"status"`
	Unschedulable     bool        `json:"unschedulable"`
	KubeletVersion    string      `json:"kubeletVersion"`
	OSImage           string      `json:"osImage"`
	Architecture      string      `json:"architecture"`
	InternalIP        string      `json:"internalIP"`
	CreationTimestamp metav1.Time `json:"creationTimestamp"`
}

// handleListNodes lists cluster nodes through the impersonated viewer — the
// read rides the orkano-viewer ClusterRole's nodes get/list, the dashboard's
// only cluster-scoped read besides the pinned impersonate verb itself.
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	var list corev1.NodeList
	if err := s.cfg.ViewerClient.List(r.Context(), &list); err != nil {
		s.writeK8sError(w, "nodes.list", err)
		return
	}
	out := make([]nodeResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, nodeToResponse(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func nodeToResponse(node *corev1.Node) nodeResponse {
	resp := nodeResponse{
		Name:              node.Name,
		Roles:             nodeRoles(node),
		Status:            "Unknown",
		Unschedulable:     node.Spec.Unschedulable,
		KubeletVersion:    node.Status.NodeInfo.KubeletVersion,
		OSImage:           node.Status.NodeInfo.OSImage,
		Architecture:      node.Status.NodeInfo.Architecture,
		CreationTimestamp: node.CreationTimestamp,
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}
		if cond.Status == corev1.ConditionTrue {
			resp.Ready, resp.Status = true, "Ready"
		} else {
			resp.Status = "NotReady"
		}
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			resp.InternalIP = addr.Address
			break
		}
	}
	return resp
}

// nodeRoles reads the node-role.kubernetes.io/* labels; a node without any
// (a k3s agent, a plain worker) reads as "worker" so the table never shows an
// empty cell for the most common shape.
func nodeRoles(node *corev1.Node) []string {
	var roles []string
	for label := range node.Labels {
		if role, ok := strings.CutPrefix(label, nodeRolePrefix); ok && role != "" {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return []string{"worker"}
	}
	sort.Strings(roles)
	return roles
}
