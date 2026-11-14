package diagnose

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/culprit/culprit/internal/snapshot"
)

// scheduling handles a pod the scheduler couldn't place on a node.
// One cause per symptom: the first rule that explains it wins.
func (b *builder) scheduling() {
	pod := b.snap.Pod
	cond := podCondition(pod, corev1.PodScheduled)
	if cond == nil || cond.Status != corev1.ConditionFalse {
		return
	}

	symptom := link{
		object:   b.podObject(),
		summary:  "Pod is Pending: the scheduler can't place it on any node",
		evidence: []string{b.ev("pod.status.conditions[PodScheduled]", joinNonEmpty(": ", cond.Reason, cond.Message))},
	}

	rules := []func(msg string) (links []link, rooted bool){
		b.storageCause,
		b.resourceCause,
		b.placementCause,
	}
	for _, rule := range rules {
		rest, rooted := rule(cond.Message)
		if len(rest) == 0 {
			continue
		}
		b.add(rooted, append([]link{symptom}, rest...))
		return
	}
	b.chain(symptom)
}

// storageCause follows the pod's disks: PVC -> StorageClass.
func (b *builder) storageCause(string) ([]link, bool) {
	snap := b.snap
	for _, pvc := range snap.PVCs {
		object := fmt.Sprintf("PersistentVolumeClaim %s/%s", snap.Namespace, pvc.Name)
		src := fmt.Sprintf("pvcs[%s]", pvc.Name)

		switch {
		case pvc.State == snapshot.Missing:
			return []link{{
				object:   object,
				summary:  fmt.Sprintf("PVC %q doesn't exist, but the pod mounts it (volume %s)", pvc.Name, strings.Join(pvc.UsedBy, ", ")),
				evidence: []string{b.ev(src+".state", snapshot.Missing)},
				fix:      fmt.Sprintf("Create PersistentVolumeClaim %q in namespace %q, or fix the claimName in the pod.", pvc.Name, snap.Namespace),
			}}, true

		case pvc.State != snapshot.Found:
			continue

		case pvc.Phase == string(corev1.ClaimLost):
			return []link{{
				object:   object,
				summary:  fmt.Sprintf("PVC %q has lost its disk (PersistentVolume %q is gone)", pvc.Name, pvc.VolumeName),
				evidence: []string{b.ev(src+".phase", pvc.Phase)},
				fix:      "Restore the PersistentVolume, or delete the PVC and create it again (the data on the old disk is not recovered by this).",
			}}, true

		case pvc.Phase != string(corev1.ClaimPending):
			continue
		}

		pending := link{
			object:   object,
			summary:  fmt.Sprintf("PVC %q is Pending: no disk has been bound to it", pvc.Name),
			evidence: []string{b.ev(src+".phase", pvc.Phase)},
		}
		size := joinNonEmpty(", ", pvc.Requested, strings.Join(pvc.AccessModes, "+"))

		// The PVC names no class: the cluster's default one applies.
		if pvc.UsesDefaultClass {
			if b.defaultClass() != nil {
				continue // a default exists; the cluster assigns it to the PVC by itself
			}
			if b.readFailed("storageclass") {
				return []link{pending}, false
			}
			return []link{pending, {
				object:  "StorageClass (default)",
				summary: fmt.Sprintf("PVC %q names no StorageClass, and the cluster has no default one", pvc.Name),
				evidence: []string{
					b.ev(src+".storageClassName", "(not set)"),
					b.ev("storageClasses", "none is marked as the default"),
				},
				fix: "Set storageClassName on the PVC, or mark a StorageClass as the default.",
			}}, true
		}

		noProvisioner := link{
			object:   object,
			summary:  fmt.Sprintf("No existing PersistentVolume fits PVC %q (%s), and nothing creates one automatically", pvc.Name, size),
			evidence: []string{b.ev(src+".storageClassName", orNone(pvc.StorageClassName))},
			fix:      "Create a PersistentVolume that matches the PVC's size, access mode and class, or use a StorageClass with a provisioner.",
		}
		if pvc.StorageClassName == "" {
			return []link{pending, noProvisioner}, true
		}

		sc := b.storageClass(pvc.StorageClassName)
		scSrc := fmt.Sprintf("storageClasses[%s]", pvc.StorageClassName)
		scObject := "StorageClass " + pvc.StorageClassName
		switch {
		case sc == nil || sc.State == snapshot.Unreadable:
			return []link{pending}, false

		case sc.State == snapshot.Missing:
			return []link{pending, {
				object:  scObject,
				summary: fmt.Sprintf("StorageClass %q doesn't exist, so nothing can create the disk", sc.Name),
				evidence: []string{
					b.ev(src+".storageClassName", pvc.StorageClassName),
					b.ev(scSrc+".state", snapshot.Missing),
				},
				fix: fmt.Sprintf("Create StorageClass %q, or recreate the PVC with a class that exists (`kubectl get storageclass` lists them).", sc.Name),
			}}, true

		case sc.VolumeBindingMode == "WaitForFirstConsumer":
			// Normal: the disk is only made once the pod has a node, so this
			// PVC is waiting on the pod, not the other way round.
			continue

		case sc.Provisioner == "kubernetes.io/no-provisioner":
			noProvisioner.evidence = append(noProvisioner.evidence, b.ev(scSrc+".provisioner", sc.Provisioner))
			return []link{pending, noProvisioner}, true

		default:
			return []link{pending, {
				object:   scObject,
				summary:  fmt.Sprintf("StorageClass %q exists, but its provisioner (%s) hasn't created a disk; the PVC's own events will say why", sc.Name, sc.Provisioner),
				evidence: []string{b.ev(scSrc+".provisioner", sc.Provisioner)},
			}}, false
		}
	}
	return nil, false
}

