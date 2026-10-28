package collect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCollectPod(t *testing.T) {
	pod := newPod("web", func(p *corev1.Pod) {
		p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "busybox"}}
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: "envoy"})
	})
	client := fake.NewSimpleClientset(pod)

	got, logs, err := collectPod(context.Background(), client, testNS, "web")
	if err != nil {
		t.Fatalf("collectPod: %v", err)
	}
	if got.Name != "web" {
		t.Errorf("pod name = %q, want %q", got.Name, "web")
	}

	// Every container, including init containers, gets a log entry.
	for _, key := range []string{"web/init", "web/app", "web/sidecar"} {
		if _, ok := logs[key]; !ok {
			t.Errorf("missing log entry %q (have %v)", key, keys(logs))
		}
	}
	if len(logs) != 3 {
		t.Errorf("got %d log entries, want 3", len(logs))
	}
}

func TestCollectPodNotFound(t *testing.T) {
	client := fake.NewSimpleClientset()

	if _, _, err := collectPod(context.Background(), client, testNS, "missing"); err == nil {
		t.Fatal("expected an error for a missing pod, got nil")
	}
}

func TestCollectEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "web.1", Namespace: testNS},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web", Namespace: testNS},
		Reason:         "FailedScheduling",
	}
	otherNS := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "web.2", Namespace: "other"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web", Namespace: "other"},
	}
	client := fake.NewSimpleClientset(ev, otherNS)

	events, err := collectEvents(context.Background(), client, testNS, "web")
	if err != nil {
		t.Fatalf("collectEvents: %v", err)
	}
	if len(events) != 1 || events[0].Reason != "FailedScheduling" {
		t.Errorf("events = %+v, want the single FailedScheduling event", events)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
