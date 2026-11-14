package diagnose

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/culprit/culprit/internal/snapshot"
)

// The testdata/*.json files are real snapshots of the test pods, taken from
// the kind-culprit cluster with:
//
//	kubectl culprit NAME -o snapshot

func load(t *testing.T, name string) *snapshot.Snapshot {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap snapshot.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("parse %s.json: %v", name, err)
	}
	return &snap
}

// diagnosed runs Diagnose and checks what every diagnosis must satisfy.
func diagnosed(t *testing.T, snap *snapshot.Snapshot) *Diagnosis {
	t.Helper()
	d := Diagnose(snap)
	checkWellFormed(t, d)
	return d
}

// checkWellFormed: every finding has evidence that exists, chains link to
// the next finding, and RootCauses lists exactly the root-cause findings.
func checkWellFormed(t *testing.T, d *Diagnosis) {
	t.Helper()
	evidence := map[string]bool{}
	for _, e := range d.Evidence {
		if e.Source == "" || e.Value == "" {
			t.Errorf("%s has an empty source or value: %+v", e.ID, e)
		}
		evidence[e.ID] = true
	}
	roots := []string{}
	for i, f := range d.Findings {
		if len(f.Evidence) == 0 {
			t.Errorf("%s (%s) has no evidence", f.ID, f.Summary)
		}
		for _, id := range f.Evidence {
			if !evidence[id] {
				t.Errorf("%s points at %s, which doesn't exist", f.ID, id)
			}
		}
		if f.CausedBy != "" {
			if i+1 >= len(d.Findings) || d.Findings[i+1].ID != f.CausedBy || d.Findings[i+1].Kind == Symptom {
				t.Errorf("%s is causedBy %s, which is not the next finding in its chain", f.ID, f.CausedBy)
			}
		}
		if f.Kind == RootCause {
			roots = append(roots, f.ID)
			if f.CausedBy != "" {
				t.Errorf("root cause %s should not be caused by anything", f.ID)
			}
			if f.Fix == "" {
				t.Errorf("root cause %s has no fix", f.ID)
			}
		}
	}
	if strings.Join(roots, ",") != strings.Join(d.RootCauses, ",") {
		t.Errorf("rootCauses = %v, but the root-cause findings are %v", d.RootCauses, roots)
	}
	if d.Healthy && len(d.Findings) > 0 {
		t.Error("a healthy pod should have no findings")
	}
}

// chainOf returns the kinds of the findings, e.g. "symptom>cause>rootCause".
func chainOf(d *Diagnosis) string {
	kinds := []string{}
	for _, f := range d.Findings {
		kinds = append(kinds, f.Kind)
	}
	return strings.Join(kinds, ">")
}

// root returns the single root cause, failing the test if there isn't one.
func root(t *testing.T, d *Diagnosis) Finding {
	t.Helper()
	if len(d.RootCauses) != 1 {
		t.Fatalf("want one root cause, got %v\n%s", d.RootCauses, d)
	}
	for _, f := range d.Findings {
		if f.ID == d.RootCauses[0] {
			return f
		}
	}
	t.Fatalf("root cause %s is not in the findings", d.RootCauses[0])
	return Finding{}
}

