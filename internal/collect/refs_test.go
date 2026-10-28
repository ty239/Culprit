package collect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReferencedNames(t *testing.T) {
	pod := newPod("web", func(p *corev1.Pod) {
		p.Spec.Volumes = []corev1.Volume{
			{Name: "v1", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cm-volume"}}}},
			{Name: "v2", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "secret-volume"}}},
			{Name: "v3", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "cm-projected"}}},
					{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "secret-projected"}}},
				}}}},
		}
		p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "secret-pull"}}
		p.Spec.InitContainers = []corev1.Container{{
			Name: "init",
			EnvFrom: []corev1.EnvFromSource{
				{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm-envfrom"}}},
			},
		}}
		p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "secret-envfrom"}}},
		}
		p.Spec.Containers[0].Env = []corev1.EnvVar{
			{Name: "PLAIN", Value: "x"},
			{Name: "A", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cm-env"}, Key: "k"}}},
			{Name: "B", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "secret-env"}, Key: "k"}}},
			// An empty name must not produce an empty-string entry.
			{Name: "C", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{Key: "k"}}},
		}
	})

	cms, secrets := referencedNames(pod)

	wantCMs := []string{"cm-volume", "cm-projected", "cm-envfrom", "cm-env"}
	wantSecrets := []string{"secret-volume", "secret-projected", "secret-pull", "secret-envfrom", "secret-env"}

	assertSet(t, "configmaps", cms, wantCMs)
	assertSet(t, "secrets", secrets, wantSecrets)
}

func TestCollectRefs(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: testNS}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-creds", Namespace: testNS},
		Data:       map[string][]byte{"password": []byte("hunter2")},
		StringData: map[string]string{"user": "admin"},
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: testNS}}
	client := fake.NewSimpleClientset(cm, secret, sa)

	pod := newPod("web", func(p *corev1.Pod) {
		p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}},
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing-config"}}},
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"}}},
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing-secret"}}},
		}
	})

	cms, secrets, gotSA, err := collectRefs(context.Background(), client, testNS, pod)
	if err != nil {
		t.Fatalf("collectRefs: %v", err)
	}

	if cms["app-config"] == nil {
		t.Error("ConfigMap app-config was not collected")
	}
	if v, ok := cms["missing-config"]; !ok || v != nil {
		t.Errorf("want cms[missing-config] = nil entry, got %v (present=%v)", v, ok)
	}
	if v, ok := secrets["missing-secret"]; !ok || v != nil {
		t.Errorf("want secrets[missing-secret] = nil entry, got %v (present=%v)", v, ok)
	}

	s := secrets["db-creds"]
	if s == nil {
		t.Fatal("Secret db-creds was not collected")
	}
	if s.Data != nil || s.StringData != nil {
		t.Errorf("secret values leaked into snapshot: Data=%v StringData=%v", s.Data, s.StringData)
	}

	// Pod has no ServiceAccountName, so "default" is used.
	if gotSA == nil || gotSA.Name != "default" {
		t.Errorf("ServiceAccount = %v, want default", gotSA)
	}
}

func TestCollectRefsMissingServiceAccount(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := newPod("web", func(p *corev1.Pod) { p.Spec.ServiceAccountName = "builder" })

	_, _, sa, err := collectRefs(context.Background(), client, testNS, pod)
	if err != nil {
		t.Fatalf("a missing ServiceAccount should not be an error, got %v", err)
	}
	if sa != nil {
		t.Errorf("ServiceAccount = %v, want nil", sa)
	}
}

func assertSet(t *testing.T, label string, got map[string]struct{}, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d names %v, want %d %v", label, len(got), got, len(want), want)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("%s: missing %q", label, w)
		}
	}
}
