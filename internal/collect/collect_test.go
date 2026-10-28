package collect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCollect(t *testing.T) {
	pod := newPod("web", func(p *corev1.Pod) {
		p.Spec.NodeName = "node-a"
		p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "data")}
	})
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "web.1", Namespace: testNS},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web", Namespace: testNS},
		Reason:         "BackOff",
	}
	client := fake.NewSimpleClientset(
		pod, event, newNode("node-a"), newNode("node-b"),
		newPVC("data", "pv-1", "fast"), newPV("pv-1"), newStorageClass("fast"),
	)

	snap, err := Collect(context.Background(), client, testNS, "web")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if snap.Pod == nil || snap.Pod.Name != "web" {
		t.Errorf("Pod = %v, want web", snap.Pod)
	}
	if len(snap.ContainerLogs) != 1 {
		t.Errorf("ContainerLogs has %d entries, want 1", len(snap.ContainerLogs))
	}
	if len(snap.Events) != 1 {
		t.Errorf("Events has %d entries, want 1", len(snap.Events))
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].Name != "node-a" {
		t.Errorf("Nodes = %v, want only node-a", snap.Nodes)
	}
	if snap.PVCs["data"] == nil || snap.PVs["pv-1"] == nil || snap.StorageClasses["fast"] == nil {
		t.Errorf("volume chain incomplete: pvcs=%v pvs=%v scs=%v", snap.PVCs, snap.PVs, snap.StorageClasses)
	}
}

func TestCollectMissingPod(t *testing.T) {
	client := fake.NewSimpleClientset()

	snap, err := Collect(context.Background(), client, testNS, "missing")
	if err == nil {
		t.Fatal("expected an error for a missing pod, got nil")
	}
	if snap != nil {
		t.Errorf("snapshot = %v, want nil on error", snap)
	}
}