// evidenceText is every evidence value the finding points at, joined.
func evidenceText(d *Diagnosis, f Finding) string {
	parts := []string{}
	for _, e := range d.Evidence {
		for _, id := range f.Evidence {
			if e.ID == id {
				parts = append(parts, e.Source+" = "+e.Value)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func wantContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s = %q, want it to contain %q", what, got, w)
		}
	}
}

// --- The "Done when" table from docs/diagnose-requirements.md --------------

func TestDiagnose_UserAPI(t *testing.T) {
	d := diagnosed(t, load(t, "user-api"))
	if got := chainOf(d); got != "symptom>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	r := root(t, d)
	if r.Object != "Image my-python-app:1.0" {
		t.Errorf("root object = %q", r.Object)
	}
	wantContains(t, "root summary", r.Summary, "my-python-app:1.0", "isn't on node culprit-control-plane", "can't be pulled")
	wantContains(t, "root evidence", evidenceText(d, r), "nodes[culprit-control-plane].images", "pull access denied")
}

func TestDiagnose_CrashAPI(t *testing.T) {
	d := diagnosed(t, load(t, "crash-api"))
	if got := chainOf(d); got != "symptom>cause>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	wantContains(t, "symptom", d.Findings[0].Summary, "keeps crashing")
	wantContains(t, "cause", d.Findings[1].Summary, "exits with code 1")
	r := root(t, d)
	wantContains(t, "root summary", r.Summary, "No module named uvicorn")
	wantContains(t, "root evidence", evidenceText(d, r), "logs[python-api]")
}

func TestDiagnose_SecretAPI(t *testing.T) {
	d := diagnosed(t, load(t, "secret-api"))
	if got := chainOf(d); got != "symptom>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	wantContains(t, "symptom", d.Findings[0].Summary, "CreateContainerConfigError")
	r := root(t, d)
	if r.Object != "Secret default/db-credentials" {
		t.Errorf("root object = %q", r.Object)
	}
	wantContains(t, "root summary", r.Summary, `"db-credentials" doesn't exist`, "DB_PASSWORD")
	wantContains(t, "root evidence", evidenceText(d, r), "secrets[db-credentials].state = missing")
	wantContains(t, "fix", r.Fix, `"password"`)
}

func TestDiagnose_MemoryAPI(t *testing.T) {
	d := diagnosed(t, load(t, "memory-api"))
	if got := chainOf(d); got != "symptom>cause>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	wantContains(t, "cause", d.Findings[1].Summary, "OOMKilled", "137")
	r := root(t, d)
	wantContains(t, "root summary", r.Summary, "64Mi")
	wantContains(t, "root evidence", evidenceText(d, r), "resources.limits.memory = 64Mi")
}

func TestDiagnose_PendingAPI(t *testing.T) {
	d := diagnosed(t, load(t, "pending-api"))
	if got := chainOf(d); got != "symptom>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	wantContains(t, "symptom evidence", evidenceText(d, d.Findings[0]), "Insufficient cpu")
	r := root(t, d)
	wantContains(t, "root summary", r.Summary, "100 cpu", "culprit-control-plane", "16")
	wantContains(t, "root evidence", evidenceText(d, r),
		"resources.requests.cpu = 100", "nodes[culprit-control-plane].allocatable.cpu = 16")
}

// The chain in the README: pod -> PVC -> StorageClass.
func TestDiagnose_StorageAPI(t *testing.T) {
	d := diagnosed(t, load(t, "storage-api"))
	if got := chainOf(d); got != "symptom>cause>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	if got := d.Findings[1].Object; got != "PersistentVolumeClaim default/storage-api-data" {
		t.Errorf("cause object = %q", got)
	}
	wantContains(t, "cause", d.Findings[1].Summary, "Pending")
	r := root(t, d)
	if r.Object != "StorageClass fast-ssd" {
		t.Errorf("root object = %q", r.Object)
	}
	wantContains(t, "root summary", r.Summary, `"fast-ssd" doesn't exist`)
	wantContains(t, "root evidence", evidenceText(d, r), "storageClasses[fast-ssd].state = missing")
}

// Big enough when empty, but the pods already on the node leave too little.
func TestDiagnose_FullAPI(t *testing.T) {
	d := diagnosed(t, load(t, "full-api"))
	if got := chainOf(d); got != "symptom>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	r := root(t, d)
	if r.Object != "Node culprit-control-plane" {
		t.Errorf("root object = %q", r.Object)
	}
	wantContains(t, "root summary", r.Summary, "15500m cpu", "already requested")
	wantContains(t, "root evidence", evidenceText(d, r),
		"resources.requests.cpu = 15500m", "nodes[culprit-control-plane].allocatable.cpu = 16", "nodes[culprit-control-plane].requested.cpu")
}

