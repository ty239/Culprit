package collect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/culprit/culprit/internal/snapshot"
)

// testdata/node-culprit-control-plane.yaml is a real copy of the kind
// cluster's only node, taken with:
//
//	kubectl get node culprit-control-plane -o yaml --show-managed-fields=false
func loadNode(t *testing.T, name string) *corev1.Node {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "node-"+name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var node corev1.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		t.Fatalf("parse node-%s.yaml: %v", name, err)
	}
	return &node
}

// extraNode is a second, made-up worker node.
func extraNode(name string, mutate func(*corev1.Node)) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/hostname": name}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	if mutate != nil {
		mutate(n)
	}
	return n
}

func nodeNames(snap *snapshot.Snapshot) []string {
	var out []string
	for _, n := range snap.Nodes {
		out = append(out, n.Name)
	}
	return out
}

func collectNodesFor(t *testing.T, pod *corev1.Pod, objs ...runtime.Object) *snapshot.Snapshot {
	t.Helper()
	c := New(fake.NewSimpleClientset(append([]runtime.Object{pod}, objs...)...))
	c.FetchLogs = fakeLogs{}.fetch
	snap, err := c.Collect(context.Background(), ns, pod.Name)
	if err != nil {
		t.Fatalf("Collect(%s): %v", pod.Name, err)
	}
	return snap
}

// --- Which nodes ------------------------------------------------------------

// A placed pod gets only its own node, even when there are others.
func TestNodes_PlacedPodGetsOnlyItsNode(t *testing.T) {
	snap := collectNodesFor(t, loadPod(t, "crash-api"),
		loadNode(t, "culprit-control-plane"), extraNode("worker-2", nil))

	if got := nodeNames(snap); len(got) != 1 || got[0] != "culprit-control-plane" {
		t.Errorf("nodes = %v, want only culprit-control-plane", got)
	}
}

// Rule 2: a Pending pod has no node, so collect every node.
func TestNodes_PendingPodGetsAllNodes(t *testing.T) {
	snap := collectNodesFor(t, loadPod(t, "pending-api"),
		loadNode(t, "culprit-control-plane"), extraNode("worker-2", nil))

	got := nodeNames(snap)
	if len(got) != 2 || got[0] != "culprit-control-plane" || got[1] != "worker-2" {
		t.Errorf("nodes = %v, want both nodes, sorted", got)
	}
}

// --- What's copied from the real node --------------------------------------

func TestNodes_RealNodeFields(t *testing.T) {
	snap := collectNodesFor(t, loadPod(t, "crash-api"), loadNode(t, "culprit-control-plane"))
	n := snap.Nodes[0]

	cpu := n.Allocatable[corev1.ResourceCPU]
	if cpu.Cmp(resource.MustParse("16")) != 0 {
		t.Errorf("allocatable cpu = %s, want 16", cpu.String())
	}
	if _, ok := n.Allocatable[corev1.ResourceMemory]; !ok {
		t.Error("allocatable memory missing")
	}
	if len(n.Capacity) == 0 {
		t.Error("capacity missing")
	}

	conds := map[string]string{}
	for _, c := range n.Conditions {
		conds[c.Type] = c.Status
	}
	for typ, want := range map[string]string{"Ready": "True", "MemoryPressure": "False", "DiskPressure": "False", "PIDPressure": "False"} {
		if conds[typ] != want {
			t.Errorf("condition %s = %q, want %q", typ, conds[typ], want)
		}
	}

	if !contains(n.Images, "docker.io/library/python:3.12-slim") {
		t.Errorf("images should contain python:3.12-slim, got %v", n.Images)
	}
	if n.ImagesMayBeIncomplete {
		t.Error("12 images is below the kubelet limit; the list is complete")
	}
	if n.Labels["kubernetes.io/hostname"] != "culprit-control-plane" {
		t.Errorf("labels = %v, want the hostname label", n.Labels)
	}
}

