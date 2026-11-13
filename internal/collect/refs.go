package collect

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/culprit/culprit/internal/snapshot"
)

// collectRefs finds every Secret and ConfigMap the pod uses, checks each one
// exists and records its key names (never its values). It also checks the
// pod's service account exists.
func (c *Collector) collectRefs(ctx context.Context, snap *snapshot.Snapshot) {
	secretUses, configMapUses := findReferences(snap.Pod)

	// Secrets: only the key names and type leave this function. The Secret
	// object itself (with its values) is never stored.
	for _, name := range sortedKeys(secretUses) {
		ref := snapshot.Reference{Name: name, UsedBy: secretUses[name]}
		s, err := c.Client.CoreV1().Secrets(snap.Namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			ref.State = snapshot.Missing
		case err != nil:
			ref.State = snapshot.Unreadable
			record(snap, "secrets", "secrets/"+name, err)
		default:
			ref.State = snapshot.Found
			ref.Type = string(s.Type)
			ref.Keys = keyNames(s.Data, s.StringData)
		}
		snap.Secrets = append(snap.Secrets, ref)
	}

	for _, name := range sortedKeys(configMapUses) {
		ref := snapshot.Reference{Name: name, UsedBy: configMapUses[name]}
		cm, err := c.Client.CoreV1().ConfigMaps(snap.Namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			ref.State = snapshot.Missing
		case err != nil:
			ref.State = snapshot.Unreadable
			record(snap, "configmaps", "configmaps/"+name, err)
		default:
			ref.State = snapshot.Found
			ref.Keys = keyNames(cm.Data, cm.BinaryData)
		}
		snap.ConfigMaps = append(snap.ConfigMaps, ref)
	}

	// A pod with no serviceAccountName runs as "default".
	saName := snap.Pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}
	sa := &snapshot.ServiceAccountRef{Name: saName}
	_, err := c.Client.CoreV1().ServiceAccounts(snap.Namespace).Get(ctx, saName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		sa.State = snapshot.Missing
	case err != nil:
		sa.State = snapshot.Unreadable
		record(snap, "serviceaccount", "serviceaccounts/"+saName, err)
	default:
		sa.State = snapshot.Found
	}
	snap.ServiceAccount = sa
}

// findReferences walks the pod spec and returns, for each Secret and
// ConfigMap name, every place that refers to it. It only reads the pod; it
// makes no requests.
func findReferences(pod *corev1.Pod) (secrets, configMaps map[string][]snapshot.Usage) {
	secrets = map[string][]snapshot.Usage{}
	configMaps = map[string][]snapshot.Usage{}
	add := func(m map[string][]snapshot.Usage, name string, u snapshot.Usage) {
		if name != "" {
			m[name] = append(m[name], u)
		}
	}

	// env, envFrom: per container, init containers included.
	containers := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	for _, ctr := range containers {
		for _, e := range ctr.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.SecretKeyRef; r != nil {
				add(secrets, r.Name, snapshot.Usage{Via: "env", Container: ctr.Name, Name: e.Name, Keys: []string{r.Key}, Optional: isTrue(r.Optional)})
			}
			if r := e.ValueFrom.ConfigMapKeyRef; r != nil {
				add(configMaps, r.Name, snapshot.Usage{Via: "env", Container: ctr.Name, Name: e.Name, Keys: []string{r.Key}, Optional: isTrue(r.Optional)})
			}
		}
		for _, ef := range ctr.EnvFrom {
			if r := ef.SecretRef; r != nil {
				add(secrets, r.Name, snapshot.Usage{Via: "envFrom", Container: ctr.Name, Optional: isTrue(r.Optional)})
			}
			if r := ef.ConfigMapRef; r != nil {
				add(configMaps, r.Name, snapshot.Usage{Via: "envFrom", Container: ctr.Name, Optional: isTrue(r.Optional)})
			}
		}
	}

	// volumes, including projected volumes (which hold several sources).
	for _, v := range pod.Spec.Volumes {
		if s := v.Secret; s != nil {
			add(secrets, s.SecretName, snapshot.Usage{Via: "volume", Name: v.Name, Keys: itemKeys(s.Items), Optional: isTrue(s.Optional)})
		}
		if cm := v.ConfigMap; cm != nil {
			add(configMaps, cm.Name, snapshot.Usage{Via: "volume", Name: v.Name, Keys: itemKeys(cm.Items), Optional: isTrue(cm.Optional)})
		}
		if p := v.Projected; p != nil {
			for _, src := range p.Sources {
				if s := src.Secret; s != nil {
					add(secrets, s.Name, snapshot.Usage{Via: "projected", Name: v.Name, Keys: itemKeys(s.Items), Optional: isTrue(s.Optional)})
				}
				if cm := src.ConfigMap; cm != nil {
					add(configMaps, cm.Name, snapshot.Usage{Via: "projected", Name: v.Name, Keys: itemKeys(cm.Items), Optional: isTrue(cm.Optional)})
				}
			}
		}
	}

	// imagePullSecrets: the credentials used to download private images.
	for _, ips := range pod.Spec.ImagePullSecrets {
		add(secrets, ips.Name, snapshot.Usage{Via: "imagePullSecrets"})
	}

	return secrets, configMaps
}

// keyNames returns the sorted key names of one or two data maps, whatever
// the value type. Only the names are read, never the values.
func keyNames[A, B any](a map[string]A, b map[string]B) []string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func itemKeys(items []corev1.KeyToPath) []string {
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	return keys
}

func sortedKeys(m map[string][]snapshot.Usage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isTrue(b *bool) bool { return b != nil && *b }