func TestDiagnose_SelectorAPI(t *testing.T) {
	d := diagnosed(t, load(t, "selector-api"))
	if got := chainOf(d); got != "symptom>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	r := root(t, d)
	wantContains(t, "root summary", r.Summary, "nodeSelector", "disk=ssd")
	wantContains(t, "root evidence", evidenceText(d, r), "nodes[culprit-control-plane].labels = disk is not set")
}

// The app is healthy; its liveness probe is what's wrong. The pod even
// shows Running and ready between kills.
func TestDiagnose_ProbeAPI(t *testing.T) {
	snap := load(t, "probe-api")
	d := diagnosed(t, snap)
	if d.Healthy {
		t.Fatal("a pod the kubelet keeps killing is not healthy")
	}
	if got := chainOf(d); got != "symptom>cause>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	wantContains(t, "symptom", d.Findings[0].Summary, "keeps being restarted")
	wantContains(t, "cause", d.Findings[1].Summary, "liveness probe fails")
	r := root(t, d)
	wantContains(t, "root summary", r.Summary, "GET :8000/healthz", "404")
	// The access log's last line is not the cause.
	if strings.Contains(r.Summary, "The app fails with") {
		t.Errorf("root cause should be the probe, not the app's log: %s", r.Summary)
	}

	// Rule 2: once the events have expired, nothing proves the probe did it.
	snap.Events = []snapshot.Event{}
	snap.Pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
	}
	d = diagnosed(t, snap)
	if len(d.RootCauses) != 0 {
		t.Errorf("want no root cause without events, got:\n%s", d)
	}
	wantContains(t, "cause", d.Findings[1].Summary, "liveness probe", "may be")
}

func TestDiagnose_EvictedAPI(t *testing.T) {
	snap := load(t, "evicted-api")
	d := diagnosed(t, snap)
	// One chain: the container's exit code 137 is part of the eviction.
	if got := chainOf(d); got != "symptom>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	wantContains(t, "symptom", d.Findings[0].Summary, "evicted")
	r := root(t, d)
	wantContains(t, "root summary", r.Summary, "local disk", "10Mi")
	wantContains(t, "root evidence", evidenceText(d, r), "resources.limits.ephemeral-storage = 10Mi")

	// Stored on the pod, so it survives the events expiring.
	snap.Events = []snapshot.Event{}
	wantContains(t, "root summary", root(t, diagnosed(t, snap)).Summary, "10Mi")
}

func TestDiagnose_EvictedForNodePressure(t *testing.T) {
	snap := load(t, "evicted-api")
	snap.Pod.Status.Message = "The node was low on resource: memory. Threshold quantity: 100Mi, available: 60Mi."
	snap.Nodes[0].Conditions = append(snap.Nodes[0].Conditions,
		snapshot.NodeCondition{Type: "MemoryPressure", Status: "True", Message: "kubelet has insufficient memory available"})

	d := diagnosed(t, snap)
	r := root(t, d)
	if r.Object != "Node culprit-control-plane" {
		t.Errorf("root object = %q", r.Object)
	}
	wantContains(t, "root summary", r.Summary, "ran low on memory")
	wantContains(t, "root evidence", evidenceText(d, r), "conditions[MemoryPressure]")
}

// A real init container that fails: one chain, about the init container.
func TestDiagnose_InitAPI(t *testing.T) {
	d := diagnosed(t, load(t, "init-api"))
	if !strings.HasPrefix(d.Status, "Init:") {
		t.Errorf("status = %q, want Init:...", d.Status)
	}
	if got := chainOf(d); got != "symptom>cause>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	wantContains(t, "symptom", d.Findings[0].Summary, `"migrate"`)
	wantContains(t, "root summary", root(t, d).Summary, "migration failed: cannot reach database")
}

