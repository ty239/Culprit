package collect

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// collectRefs fetches the ConfigMaps, Secrets, and ServiceAccount the pod
// references. Maps are keyed by object name; a referenced object that does not
// exist is recorded with a nil value. Secret data is stripped so credentials
// never end up in a snapshot.
func collectRefs(ctx context.Context, client kubernetes.Interface, ns string, pod *corev1.Pod) (
	map[string]*corev1.ConfigMap,
	map[string]*corev1.Secret,
	*corev1.ServiceAccount,
	error,
) {
	cmNames, secretNames := referencedNames(pod)

	cms := make(map[string]*corev1.ConfigMap)
	for name := range cmNames {
		cm, err := client.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			cms[name] = nil
		case err != nil:
			return nil, nil, nil, fmt.Errorf("get configmap %s/%s: %w", ns, name, err)
		default:
			cms[name] = cm
		}
	}

	secrets := make(map[string]*corev1.Secret)
	for name := range secretNames {
		s, err := client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			secrets[name] = nil
		case err != nil:
			return nil, nil, nil, fmt.Errorf("get secret %s/%s: %w", ns, name, err)
		default:
			s.Data = nil
			s.StringData = nil
			// last-applied-configuration and managedFields can both carry the
			// secret's values verbatim.
			delete(s.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
			s.ManagedFields = nil
			secrets[name] = s
		}
	}

	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}
	sa, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		sa, err = nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get serviceaccount %s/%s: %w", ns, saName, err)
	}

	return cms, secrets, sa, nil
}

// referencedNames returns the sets of ConfigMap and Secret names used by the
// pod's volumes, env vars, envFrom sources, and image pull secrets.
func referencedNames(pod *corev1.Pod) (cms, secrets map[string]struct{}) {
	cms = make(map[string]struct{})
	secrets = make(map[string]struct{})

	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap != nil {
			cms[v.ConfigMap.Name] = struct{}{}
		}
		if v.Secret != nil {
			secrets[v.Secret.SecretName] = struct{}{}
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil {
					cms[src.ConfigMap.Name] = struct{}{}
				}
				if src.Secret != nil {
					secrets[src.Secret.Name] = struct{}{}
				}
			}
		}
	}

	for _, ips := range pod.Spec.ImagePullSecrets {
		secrets[ips.Name] = struct{}{}
	}

	containers := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	for _, c := range containers {
		for _, ef := range c.EnvFrom {
			if ef.ConfigMapRef != nil {
				cms[ef.ConfigMapRef.Name] = struct{}{}
			}
			if ef.SecretRef != nil {
				secrets[ef.SecretRef.Name] = struct{}{}
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if e.ValueFrom.ConfigMapKeyRef != nil {
				cms[e.ValueFrom.ConfigMapKeyRef.Name] = struct{}{}
			}
			if e.ValueFrom.SecretKeyRef != nil {
				secrets[e.ValueFrom.SecretKeyRef.Name] = struct{}{}
			}
		}
	}

	delete(cms, "")
	delete(secrets, "")
	return cms, secrets
}