var insufficient = regexp.MustCompile(`Insufficient ([A-Za-z0-9._/-]*[A-Za-z0-9])`)

// resourceCause compares what the pod asks for with what the nodes have.
func (b *builder) resourceCause(msg string) ([]link, bool) {
	pod := b.snap.Pod
	for _, m := range insufficient.FindAllStringSubmatch(msg, -1) {
		name := corev1.ResourceName(m[1])
		want := podRequest(pod, name)
		if want.IsZero() {
			continue
		}

		var best *snapshot.Node
		var bestQty resource.Quantity
		for i, n := range b.snap.Nodes {
			if n.Missing {
				continue
			}
			if q := n.Allocatable[name]; best == nil || q.Cmp(bestQty) > 0 {
				best, bestQty = &b.snap.Nodes[i], q
			}
		}
		if best == nil {
			continue
		}

		evidence := []string{}
		for _, c := range pod.Spec.Containers {
			if q, ok := c.Resources.Requests[name]; ok {
				evidence = append(evidence, b.ev(fmt.Sprintf("pod.spec.containers[%s].resources.requests.%s", c.Name, name), q.String()))
			}
		}
		for _, c := range pod.Spec.InitContainers {
			if q, ok := c.Resources.Requests[name]; ok {
				evidence = append(evidence, b.ev(fmt.Sprintf("pod.spec.initContainers[%s].resources.requests.%s", c.Name, name), q.String()))
			}
		}
		evidence = append(evidence, b.ev(fmt.Sprintf("nodes[%s].allocatable.%s", best.Name, name), bestQty.String()))

		if want.Cmp(bestQty) > 0 {
			return []link{{
				object: b.podObject(),
				summary: fmt.Sprintf("The pod asks for %s %s, but the biggest node (%s) only has %s",
					want.String(), name, best.Name, bestQty.String()),
				evidence: evidence,
				fix:      fmt.Sprintf("Lower the %s request to %s or less, or add a node with at least %s %s.", name, bestQty.String(), want.String(), name),
			}}, true
		}
		return []link{{
			object: b.podObject(),
			summary: fmt.Sprintf("The pod's %s request (%s) would fit on an empty node (%s has %s), so other pods are using the space. Culprit doesn't collect their requests yet",
				name, want.String(), best.Name, bestQty.String()),
			evidence: evidence,
		}}, false
	}
	return nil, false
}