// Rule 6: running and ready is healthy, even with a restart in the past.
func TestDiagnose_HealthyPod(t *testing.T) {
	snap := load(t, "my-app")
	if snap.Pod.Status.ContainerStatuses[0].RestartCount == 0 {
		t.Fatal("this snapshot is meant to have a past restart")
	}
	d := diagnosed(t, snap)
	if !d.Healthy || len(d.Findings) != 0 || len(d.RootCauses) != 0 {
		t.Errorf("want healthy with no findings, got:\n%s", d)
	}
	if d.Status != "Running" {
		t.Errorf("status = %q, want Running", d.Status)
	}
}

// --- Rules ------------------------------------------------------------------

// Rule 3: events expire. The answer must not change when they are gone.
func TestDiagnose_SameRootCauseWithoutEvents(t *testing.T) {
	for _, name := range []string{"user-api", "crash-api", "secret-api", "memory-api", "pending-api", "storage-api",
		"full-api", "selector-api", "evicted-api", "init-api"} {
		t.Run(name, func(t *testing.T) {
			snap := load(t, name)
			want := root(t, diagnosed(t, snap))

			snap.Events = []snapshot.Event{}
			got := root(t, diagnosed(t, snap))
			if got.Object != want.Object {
				t.Errorf("root object without events = %q, want %q", got.Object, want.Object)
			}
		})
	}
}

// Rule 4: memory-api flips between CrashLoopBackOff, OOMKilled and Running.
func TestDiagnose_MemoryAPIInEveryState(t *testing.T) {
	base := load(t, "memory-api")
	last := base.Pod.Status.ContainerStatuses[0].LastTerminationState.Terminated

	states := map[string]corev1.ContainerState{
		"terminated": {Terminated: last.DeepCopy()},
		"running":    {Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base.CollectedAt)}},
	}
	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			snap := load(t, "memory-api")
			snap.Pod.Status.ContainerStatuses[0].State = state
			// Without a readiness probe, it even reports ready while it runs.
			snap.Pod.Status.ContainerStatuses[0].Ready = state.Running != nil

			d := diagnosed(t, snap)
			if d.Healthy {
				t.Fatal("a crash-looping pod caught while running is not healthy")
			}
			wantContains(t, "root summary", root(t, d).Summary, "64Mi")
		})
	}
}

// Rule 4: user-api flips between ErrImagePull and ImagePullBackOff.
func TestDiagnose_UserAPIInBothStates(t *testing.T) {
	const pullErr = `failed to pull and unpack image "docker.io/library/my-python-app:1.0": pull access denied, repository does not exist or may require authorization`
	states := map[string]corev1.ContainerStateWaiting{
		"ErrImagePull":     {Reason: "ErrImagePull", Message: pullErr},
		"ImagePullBackOff": {Reason: "ImagePullBackOff", Message: `Back-off pulling image "my-python-app:1.0"`},
	}
	for name, w := range states {
		t.Run(name, func(t *testing.T) {
			snap := load(t, "user-api")
			snap.Pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &w}
			d := diagnosed(t, snap)
			if d.Status != name {
				t.Errorf("status = %q, want %q", d.Status, name)
			}
			r := root(t, d)
			if r.Object != "Image my-python-app:1.0" {
				t.Errorf("root object = %q", r.Object)
			}
			wantContains(t, "root summary", r.Summary, "doesn't exist or needs a login")
		})
	}
}

// Rule 5: the crash message can be in the current or the previous log.
func TestDiagnose_CrashLogOnEitherSide(t *testing.T) {
	const msg = "/usr/local/bin/python: No module named uvicorn\n"
	gone := snapshot.Log{Unavailable: "unable to retrieve container logs for containerd://dd82"}

	cases := map[string]snapshot.ContainerLogs{
		"current":  {Current: snapshot.Log{Text: msg}, Previous: gone},
		"previous": {Current: snapshot.Log{Unavailable: "log is empty"}, Previous: snapshot.Log{Text: msg}},
	}
	for name, logs := range cases {
		t.Run(name, func(t *testing.T) {
			snap := load(t, "crash-api")
			snap.Logs["python-api"] = logs
			r := root(t, diagnosed(t, snap))
			wantContains(t, "root summary", r.Summary, "No module named uvicorn")
		})
	}
}

