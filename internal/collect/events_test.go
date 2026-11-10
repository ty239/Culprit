package collect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/culprit/culprit/internal/snapshot"
)

// testdata/events.yaml is a real copy of every event in the default
// namespace of the kind-culprit cluster, taken with:
//
//	kubectl get events -n default -o yaml --show-managed-fields=false
func loadEvents(t *testing.T) []runtime.Object {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var list corev1.EventList
	if err := yaml.Unmarshal(b, &list); err != nil {
		t.Fatalf("parse events.yaml: %v", err)
	}
	objs := make([]runtime.Object, 0, len(list.Items))
	for i := range list.Items {
		objs = append(objs, &list.Items[i])
	}
	return objs
}

// collectWithEvents runs Collect for podName with all the real events (for
// every pod) loaded, plus any extra objects.
func collectWithEvents(t *testing.T, podName string, extra ...runtime.Object) *snapshot.Snapshot {
	t.Helper()
	objs := append([]runtime.Object{loadPod(t, podName)}, loadEvents(t)...)
	objs = append(objs, extra...)
	c := New(fake.NewSimpleClientset(objs...))
	c.FetchLogs = fakeLogs{}.fetch
	snap, err := c.Collect(context.Background(), ns, podName)
	if err != nil {
		t.Fatalf("Collect(%s): %v", podName, err)
	}
	return snap
}

func findEvent(snap *snapshot.Snapshot, reason string) *snapshot.Event {
	for i := range snap.Events {
		if snap.Events[i].Reason == reason {
			return &snap.Events[i]
		}
	}
	return nil
}

func reasons(snap *snapshot.Snapshot) []string {
	var out []string
	for _, e := range snap.Events {
		out = append(out, e.Reason)
	}
	return out
}

// --- Real events per pod ---------------------------------------------------

func TestEvents_PendingAPI(t *testing.T) {
	snap := collectWithEvents(t, "pending-api")

	e := findEvent(snap, "FailedScheduling")
	if e == nil {
		t.Fatalf("no FailedScheduling event, got %v", reasons(snap))
	}
	if e.Type != "Warning" {
		t.Errorf("type = %q, want Warning", e.Type)
	}
	if !strings.Contains(e.Message, "Insufficient cpu") {
		t.Errorf("message = %q, want 'Insufficient cpu'", e.Message)
	}
	if e.Count < 2 {
		t.Errorf("count = %d, want the repeat count from the cluster", e.Count)
	}
	if e.LastSeen.IsZero() {
		t.Error("lastSeen should be set")
	}
}

func TestEvents_UserAPI(t *testing.T) {
	snap := collectWithEvents(t, "user-api")

	e := findEvent(snap, "BackOff")
	if e == nil || !strings.Contains(e.Message, `Back-off pulling image "my-python-app:1.0"`) {
		t.Fatalf("want a BackOff event for the image pull, got %+v", snap.Events)
	}
}

func TestEvents_CrashAPI(t *testing.T) {
	snap := collectWithEvents(t, "crash-api")

	got := reasons(snap)
	if len(got) != 2 || findEvent(snap, "BackOff") == nil || findEvent(snap, "Pulled") == nil {
		t.Errorf("events = %v, want exactly Pulled and BackOff", got)
	}
}

// --- Rules -----------------------------------------------------------------

// Only this pod's events, never another pod's.
func TestEvents_OnlyThisPod(t *testing.T) {
	snap := collectWithEvents(t, "crash-api")
	for _, e := range snap.Events {
		if strings.Contains(e.Message, "memory-api") || strings.Contains(e.Message, "pending-api") {
			t.Errorf("got another pod's event: %+v", e)
		}
	}
}

// Same name, different kind (e.g. a Service called crash-api): skip it.
func TestEvents_SkipsOtherKindWithSameName(t *testing.T) {
	svcEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "crash-api.svc", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Service", Name: "crash-api", Namespace: ns},
		Reason:         "SyncLoadBalancerFailed",
	}
	snap := collectWithEvents(t, "crash-api", svcEvent)
	if findEvent(snap, "SyncLoadBalancerFailed") != nil {
		t.Error("an event about a Service named crash-api ended up in the pod's events")
	}
}

