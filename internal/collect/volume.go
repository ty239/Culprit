package collect

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// collectVolumes fetches every PVC the pod mounts, plus the PV each one is
// bound to and the StorageClass it requests. Maps are keyed by object name.
// A referenced object that does not exist is recorded with a nil value, since
// its absence is often the root cause.
func collectVolumes(ctx context.Context, client kubernetes.Interface, ns string, pod *corev1.Pod) (
	map[string]*corev1.PersistentVolumeClaim,
	map[string]*corev1.PersistentVolume,
	map[string]*storagev1.StorageClass,
	error,
) {
	pvcs := make(map[string]*corev1.PersistentVolumeClaim)
	pvs := make(map[string]*corev1.PersistentVolume)
	scs := make(map[string]*storagev1.StorageClass)

	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		claimName := v.PersistentVolumeClaim.ClaimName
		if _, seen := pvcs[claimName]; seen {
			continue
		}

		pvc, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, claimName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			pvcs[claimName] = nil
			continue
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("get pvc %s/%s: %w", ns, claimName, err)
		}
		pvcs[claimName] = pvc

		if pvName := pvc.Spec.VolumeName; pvName != "" {
			if _, seen := pvs[pvName]; !seen {
				pv, err := client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				switch {
				case apierrors.IsNotFound(err):
					pvs[pvName] = nil
				case err != nil:
					return nil, nil, nil, fmt.Errorf("get pv %s: %w", pvName, err)
				default:
					pvs[pvName] = pv
				}
			}
		}

		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
			scName := *pvc.Spec.StorageClassName
			if _, seen := scs[scName]; !seen {
				sc, err := client.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{})
				switch {
				case apierrors.IsNotFound(err):
					scs[scName] = nil
				case err != nil:
					return nil, nil, nil, fmt.Errorf("get storageclass %s: %w", scName, err)
				default:
					scs[scName] = sc
				}
			}
		}
	}
	return pvcs, pvs, scs, nil
}