// Caught just after a restart, the new run's output is not the crash.
func TestDiagnose_RunningCrashLoopReadsPreviousLog(t *testing.T) {
	snap := load(t, "crash-api")
	st := &snap.Pod.Status.ContainerStatuses[0]
	st.LastTerminationState.Terminated.FinishedAt = metav1.NewTime(snap.CollectedAt.Add(-20 * time.Second))
	st.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(snap.CollectedAt)}}
	snap.Logs["python-api"] = snapshot.ContainerLogs{
		Current:  snapshot.Log{Text: "starting up...\n"},
		Previous: snapshot.Log{Text: "Traceback (most recent call last):\n  File \"app.py\", line 3\nKeyError: 'DATABASE_URL'\n\n"},
	}
	r := root(t, diagnosed(t, snap))
	wantContains(t, "root summary", r.Summary, "KeyError: 'DATABASE_URL'")
	wantContains(t, "root evidence", evidenceText(diagnosed(t, snap), r), "logs[python-api].previous")
}

// Rule 2: no logs means no made-up root cause.
func TestDiagnose_CrashWithoutLogsHasNoRootCause(t *testing.T) {
	snap := load(t, "crash-api")
	snap.Logs["python-api"] = snapshot.ContainerLogs{
		Current:  snapshot.Log{Unavailable: "log is empty"},
		Previous: snapshot.Log{Unavailable: "log is empty"},
	}
	d := diagnosed(t, snap)
	if got := chainOf(d); got != "symptom>cause" {
		t.Fatalf("chain = %s, want symptom>cause\n%s", got, d)
	}
	if len(d.RootCauses) != 0 {
		t.Errorf("rootCauses = %v, want none", d.RootCauses)
	}
	wantContains(t, "text", d.String(), "Root cause not found")
}

// Rule 2: an unschedulable pod we can't explain stays a symptom.
func TestDiagnose_UnknownSchedulingReasonHasNoRootCause(t *testing.T) {
	snap := load(t, "pending-api")
	podCondition(snap.Pod, corev1.PodScheduled).Message = "0/1 nodes are available: 1 node(s) had volume node affinity conflict."
	d := diagnosed(t, snap)
	if got := chainOf(d); got != "symptom" {
		t.Fatalf("chain = %s, want symptom\n%s", got, d)
	}
}

// The request fits on the node, so the node being too small is not the cause.
func TestDiagnose_RequestThatFitsIsNotBlamedOnNodeSize(t *testing.T) {
	snap := load(t, "pending-api")
	snap.Pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = snap.Nodes[0].Allocatable[corev1.ResourceCPU]

	// With the other pods' requests in the snapshot, they are the answer.
	r := root(t, diagnosed(t, snap))
	if r.Object != "Node culprit-control-plane" {
		t.Errorf("root object = %q, want the node", r.Object)
	}

	// Without them (an older snapshot, or the pod list was denied): no guess.
	snap.Nodes[0].Requested = nil
	d := diagnosed(t, snap)
	if len(d.RootCauses) != 0 {
		t.Fatalf("want no root cause, got:\n%s", d)
	}
	wantContains(t, "cause", d.Findings[1].Summary, "other pods")
}

// The request fits next to the other pods too, so cpu isn't what blocks it.
func TestDiagnose_RequestThatFitsInTheRoomLeft(t *testing.T) {
	snap := load(t, "full-api")
	snap.Pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("2")
	if d := diagnosed(t, snap); len(d.RootCauses) != 0 {
		t.Errorf("want no root cause, got:\n%s", d)
	}
}

// Rule 7: failed reads are listed.
func TestDiagnose_ListsFailedReads(t *testing.T) {
	snap := load(t, "secret-api")
	snap.Secrets[0].State = snapshot.Unreadable
	snap.Errors = []snapshot.CollectError{{Step: "refs", Target: "secrets/db-credentials", Denied: true, Message: "forbidden"}}

	d := diagnosed(t, snap)
	if len(d.RootCauses) != 0 {
		t.Errorf("an unreadable Secret must not be reported as missing:\n%s", d)
	}
	if len(d.Incomplete) != 1 {
		t.Fatalf("incomplete = %v, want one entry", d.Incomplete)
	}
	wantContains(t, "incomplete", d.Incomplete[0], "secrets/db-credentials", "access denied")
	wantContains(t, "text", d.String(), "snapshot is incomplete")
}