// placementCause covers node selectors, taints and cordoned nodes.
func (b *builder) placementCause(msg string) ([]link, bool) {
	pod := b.snap.Pod
	nodes := []snapshot.Node{}
	for _, n := range b.snap.Nodes {
		if !n.Missing {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return nil, false
	}

	if strings.Contains(msg, "node affinity/selector") && len(pod.Spec.NodeSelector) > 0 {
		keys := make([]string, 0, len(pod.Spec.NodeSelector))
		for k := range pod.Spec.NodeSelector {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		wants := []string{}
		for _, k := range keys {
			wants = append(wants, k+"="+pod.Spec.NodeSelector[k])
		}
		evidence := []string{b.ev("pod.spec.nodeSelector", strings.Join(wants, ", "))}
		matched := false
		for _, n := range nodes {
			wrong := []string{}
			for _, k := range keys {
				if got, ok := n.Labels[k]; !ok {
					wrong = append(wrong, k+" is not set")
				} else if got != pod.Spec.NodeSelector[k] {
					wrong = append(wrong, k+"="+got)
				}
			}
			if len(wrong) == 0 {
				matched = true
				continue
			}
			evidence = append(evidence, b.ev(fmt.Sprintf("nodes[%s].labels", n.Name), strings.Join(wrong, ", ")))
		}
		if !matched {
			return []link{{
				object:   b.podObject(),
				summary:  fmt.Sprintf("No node has the labels the pod's nodeSelector asks for (%s)", strings.Join(wants, ", ")),
				evidence: evidence,
				fix:      "Label a node to match (`kubectl label node NAME key=value`), or change the pod's nodeSelector.",
			}}, true
		}
	}

	if strings.Contains(msg, "untolerated taint") {
		evidence := []string{}
		shown := []string{}
		for _, n := range nodes {
			for _, t := range n.Taints {
				if t.Effect == corev1.TaintEffectPreferNoSchedule || tolerated(pod.Spec.Tolerations, t) {
					continue
				}
				text := taintText(t)
				evidence = append(evidence, b.ev(fmt.Sprintf("nodes[%s].taints", n.Name), text))
				shown = append(shown, fmt.Sprintf("%s on %s", text, n.Name))
			}
		}
		if len(evidence) > 0 {
			return []link{{
				object:   b.podObject(),
				summary:  fmt.Sprintf("The nodes have taints the pod doesn't tolerate (%s)", strings.Join(shown, "; ")),
				evidence: evidence,
				fix:      "Add a matching toleration to the pod, or remove the taint from a node (`kubectl taint node NAME KEY-`).",
			}}, true
		}
	}

	if strings.Contains(msg, "were unschedulable") {
		evidence := []string{}
		names := []string{}
		for _, n := range nodes {
			if n.Unschedulable {
				evidence = append(evidence, b.ev(fmt.Sprintf("nodes[%s].unschedulable", n.Name), "true"))
				names = append(names, n.Name)
			}
		}
		if len(names) > 0 && len(names) == len(nodes) {
			return []link{{
				object:   "Node " + strings.Join(names, ", "),
				summary:  fmt.Sprintf("Every node is cordoned (%s), so no new pods are placed on them", strings.Join(names, ", ")),
				evidence: evidence,
				fix:      "Uncordon a node: `kubectl uncordon NAME`.",
			}}, true
		}
	}
	return nil, false
}

// podRequest is what the scheduler has to find room for: the containers'
// requests added up, or the biggest init container if that is larger.
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

// tolerated reports whether any of the pod's tolerations covers the taint.
func tolerated(tolerations []corev1.Toleration, t corev1.Taint) bool {
	for _, tol := range tolerations {
		if tol.Effect != "" && tol.Effect != t.Effect {
			continue
		}
		if tol.Key != "" && tol.Key != t.Key {
			continue
		}
		if tol.Operator == corev1.TolerationOpExists || tol.Value == t.Value {
			return true
		}
	}
	return false
}

func taintText(t corev1.Taint) string {
	s := t.Key
	if t.Value != "" {
		s += "=" + t.Value
	}
	return s + ":" + string(t.Effect)
}

func (b *builder) storageClass(name string) *snapshot.StorageClass {
	for i, sc := range b.snap.StorageClasses {
		if sc.Name == name {
			return &b.snap.StorageClasses[i]
		}
	}
	return nil
}

func (b *builder) defaultClass() *snapshot.StorageClass {
	for i, sc := range b.snap.StorageClasses {
		if sc.IsDefault && sc.State == snapshot.Found {
			return &b.snap.StorageClasses[i]
		}
	}
	return nil
}

// readFailed reports whether collect failed to read something whose step or
// target mentions what.
func (b *builder) readFailed(what string) bool {
	for _, e := range b.snap.Errors {
		if strings.Contains(strings.ToLower(e.Step+" "+e.Target), what) {
			return true
		}
	}
	return false
}

func podCondition(pod *corev1.Pod, typ corev1.PodConditionType) *corev1.PodCondition {
	for i, c := range pod.Status.Conditions {
		if c.Type == typ {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func joinNonEmpty(sep string, parts ...string) string {
	out := []string{}
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
