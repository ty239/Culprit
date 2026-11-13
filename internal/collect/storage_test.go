package collect

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/culprit/culprit/internal/snapshot"
)

func strPtr(s string) *string { return &s }

func realStorageClass(t *testing.T) *storagev1.StorageClass {
	t.Helper()
	var sc storagev1.StorageClass
	loadYAML(t, "storageclass-standard.yaml", &sc)
	return &sc
}

func pvcVolume(volume, claim string) corev1.Volume {
	return corev1.Volume{Name: volume, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
	}}
}

// boundPVC is a healthy claim bound to a PV.
func boundPVC(name, pv string, class *string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: class,
			VolumeName:       pv,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		},
	}
}

func pv(name, class, claim string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName:              class,
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			ClaimRef:                      &corev1.ObjectReference{Namespace: ns, Name: claim},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
}

// --- The README's chain, with real cluster data -----------------------------

// storage-api: pod Pending → PVC Pending → StorageClass fast-ssd missing.
func TestStorage_StorageAPI(t *testing.T) {
	snap := collectFor(t, "storage-api", fakeLogs{})

	// The symptom, from the pod's stored status.
	if snap.Pod.Status.Phase != corev1.PodPending {
		t.Errorf("phase = %s, want Pending", snap.Pod.Status.Phase)
	}
	if c := condition(snap, corev1.PodScheduled); c == nil || !strings.Contains(c.Message, "unbound immediate PersistentVolumeClaims") {
		t.Errorf("PodScheduled = %+v, want 'unbound immediate PersistentVolumeClaims'", c)
	}

	// The intermediate cause: the PVC.
	if len(snap.PVCs) != 1 {
		t.Fatalf("pvcs = %+v, want storage-api-data", snap.PVCs)
	}
	pvc := snap.PVCs[0]
	if pvc.Name != "storage-api-data" || pvc.State != snapshot.Found || pvc.Phase != "Pending" {
		t.Errorf("pvc = %+v, want storage-api-data found and Pending", pvc)
	}
	if pvc.StorageClassName != "fast-ssd" || pvc.UsesDefaultClass {
		t.Errorf("pvc class = %q (default %v), want fast-ssd", pvc.StorageClassName, pvc.UsesDefaultClass)
	}
	if pvc.VolumeName != "" || len(snap.PVs) != 0 {
		t.Errorf("pvc should not be bound to any PV, got %q / %+v", pvc.VolumeName, snap.PVs)
	}
	if pvc.Requested != "1Gi" || len(pvc.UsedBy) != 1 || pvc.UsedBy[0] != "data" {
		t.Errorf("pvc = %+v, want 1Gi used by volume data", pvc)
	}

	// The root cause: the StorageClass doesn't exist (while "standard" does).
	if len(snap.StorageClasses) != 1 || snap.StorageClasses[0].Name != "fast-ssd" || snap.StorageClasses[0].State != snapshot.Missing {
		t.Errorf("storageClasses = %+v, want only fast-ssd marked missing", snap.StorageClasses)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("missing StorageClass is evidence, not an error: %+v", snap.Errors)
	}
}

// Pods without PVCs make no storage requests at all.
func TestStorage_NoClaimsNoRequests(t *testing.T) {
	client := fake.NewSimpleClientset(append([]runtime.Object{loadPod(t, "crash-api")}, realClusterObjects(t)...)...)
	c := New(client)
	c.FetchLogs = fakeLogs{}.fetch
	snap, err := c.Collect(context.Background(), ns, "crash-api")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		switch a.GetResource().Resource {
		case "persistentvolumeclaims", "persistentvolumes", "storageclasses":
			t.Errorf("unexpected storage request: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if len(snap.PVCs)+len(snap.PVs)+len(snap.StorageClasses) != 0 {
		t.Error("storage lists should be empty")
	}
}

// --- A healthy chain ----------------------------------------------------------

func TestStorage_FullBoundChain(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "app-data")} })
	snap := collectRefsFor(t, pod, boundPVC("app-data", "pv-1", strPtr("standard")), pv("pv-1", "standard", "app-data"), realStorageClass(t))

	if len(snap.PVCs) != 1 || snap.PVCs[0].Phase != "Bound" || snap.PVCs[0].VolumeName != "pv-1" || snap.PVCs[0].Capacity != "1Gi" {
		t.Errorf("pvcs = %+v, want app-data Bound to pv-1 with 1Gi", snap.PVCs)
	}
	if len(snap.PVs) != 1 {
		t.Fatalf("pvs = %+v, want pv-1", snap.PVs)
	}
	p := snap.PVs[0]
	if p.Name != "pv-1" || p.State != snapshot.Found || p.Phase != "Bound" || p.ClaimRef != ns+"/app-data" || p.ReclaimPolicy != "Delete" {
		t.Errorf("pv = %+v", p)
	}
	if len(snap.StorageClasses) != 1 {
		t.Fatalf("storageClasses = %+v, want standard once (named by both PVC and PV)", snap.StorageClasses)
	}
	sc := snap.StorageClasses[0]
	if sc.Name != "standard" || sc.State != snapshot.Found || sc.Provisioner != "rancher.io/local-path" ||
		sc.VolumeBindingMode != "WaitForFirstConsumer" || !sc.IsDefault {
		t.Errorf("storageClass = %+v, want the real kind 'standard' class", sc)
	}
}

// --- Missing links ------------------------------------------------------------