// --- More chains -------------------------------------------------------------

func TestDiagnose_SecretWithoutTheKey(t *testing.T) {
	snap := load(t, "secret-api")
	snap.Secrets[0].State = snapshot.Found
	snap.Secrets[0].Keys = []string{"username"}
	snap.Pod.Status.ContainerStatuses[0].State.Waiting.Message = `couldn't find key password in Secret default/db-credentials`

	r := root(t, diagnosed(t, snap))
	wantContains(t, "root summary", r.Summary, `has no key "password"`)
}

func TestDiagnose_OptionalSecretIsNotACause(t *testing.T) {
	snap := load(t, "secret-api")
	snap.Secrets[0].UsedBy[0].Optional = true
	if d := diagnosed(t, snap); len(d.RootCauses) != 0 {
		t.Errorf("an optional Secret can't block the pod:\n%s", d)
	}
}

func TestDiagnose_MissingImagePullSecret(t *testing.T) {
	snap := load(t, "user-api")
	snap.Secrets = []snapshot.Reference{{
		Name: "regcred", State: snapshot.Missing,
		UsedBy: []snapshot.Usage{{Via: "imagePullSecrets"}},
	}}
	r := root(t, diagnosed(t, snap))
	if r.Object != "Secret default/regcred" {
		t.Errorf("root object = %q, want Secret default/regcred", r.Object)
	}
}

func TestDiagnose_MissingPVC(t *testing.T) {
	snap := load(t, "storage-api")
	snap.PVCs = []snapshot.PVC{{Name: "storage-api-data", State: snapshot.Missing, UsedBy: []string{"data"}}}
	snap.StorageClasses = []snapshot.StorageClass{}

	d := diagnosed(t, snap)
	if got := chainOf(d); got != "symptom>rootCause" {
		t.Fatalf("chain = %s\n%s", got, d)
	}
	wantContains(t, "root summary", root(t, d).Summary, `"storage-api-data" doesn't exist`)
}

func TestDiagnose_NoDefaultStorageClass(t *testing.T) {
	snap := load(t, "storage-api")
	snap.PVCs[0].StorageClassName = ""
	snap.PVCs[0].UsesDefaultClass = true
	snap.StorageClasses = []snapshot.StorageClass{}

	r := root(t, diagnosed(t, snap))
	wantContains(t, "root summary", r.Summary, "no default")
}

// A WaitForFirstConsumer PVC is Pending because the pod is, not the reverse.
func TestDiagnose_WaitForFirstConsumerIsNotACause(t *testing.T) {
	snap := load(t, "pending-api")
	snap.PVCs = []snapshot.PVC{{
		Name: "data", State: snapshot.Found, UsedBy: []string{"data"},
		Phase: "Pending", StorageClassName: "standard",
	}}
	snap.StorageClasses = []snapshot.StorageClass{{
		Name: "standard", State: snapshot.Found, Provisioner: "rancher.io/local-path",
		VolumeBindingMode: "WaitForFirstConsumer", IsDefault: true,
	}}
	r := root(t, diagnosed(t, snap))
	wantContains(t, "root summary", r.Summary, "100 cpu") // still the CPU request
}

func TestDiagnose_NodeSelectorMatchesNoNode(t *testing.T) {
	snap := load(t, "pending-api")
	snap.Pod.Spec.NodeSelector = map[string]string{"disk": "ssd"}
	podCondition(snap.Pod, corev1.PodScheduled).Message = "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector."

	d := diagnosed(t, snap)
	r := root(t, d)
	wantContains(t, "root summary", r.Summary, "disk=ssd")
	wantContains(t, "root evidence", evidenceText(d, r), "disk is not set")
}