// Same name, older (deleted) pod: skip it.
func TestEvents_SkipsOlderPodWithSameName(t *testing.T) {
	old := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "crash-api.old", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "crash-api", Namespace: ns, UID: "some-older-pod"},
		Reason:         "OldPodEvent",
	}
	snap := collectWithEvents(t, "crash-api", old)
	if findEvent(snap, "OldPodEvent") != nil {
		t.Error("an event about an older pod with the same name was kept")
	}
}

func TestEvents_SortedOldestFirst(t *testing.T) {
	snap := collectWithEvents(t, "memory-api")
	if len(snap.Events) < 2 {
		t.Fatalf("want several events, got %v", reasons(snap))
	}
	for i := 1; i < len(snap.Events); i++ {
		if snap.Events[i].LastSeen.Before(snap.Events[i-1].LastSeen) {
			t.Errorf("events out of order: %v", snap.Events)
		}
	}
}

// Newer clusters may send events in the events.k8s.io format: no
// Count/LastTimestamp, but EventTime and Series instead.
func TestEvents_NewEventFormat(t *testing.T) {
	pod := loadPod(t, "crash-api")
	first := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	last := first.Add(10 * time.Minute)
	e := &corev1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: "crash-api.series", Namespace: ns},
		InvolvedObject:      corev1.ObjectReference{Kind: "Pod", Name: "crash-api", Namespace: ns, UID: pod.UID},
		Reason:              "SeriesEvent",
		EventTime:           metav1.NewMicroTime(first),
		Series:              &corev1.EventSeries{Count: 42, LastObservedTime: metav1.NewMicroTime(last)},
		ReportingController: "kubelet",
	}
	snap := collectWithEvents(t, "crash-api", e)

	got := findEvent(snap, "SeriesEvent")
	if got == nil {
		t.Fatal("event missing")
	}
	if got.Count != 42 || !got.FirstSeen.Equal(first) || !got.LastSeen.Equal(last) || got.Source != "kubelet" {
		t.Errorf("got %+v, want count 42, first %s, last %s, source kubelet", got, first, last)
	}
}

// Rule 5: events expire. No events is normal, not an error.
func TestEvents_NoneIsNotAnError(t *testing.T) {
	snap := collectFor(t, "secret-api", fakeLogs{}) // no events loaded at all
	if snap.Events == nil || len(snap.Events) != 0 {
		t.Errorf("events = %v, want an empty list", snap.Events)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("errors = %v, want none", snap.Errors)
	}
}

// Rule 3: if reading events is denied, record it and keep everything else.
func TestEvents_DeniedIsRecordedNotFatal(t *testing.T) {
	client := fake.NewSimpleClientset(loadPod(t, "crash-api"))
	client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("RBAC"))
	})
	c := New(client)
	c.FetchLogs = fakeLogs{"crash-api/python-api/previous": "No module named uvicorn"}.fetch

	snap, err := c.Collect(context.Background(), ns, "crash-api")
	if err != nil {
		t.Fatalf("Collect should not fail when events are denied: %v", err)
	}
	if len(snap.Errors) != 1 || snap.Errors[0].Step != "events" || !snap.Errors[0].Denied {
		t.Fatalf("errors = %+v, want one denied events error", snap.Errors)
	}
	if snap.Pod == nil || snap.Logs["python-api"].Previous.Text == "" {
		t.Error("pod and logs should still be collected")
	}
}

// The selector we send to the API server.
func TestEvents_SendsFieldSelector(t *testing.T) {
	client := fake.NewSimpleClientset(loadPod(t, "crash-api"))
	var sent string
	client.PrependReactor("list", "events", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sent = a.(k8stesting.ListAction).GetListRestrictions().Fields.String()
		return false, nil, nil
	})
	c := New(client)
	c.FetchLogs = fakeLogs{}.fetch
	if _, err := c.Collect(context.Background(), ns, "crash-api"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"involvedObject.kind=Pod", "involvedObject.name=crash-api", "involvedObject.uid="} {
		if !strings.Contains(sent, want) {
			t.Errorf("field selector %q is missing %q", sent, want)
		}
	}
}