func TestStorage_MissingPVC(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "nope")} })
	snap := collectRefsFor(t, pod)

	if len(snap.PVCs) != 1 || snap.PVCs[0].Name != "nope" || snap.PVCs[0].State != snapshot.Missing {
		t.Errorf("pvcs = %+v, want nope missing", snap.PVCs)
	}
	if len(snap.PVs)+len(snap.StorageClasses) != 0 {
		t.Error("nothing to follow from a missing PVC")
	}
}

// The PVC says it's bound to a PV that no longer exists.
func TestStorage_MissingPV(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "app-data")} })
	pvc := boundPVC("app-data", "pv-gone", strPtr("standard"))
	pvc.Status.Phase = corev1.ClaimLost
	snap := collectRefsFor(t, pod, pvc, realStorageClass(t))

	if len(snap.PVs) != 1 || snap.PVs[0].Name != "pv-gone" || snap.PVs[0].State != snapshot.Missing {
		t.Errorf("pvs = %+v, want pv-gone missing", snap.PVs)
	}
	if snap.PVCs[0].Phase != "Lost" {
		t.Errorf("pvc phase = %q, want Lost", snap.PVCs[0].Phase)
	}
}

// --- Default and empty StorageClass -------------------------------------------

// No storageClassName → the cluster default applies, so collect it.
func TestStorage_DefaultClass(t *testing.T) {
	other := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "slow"}, Provisioner: "example.com/slow"}
	pod := podWith(func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "app-data")} })
	pvc := boundPVC("app-data", "", nil)
	pvc.Status.Phase = corev1.ClaimPending
	snap := collectRefsFor(t, pod, pvc, realStorageClass(t), other)

	if !snap.PVCs[0].UsesDefaultClass || snap.PVCs[0].StorageClassName != "" {
		t.Errorf("pvc = %+v, want usesDefaultClass", snap.PVCs[0])
	}
	if len(snap.StorageClasses) != 1 || snap.StorageClasses[0].Name != "standard" || !snap.StorageClasses[0].IsDefault {
		t.Errorf("storageClasses = %+v, want only the default (standard)", snap.StorageClasses)
	}
}

// No storageClassName and no default class in the cluster: nothing found,
// and that empty result is the evidence.
func TestStorage_NoDefaultClass(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "app-data")} })
	snap := collectRefsFor(t, pod, boundPVC("app-data", "", nil))

	if !snap.PVCs[0].UsesDefaultClass || len(snap.StorageClasses) != 0 || len(snap.Errors) != 0 {
		t.Errorf("pvc = %+v, classes = %+v, errors = %+v", snap.PVCs[0], snap.StorageClasses, snap.Errors)
	}
}

// storageClassName: "" means "no class": don't look one up.
func TestStorage_EmptyClassMeansNone(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "app-data")} })
	snap := collectRefsFor(t, pod, boundPVC("app-data", "", strPtr("")), realStorageClass(t))

	if snap.PVCs[0].UsesDefaultClass || len(snap.StorageClasses) != 0 {
		t.Errorf("pvc = %+v, classes = %+v, want no class at all", snap.PVCs[0], snap.StorageClasses)
	}
}

// --- How claims are found -------------------------------------------------------

// Two volumes mounting the same claim → fetched once, both listed.
// A generic ephemeral volume → its PVC is named "<pod>-<volume>".
func TestStorage_ClaimDiscovery(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) {
		p.Spec.Volumes = []corev1.Volume{
			pvcVolume("data", "shared"),
			pvcVolume("data-again", "shared"),
			{Name: "scratch", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
	})
	claims := podClaims(pod)
	if len(claims) != 2 {
		t.Fatalf("claims = %v, want shared and app-scratch", claims)
	}
	if u := claims["shared"]; len(u) != 2 || u[0] != "data" || u[1] != "data-again" {
		t.Errorf("shared used by %v, want [data data-again]", u)
	}
	if u := claims["app-scratch"]; len(u) != 1 || u[0] != "scratch" {
		t.Errorf("ephemeral claim = %v, want app-scratch used by scratch", claims)
	}
}

// --- Rule 3 -------------------------------------------------------------------

// StorageClasses can't be read: unreadable (not missing), error recorded,
// the PVC still collected.
func TestStorage_DeniedStorageClassIsUnreadable(t *testing.T) {
	client := fake.NewSimpleClientset(append([]runtime.Object{loadPod(t, "storage-api")}, realClusterObjects(t)...)...)
	client.PrependReactor("get", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "fast-ssd", errors.New("RBAC"))
	})
	c := New(client)
	c.FetchLogs = fakeLogs{}.fetch

	snap, err := c.Collect(context.Background(), ns, "storage-api")
	if err != nil {
		t.Fatalf("Collect should not fail: %v", err)
	}
	if len(snap.StorageClasses) != 1 || snap.StorageClasses[0].State != snapshot.Unreadable {
		t.Errorf("storageClasses = %+v, want fast-ssd unreadable (we can't say it's missing)", snap.StorageClasses)
	}
	if len(snap.Errors) != 1 || snap.Errors[0].Step != "storage" || !snap.Errors[0].Denied {
		t.Errorf("errors = %+v, want one denied storage error", snap.Errors)
	}
	if len(snap.PVCs) != 1 || snap.PVCs[0].State != snapshot.Found {
		t.Error("the PVC should still be collected")
	}
}
