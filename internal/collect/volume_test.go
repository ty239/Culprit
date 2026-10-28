package collect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCollectVolumesFollowsChain(t *testing.T) {
	client := fake.NewSimpleClientset(
		newPVC("data", "pv-1", "fast"),
		newPV("pv-1"),
		newStorageClass("fast"),
	)
	pod := newPod("web", func(p *corev1.Pod) {
		p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "data")}
	})

	pvcs, pvs, scs, err := collectVolumes(context.Background(), client, testNS, pod)
	if err != nil {
		t.Fatalf("collectVolumes: %v", err)
	}
	if pvcs["data"] == nil {
		t.Error("PVC data was not collected")
	}
	if pvs["pv-1"] == nil {
		t.Error("PV pv-1 was not collected")
	}
	if scs["fast"] == nil {
		t.Error("StorageClass fast was not collected")
	}
}

// A missing object is often the root cause, so it must be recorded as a nil
// entry rather than silently dropped or treated as an error.
func TestCollectVolumesRecordsMissingAsNil(t *testing.T) {
	t.Run("missing PVC", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		pod := newPod("web", func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "data")} })

		pvcs, pvs, scs, err := collectVolumes(context.Background(), client, testNS, pod)
		if err != nil {
			t.Fatalf("collectVolumes: %v", err)
		}
		if v, ok := pvcs["data"]; !ok || v != nil {
			t.Errorf("want pvcs[data] = nil entry, got %v (present=%v)", v, ok)
		}
		if len(pvs) != 0 || len(scs) != 0 {
			t.Errorf("want no PVs or StorageClasses, got %d and %d", len(pvs), len(scs))
		}
	})

	t.Run("missing PV and StorageClass", func(t *testing.T) {
		client := fake.NewSimpleClientset(newPVC("data", "pv-gone", "no-such-class"))
		pod := newPod("web", func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "data")} })

		_, pvs, scs, err := collectVolumes(context.Background(), client, testNS, pod)
		if err != nil {
			t.Fatalf("collectVolumes: %v", err)
		}
		if v, ok := pvs["pv-gone"]; !ok || v != nil {
			t.Errorf("want pvs[pv-gone] = nil entry, got %v (present=%v)", v, ok)
		}
		if v, ok := scs["no-such-class"]; !ok || v != nil {
			t.Errorf("want scs[no-such-class] = nil entry, got %v (present=%v)", v, ok)
		}
	})
}

func TestCollectVolumesUnboundPVC(t *testing.T) {
	// A Pending PVC has no VolumeName and no StorageClass: only the PVC is recorded.
	client := fake.NewSimpleClientset(newPVC("data", "", ""))
	pod := newPod("web", func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{pvcVolume("data", "data")} })

	pvcs, pvs, scs, err := collectVolumes(context.Background(), client, testNS, pod)
	if err != nil {
		t.Fatalf("collectVolumes: %v", err)
	}
	if pvcs["data"] == nil {
		t.Error("PVC data was not collected")
	}
	if len(pvs) != 0 || len(scs) != 0 {
		t.Errorf("want no PVs or StorageClasses, got %d and %d", len(pvs), len(scs))
	}
}

func TestCollectVolumesIgnoresNonPVCVolumes(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := newPod("web", func(p *corev1.Pod) {
		p.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	})

	pvcs, pvs, scs, err := collectVolumes(context.Background(), client, testNS, pod)
	if err != nil {
		t.Fatalf("collectVolumes: %v", err)
	}
	if len(pvcs)+len(pvs)+len(scs) != 0 {
		t.Errorf("want nothing collected, got pvcs=%d pvs=%d scs=%d", len(pvcs), len(pvs), len(scs))
	}
}

func TestCollectVolumesDeduplicates(t *testing.T) {
	client := fake.NewSimpleClientset(newPVC("shared", "pv-1", "fast"), newPV("pv-1"), newStorageClass("fast"))
	pod := newPod("web", func(p *corev1.Pod) {
		p.Spec.Volumes = []corev1.Volume{pvcVolume("a", "shared"), pvcVolume("b", "shared")}
	})

	if _, _, _, err := collectVolumes(context.Background(), client, testNS, pod); err != nil {
		t.Fatalf("collectVolumes: %v", err)
	}

	gets := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "persistentvolumeclaims" {
			gets++
		}
	}
	if gets != 1 {
		t.Errorf("PVC fetched %d times, want 1", gets)
	}
}
