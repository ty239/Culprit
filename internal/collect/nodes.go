package collect

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/culprit/culprit/internal/snapshot"
)

// kubeletMaxImages is how many images the kubelet reports per node by
// default (--node-status-max-images). A full list may be cut short.
const kubeletMaxImages = 50

// collectNodes reads the node the pod runs on. If the pod hasn't been placed
// on a node yet (Pending), it reads every node instead, because their
// capacity, taints and labels are what explain why it can't be placed.
func (c *Collector) collectNodes(ctx context.Context, snap *snapshot.Snapshot) {
	nodeName := snap.Pod.Spec.NodeName

	if nodeName != "" {
		node, err := c.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// The pod points at a node that's gone: record it as missing.
			snap.Nodes = append(snap.Nodes, snapshot.Node{Name: nodeName, Missing: true})
		case err != nil:
			record(snap, "nodes", "nodes/"+nodeName, err)
		default:
			snap.Nodes = append(snap.Nodes, toNode(node))
		}
		return
	}

	list, err := c.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		record(snap, "nodes", "nodes", err)
		return
	}
	for i := range list.Items {
		snap.Nodes = append(snap.Nodes, toNode(&list.Items[i]))
	}
	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })

	c.collectNodeRequests(ctx, snap)
}

// collectNodeRequests adds up what the pods already on each node have asked
// for. A node can be big enough for the pod and still have no room left;
// only the other pods' requests show that.
func (c *Collector) collectNodeRequests(ctx context.Context, snap *snapshot.Snapshot) {
	pods, err := c.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		record(snap, "nodes", "pods (all namespaces)", err)
		return
	}

	for i := range snap.Nodes {
		node := &snap.Nodes[i]
		var cpu, memory resource.Quantity
		for _, p := range pods.Items {
			// Finished pods no longer hold their requests.
			if p.Spec.NodeName != node.Name || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			node.PodCount++
			cpu.Add(podRequest(&p, corev1.ResourceCPU))
			memory.Add(podRequest(&p, corev1.ResourceMemory))
		}
		node.Requested = corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}
	}
}

// podRequest is what the scheduler sets aside for a pod: its containers'
// requests added up, or its biggest init container if that is larger.
func podRequest(pod *corev1.Pod, name corev1.ResourceName) resource.Quantity {
	var total resource.Quantity
	for _, c := range pod.Spec.Containers {
		if q, ok := c.Resources.Requests[name]; ok {
			total.Add(q)
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if q, ok := c.Resources.Requests[name]; ok && q.Cmp(total) > 0 {
			total = q.DeepCopy()
		}
	}
	return total
}

// toNode copies the fields that explain scheduling and image problems.
func toNode(n *corev1.Node) snapshot.Node {
	out := snapshot.Node{
		Name:          n.Name,
		Allocatable:   n.Status.Allocatable,
		Capacity:      n.Status.Capacity,
		Unschedulable: n.Spec.Unschedulable,
		Taints:        n.Spec.Taints,
		Labels:        n.Labels,
	}
	for _, c := range n.Status.Conditions {
		out.Conditions = append(out.Conditions, snapshot.NodeCondition{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}

	// Each image can have several names (tags and digests); keep them all.
	seen := map[string]bool{}
	for _, img := range n.Status.Images {
		for _, name := range img.Names {
			if !seen[name] {
				seen[name] = true
				out.Images = append(out.Images, name)
			}
		}
	}
	sort.Strings(out.Images)
	out.ImagesMayBeIncomplete = len(n.Status.Images) >= kubeletMaxImages

	return out
}
