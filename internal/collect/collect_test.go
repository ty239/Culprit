package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/culprit/culprit/internal/snapshot"
)

// The testdata/*.yaml files are real copies of the broken pods, taken from
// the kind-culprit cluster with:
//
//	kubectl get pod NAME -o yaml --show-managed-fields=false

const ns = "default"

func loadPod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(b, &pod); err != nil {
		t.Fatalf("parse %s.yaml: %v", name, err)
	}
	return &pod
}

func loadText(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeLogs answers log requests from a table, like the API server would.
// Keys are "pod/container/current" or "pod/container/previous".
// Anything not in the table gets the API server's real "no log" answer.
type fakeLogs map[string]any // string (log text) or error

func (f fakeLogs) fetch(_ context.Context, _ string, pod string, opts *corev1.PodLogOptions) (string, error) {
	which := "current"
	if opts.Previous {
		which = "previous"
	}
	switch v := f[pod+"/"+opts.Container+"/"+which].(type) {
	case string:
		return v, nil
	case error:
		return "", v
	}
	return "", apierrors.NewBadRequest(fmt.Sprintf("container %q in pod %q is waiting to start", opts.Container, pod))
}

func collectFor(t *testing.T, podName string, logs fakeLogs) *snapshot.Snapshot {
	t.Helper()
	c := New(fake.NewSimpleClientset(loadPod(t, podName)))
	c.FetchLogs = logs.fetch
	snap, err := c.Collect(context.Background(), ns, podName)
	if err != nil {
		t.Fatalf("Collect(%s): %v", podName, err)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("unexpected collect errors: %+v", snap.Errors)
	}
	return snap
}

func status(t *testing.T, snap *snapshot.Snapshot, container string) corev1.ContainerStatus {
	t.Helper()
	for _, s := range snap.Pod.Status.ContainerStatuses {
		if s.Name == container {
			return s
		}
	}
	t.Fatalf("no status for container %q", container)
	return corev1.ContainerStatus{}
}

func condition(snap *snapshot.Snapshot, typ corev1.PodConditionType) *corev1.PodCondition {
	for i, c := range snap.Pod.Status.Conditions {
		if c.Type == typ {
			return &snap.Pod.Status.Conditions[i]
		}
	}
	return nil
}

// --- The "Done when" table from docs/collect-requirements.md ---------------

func TestCollect_UserAPI(t *testing.T) {
	snap := collectFor(t, "user-api", fakeLogs{})

	st := status(t, snap, "python-api")
	if st.State.Waiting == nil {
		t.Fatal("expected a waiting state")
	}
	if r := st.State.Waiting.Reason; r != "ErrImagePull" && r != "ImagePullBackOff" {
		t.Errorf("waiting reason = %q, want ErrImagePull or ImagePullBackOff", r)
	}
	if img := snap.Pod.Spec.Containers[0].Image; img != "my-python-app:1.0" {
		t.Errorf("image = %q, want my-python-app:1.0", img)
	}
	// Never started: no logs, and that's evidence, not an error.
	if snap.Logs["python-api"].Current.Unavailable == "" {
		t.Error("current log should be marked unavailable")
	}
	// TODO(nodes step): the node's image list, without my-python-app:1.0.
}

func TestCollect_CrashAPI(t *testing.T) {
	snap := collectFor(t, "crash-api", fakeLogs{
		"crash-api/python-api/previous": loadText(t, "crash-api.previous.log"),
	})

	term := status(t, snap, "python-api").LastTerminationState.Terminated
	if term == nil {
		t.Fatal("expected a last termination")
	}
	if term.Reason != "Error" || term.ExitCode != 1 {
		t.Errorf("last termination = %s/%d, want Error/1", term.Reason, term.ExitCode)
	}
	if prev := snap.Logs["python-api"].Previous.Text; !strings.Contains(prev, "No module named uvicorn") {
		t.Errorf("previous log = %q, want it to contain 'No module named uvicorn'", prev)
	}
}

func TestCollect_SecretAPI(t *testing.T) {
	snap := collectFor(t, "secret-api", fakeLogs{})

	w := status(t, snap, "python-api").State.Waiting
	if w == nil || w.Reason != "CreateContainerConfigError" {
		t.Fatalf("waiting = %+v, want reason CreateContainerConfigError", w)
	}
	if !strings.Contains(w.Message, "db-credentials") {
		t.Errorf("waiting message = %q, want it to name db-credentials", w.Message)
	}
	// TODO(refs step): Secret db-credentials marked as missing.
}

func TestCollect_MemoryAPI(t *testing.T) {
	snap := collectFor(t, "memory-api", fakeLogs{})

	term := status(t, snap, "python-api").LastTerminationState.Terminated
	if term == nil || term.Reason != "OOMKilled" || term.ExitCode != 137 {
		t.Fatalf("last termination = %+v, want OOMKilled/137", term)
	}
	limit := snap.Pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	if limit.Cmp(resource.MustParse("64Mi")) != 0 {
		t.Errorf("memory limit = %s, want 64Mi", limit.String())
	}
	// OOMKilled containers often leave empty logs: normal, not an error.
	if snap.Logs["python-api"].Previous.Unavailable == "" && snap.Logs["python-api"].Previous.Text == "" {
		t.Error("previous log should have text or say why it is unavailable")
	}
}

func TestCollect_PendingAPI(t *testing.T) {
	snap := collectFor(t, "pending-api", fakeLogs{})

	if snap.Pod.Status.Phase != corev1.PodPending {
		t.Errorf("phase = %s, want Pending", snap.Pod.Status.Phase)
	}
	cond := condition(snap, corev1.PodScheduled)
	if cond == nil || cond.Status != corev1.ConditionFalse {
		t.Fatalf("PodScheduled = %+v, want status False", cond)
	}
	if !strings.Contains(cond.Message, "Insufficient cpu") {
		t.Errorf("PodScheduled message = %q, want 'Insufficient cpu'", cond.Message)
	}
	cpu := snap.Pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpu.Cmp(resource.MustParse("100")) != 0 {
		t.Errorf("cpu request = %s, want 100", cpu.String())
	}
	// TODO(nodes step): the node's 16 allocatable CPUs.
}

// --- Rules ------------------------------------------------------------------

func TestCollect_PodNotFound(t *testing.T) {
	c := New(fake.NewSimpleClientset())
	snap, err := c.Collect(context.Background(), ns, "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
	if snap != nil {
		t.Error("snapshot should be nil when the pod can't be read")
	}
}

// Rule 3: a failed read is recorded and collection keeps going.
func TestCollect_LogsDeniedIsRecordedNotFatal(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "crash-api", errors.New("RBAC"))
	c := New(fake.NewSimpleClientset(loadPod(t, "crash-api")))
	c.FetchLogs = fakeLogs{
		"crash-api/python-api/current":  denied,
		"crash-api/python-api/previous": denied,
	}.fetch

	snap, err := c.Collect(context.Background(), ns, "crash-api")
	if err != nil {
		t.Fatalf("Collect should not fail when logs are denied: %v", err)
	}
	if snap.Pod == nil {
		t.Fatal("pod should still be collected")
	}
	if len(snap.Errors) != 2 {
		t.Fatalf("got %d errors, want 2: %+v", len(snap.Errors), snap.Errors)
	}
	for _, e := range snap.Errors {
		if !e.Denied || e.Step != "logs" {
			t.Errorf("error = %+v, want a denied logs error", e)
		}
	}
}

// Rule 4: we don't ask for previous logs when there was no previous run.
func TestCollect_NoPreviousRequestWithoutRestart(t *testing.T) {
	asked := false
	c := New(fake.NewSimpleClientset(loadPod(t, "secret-api")))
	c.FetchLogs = func(ctx context.Context, ns, pod string, opts *corev1.PodLogOptions) (string, error) {
		if opts.Previous {
			asked = true
		}
		return fakeLogs{}.fetch(ctx, ns, pod, opts)
	}
	snap, err := c.Collect(context.Background(), ns, "secret-api")
	if err != nil {
		t.Fatal(err)
	}
	if asked {
		t.Error("should not request previous logs for a container that never restarted")
	}
	if snap.Logs["python-api"].Previous.Unavailable != "container has not restarted" {
		t.Errorf("previous = %+v", snap.Logs["python-api"].Previous)
	}
}

func TestCollect_DropsBookkeepingFields(t *testing.T) {
	snap := collectFor(t, "crash-api", fakeLogs{})
	if _, ok := snap.Pod.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Error("last-applied-configuration should be removed")
	}
	if snap.Pod.Annotations["description"] == "" {
		t.Error("other annotations should be kept")
	}
}

func TestCollect_SnapshotIsJSON(t *testing.T) {
	snap := collectFor(t, "crash-api", fakeLogs{
		"crash-api/python-api/previous": loadText(t, "crash-api.previous.log"),
	})
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var back snapshot.Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(back.Logs["python-api"].Previous.Text, "uvicorn") {
		t.Error("logs lost in the JSON round trip")
	}
}

// The real fetcher, against the fake client (which always answers "fake logs").
func TestAPILogFetcher(t *testing.T) {
	client := fake.NewSimpleClientset(loadPod(t, "crash-api"))
	text, err := apiLogFetcher(client)(context.Background(), ns, "crash-api", &corev1.PodLogOptions{Container: "python-api"})
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Error("expected log text")
	}
}
