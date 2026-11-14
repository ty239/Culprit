package collect

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/culprit/culprit/internal/snapshot"
)

// Annotations that mark the cluster's default StorageClass.
var defaultClassAnnotations = []string{
	"storageclass.kubernetes.io/is-default-class",
	"storageclass.beta.kubernetes.io/is-default-class",
}

// collectStorage follows the pod's storage chain:
//
//	pod volume → PVC → PV it's bound to
//	                 → StorageClass it asks for
//
// Each link that doesn't exist is recorded as missing; that's usually the
// root cause of a pod stuck Pending on storage.
func (c *Collector) collectStorage(ctx context.Context, snap *snapshot.Snapshot) {
	claims := podClaims(snap.Pod)
	if len(claims) == 0 {
		return
	}

	pvNames := map[string]bool{}
	classNames := map[string]bool{}
	needDefaultClass := false

	// 1. PVCs.
	for _, name := range sortedClaimNames(claims) {
		out := snapshot.PVC{Name: name, UsedBy: claims[name]}
		pvc, err := c.Client.CoreV1().PersistentVolumeClaims(snap.Namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			out.State = snapshot.Missing
		case err != nil:
			out.State = snapshot.Unreadable
			record(snap, "storage", "persistentvolumeclaims/"+name, err)
		default:
			out = toPVC(pvc, claims[name])
			if out.VolumeName != "" {
				pvNames[out.VolumeName] = true
			}
			switch {
			case out.UsesDefaultClass:
				needDefaultClass = true
			case out.StorageClassName != "":
				classNames[out.StorageClassName] = true
			}
		}
		snap.PVCs = append(snap.PVCs, out)
	}

	// 2. PVs the claims are bound to.
	for _, name := range sortedSet(pvNames) {
		out := snapshot.PV{Name: name}
		pv, err := c.Client.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			out.State = snapshot.Missing
		case err != nil:
			out.State = snapshot.Unreadable
			record(snap, "storage", "persistentvolumes/"+name, err)
		default:
			out = toPV(pv)
			if out.StorageClassName != "" {
				classNames[out.StorageClassName] = true
			}
		}
		snap.PVs = append(snap.PVs, out)
	}

	// 3. StorageClasses, by name.
	for _, name := range sortedSet(classNames) {
		out := snapshot.StorageClass{Name: name}
		sc, err := c.Client.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			out.State = snapshot.Missing
		case err != nil:
			out.State = snapshot.Unreadable
			record(snap, "storage", "storageclasses/"+name, err)
		default:
			out = toStorageClass(sc)
		}
		snap.StorageClasses = append(snap.StorageClasses, out)
	}

	// A PVC that names no class gets the cluster default, so find it. If
	// there is none, the empty result is the evidence.
	if needDefaultClass {
		list, err := c.Client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
		if err != nil {
			record(snap, "storage", "storageclasses", err)
		} else {
			for i := range list.Items {
				sc := toStorageClass(&list.Items[i])
				if sc.IsDefault && !classNames[sc.Name] {
					snap.StorageClasses = append(snap.StorageClasses, sc)
				}
			}
		}
	}
	sort.Slice(snap.StorageClasses, func(i, j int) bool { return snap.StorageClasses[i].Name < snap.StorageClasses[j].Name })
}

// podClaims maps each PVC name the pod uses to the volume names that use it.
// Generic ephemeral volumes get a PVC named "<pod>-<volume>".
func podClaims(pod *corev1.Pod) map[string][]string {
	claims := map[string][]string{}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "":
			claims[v.PersistentVolumeClaim.ClaimName] = append(claims[v.PersistentVolumeClaim.ClaimName], v.Name)
		case v.Ephemeral != nil:
			name := pod.Name + "-" + v.Name
			claims[name] = append(claims[name], v.Name)
		}
	}
	return claims
}

func toPVC(pvc *corev1.PersistentVolumeClaim, usedBy []string) snapshot.PVC {
	out := snapshot.PVC{
		Name:        pvc.Name,
		State:       snapshot.Found,
		UsedBy:      usedBy,
		Phase:       string(pvc.Status.Phase),
		VolumeName:  pvc.Spec.VolumeName,
		AccessModes: accessModes(pvc.Spec.AccessModes),
	}
	// nil = "use the default class"; "" = "no class, bind to an existing PV".
	if pvc.Spec.StorageClassName == nil {
		out.UsesDefaultClass = true
	} else {
		out.StorageClassName = *pvc.Spec.StorageClassName
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		out.Requested = q.String()
	}
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		out.Capacity = q.String()
	}
	for _, c := range pvc.Status.Conditions {
		out.Conditions = append(out.Conditions, snapshot.Condition{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return out
}

func toPV(pv *corev1.PersistentVolume) snapshot.PV {
	out := snapshot.PV{
		Name:             pv.Name,
		State:            snapshot.Found,
		Phase:            string(pv.Status.Phase),
		Reason:           pv.Status.Reason,
		Message:          pv.Status.Message,
		StorageClassName: pv.Spec.StorageClassName,
		AccessModes:      accessModes(pv.Spec.AccessModes),
		ReclaimPolicy:    string(pv.Spec.PersistentVolumeReclaimPolicy),
	}
	if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		out.Capacity = q.String()
	}
	if r := pv.Spec.ClaimRef; r != nil {
		out.ClaimRef = r.Namespace + "/" + r.Name
	}
	return out
}

func toStorageClass(sc *storagev1.StorageClass) snapshot.StorageClass {
	out := snapshot.StorageClass{
		Name:        sc.Name,
		State:       snapshot.Found,
		Provisioner: sc.Provisioner,
	}
	if sc.VolumeBindingMode != nil {
		out.VolumeBindingMode = string(*sc.VolumeBindingMode)
	}
	if sc.ReclaimPolicy != nil {
		out.ReclaimPolicy = string(*sc.ReclaimPolicy)
	}
	for _, a := range defaultClassAnnotations {
		if sc.Annotations[a] == "true" {
			out.IsDefault = true
		}
	}
	return out
}

func accessModes(modes []corev1.PersistentVolumeAccessMode) []string {
	var out []string
	for _, m := range modes {
		out = append(out, string(m))
	}
	return out
}

func sortedClaimNames(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSet(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