// Taints, cordoning and bad conditions are all copied.
func TestNodes_TaintsAndProblems(t *testing.T) {
	sick := extraNode("worker-2", func(n *corev1.Node) {
		n.Spec.Unschedulable = true
		n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue,
			Reason: "KubeletHasDiskPressure", Message: "kubelet has disk pressure",
		})
	})
	snap := collectNodesFor(t, loadPod(t, "pending-api"), sick)

	n := snap.Nodes[0]
	if !n.Unschedulable {
		t.Error("unschedulable not copied")
	}
	if len(n.Taints) != 1 || n.Taints[0].Key != "dedicated" || n.Taints[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("taints = %+v", n.Taints)
	}
	var disk *snapshot.NodeCondition
	for i := range n.Conditions {
		if n.Conditions[i].Type == "DiskPressure" {
			disk = &n.Conditions[i]
		}
	}
	if disk == nil || disk.Status != "True" || disk.Message == "" {
		t.Errorf("DiskPressure = %+v, want True with its message", disk)
	}
}

// An image with several names (tag + digest) keeps all of them, once each.
func TestNodes_ImageNames(t *testing.T) {
	n := extraNode("worker-2", func(n *corev1.Node) {
		n.Status.Images = []corev1.ContainerImage{
			{Names: []string{"docker.io/library/nginx@sha256:abc", "docker.io/library/nginx:1.27"}},
			{Names: []string{"docker.io/library/nginx:1.27"}}, // duplicate
		}
	})
	got := toNode(n).Images
	if len(got) != 2 || !contains(got, "docker.io/library/nginx:1.27") || !contains(got, "docker.io/library/nginx@sha256:abc") {
		t.Errorf("images = %v, want both names once", got)
	}
}

// The kubelet only reports 50 images by default; a full list is flagged so
// "image not on the node" isn't trusted blindly.
func TestNodes_FullImageListIsFlagged(t *testing.T) {
	n := extraNode("worker-2", func(n *corev1.Node) {
		for i := 0; i < kubeletMaxImages; i++ {
			n.Status.Images = append(n.Status.Images, corev1.ContainerImage{Names: []string{fmt.Sprintf("img-%d:1", i)}})
		}
	})
	if !toNode(n).ImagesMayBeIncomplete {
		t.Error("a list of 50 images should be flagged as possibly incomplete")
	}
}

// --- Rules -----------------------------------------------------------------

// Rule 1: the pod's node no longer exists. That's evidence, not an error.
func TestNodes_MissingNodeIsEvidence(t *testing.T) {
	snap := collectNodesFor(t, loadPod(t, "crash-api")) // no nodes in the cluster

	if len(snap.Nodes) != 1 || snap.Nodes[0].Name != "culprit-control-plane" || !snap.Nodes[0].Missing {
		t.Errorf("nodes = %+v, want culprit-control-plane marked missing", snap.Nodes)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("errors = %+v, want none", snap.Errors)
	}
}

// Rule 3: reading nodes is denied → recorded, everything else still collected.
func TestNodes_DeniedIsRecordedNotFatal(t *testing.T) {
	for _, tc := range []struct{ pod, verb string }{
		{"crash-api", "get"},    // placed pod → GET its node
		{"pending-api", "list"}, // pending pod → LIST all nodes
	} {
		t.Run(tc.pod, func(t *testing.T) {
			client := fake.NewSimpleClientset(loadPod(t, tc.pod), loadNode(t, "culprit-control-plane"))
			client.PrependReactor(tc.verb, "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("RBAC"))
			})
			c := New(client)
			c.FetchLogs = fakeLogs{}.fetch

			snap, err := c.Collect(context.Background(), ns, tc.pod)
			if err != nil {
				t.Fatalf("Collect should not fail when nodes are denied: %v", err)
			}
			if len(snap.Errors) != 1 || snap.Errors[0].Step != "nodes" || !snap.Errors[0].Denied {
				t.Errorf("errors = %+v, want one denied nodes error", snap.Errors)
			}
			if len(snap.Nodes) != 0 {
				t.Errorf("nodes = %v, want none", nodeNames(snap))
			}
			if snap.Pod == nil {
				t.Error("pod should still be collected")
			}
		})
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