func TestDiagnose_UntoleratedTaint(t *testing.T) {
	snap := load(t, "pending-api")
	snap.Nodes[0].Taints = []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	podCondition(snap.Pod, corev1.PodScheduled).Message = "0/1 nodes are available: 1 node(s) had untolerated taint {dedicated: gpu}."

	r := root(t, diagnosed(t, snap))
	wantContains(t, "root summary", r.Summary, "dedicated=gpu:NoSchedule")

	// With a toleration for it, the taint is no longer the answer.
	snap.Pod.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gpu"}}
	if d := diagnosed(t, snap); len(d.RootCauses) != 0 {
		t.Errorf("a tolerated taint is not a cause:\n%s", d)
	}
}

func TestDiagnose_OOMKilledWithoutALimit(t *testing.T) {
	snap := load(t, "memory-api")
	snap.Pod.Spec.Containers[0].Resources.Limits = nil
	r := root(t, diagnosed(t, snap))
	wantContains(t, "root summary", r.Summary, "no memory limit")
}

// An init container that fails is reported; the containers waiting on it aren't.
func TestDiagnose_FailingInitContainer(t *testing.T) {
	snap := load(t, "crash-api")
	st := snap.Pod.Status.ContainerStatuses[0]
	st.Name = "migrate"
	spec := snap.Pod.Spec.Containers[0]
	spec.Name = "migrate"
	snap.Pod.Spec.InitContainers = []corev1.Container{spec}
	snap.Pod.Status.InitContainerStatuses = []corev1.ContainerStatus{st}
	snap.Pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
	}
	snap.Logs["migrate"] = snap.Logs["python-api"]

	d := diagnosed(t, snap)
	if d.Status != "Init:CrashLoopBackOff" {
		t.Errorf("status = %q, want Init:CrashLoopBackOff", d.Status)
	}
	if got := chainOf(d); got != "symptom>cause>rootCause" {
		t.Fatalf("chain = %s, want one chain\n%s", got, d)
	}
	wantContains(t, "symptom", d.Findings[0].Summary, `"migrate"`)
}

func TestDiagnose_RunningButNotReady(t *testing.T) {
	snap := load(t, "my-app")
	snap.Pod.Status.ContainerStatuses[0].Ready = false
	snap.Events = append(snap.Events, snapshot.Event{Type: "Warning", Reason: "Unhealthy", Message: "Readiness probe failed: HTTP probe failed with statuscode: 503"})

	d := diagnosed(t, snap)
	if d.Healthy || chainOf(d) != "symptom" {
		t.Fatalf("want an unhealthy pod with one symptom, got:\n%s", d)
	}
	wantContains(t, "symptom evidence", evidenceText(d, d.Findings[0]), "Readiness probe failed")
}

// --- Output -------------------------------------------------------------------

func TestRender_StorageAPI(t *testing.T) {
	text := diagnosed(t, load(t, "storage-api")).String()
	wantContains(t, "text", text,
		"Pod default/storage-api: Pending",
		"Symptom     Pod is Pending",
		"caused by",
		`Cause       PVC "storage-api-data" is Pending`,
		`Root cause  StorageClass "fast-ssd" doesn't exist`,
		"ev-4  storageClasses[fast-ssd].state = missing",
		"Fix: Create StorageClass",
	)
	// Symptom first, root cause last.
	if strings.Index(text, "Symptom") > strings.Index(text, "Root cause") {
		t.Errorf("symptom should come before the root cause:\n%s", text)
	}
}

func TestDiagnosis_IsJSON(t *testing.T) {
	b, err := json.Marshal(diagnosed(t, load(t, "storage-api")))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "json", string(b), `"causedBy":"f-2"`, `"kind":"rootCause"`, `"rootCauses":["f-3"]`, `"id":"ev-1"`)

	// Healthy pods encode empty lists, not null.
	b, err = json.Marshal(diagnosed(t, load(t, "my-app")))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "json", string(b), `"healthy":true`, `"findings":[]`, `"rootCauses":[]`)
}
