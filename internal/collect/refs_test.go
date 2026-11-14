package collect

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/culprit/culprit/internal/snapshot"
)

// Real copies from kind-culprit:
//
//	kubectl get sa default -o yaml --show-managed-fields=false
//	kubectl get cm kube-root-ca.crt -o yaml --show-managed-fields=false
//	kubectl get sc standard -o yaml --show-managed-fields=false
//	kubectl get pvc storage-api-data -o yaml --show-managed-fields=false
func loadYAML(t *testing.T, file string, into any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
}

// realClusterObjects is what every pod in kind-culprit can see: the node,
// the default service account and the kube-root-ca.crt ConfigMap.
func realClusterObjects(t *testing.T) []runtime.Object {
	t.Helper()
	var sa corev1.ServiceAccount
	loadYAML(t, "serviceaccount-default.yaml", &sa)
	var cm corev1.ConfigMap
	loadYAML(t, "configmap-kube-root-ca.crt.yaml", &cm)
	var sc storagev1.StorageClass
	loadYAML(t, "storageclass-standard.yaml", &sc)
	var pvc corev1.PersistentVolumeClaim
	loadYAML(t, "pvc-storage-api-data.yaml", &pvc)
	return []runtime.Object{loadNode(t, "culprit-control-plane"), &sa, &cm, &sc, &pvc}
}

const secretValue = "s3cr3t-hunter2" // must never appear in a snapshot

func secret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			// kubectl apply stores a full copy here, values included.
			Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"password":"` + secretValue + `"}}`},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

// podWith is a minimal pod built for one test.
func podWith(mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns, UID: "app-uid"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox"}}},
	}
	mutate(p)
	return p
}

func collectRefsFor(t *testing.T, pod *corev1.Pod, objs ...runtime.Object) *snapshot.Snapshot {
	t.Helper()
	return collectNodesFor(t, pod, objs...) // same helper: pod + objects in a fake cluster
}

func findRef(refs []snapshot.Reference, name string) *snapshot.Reference {
	for i := range refs {
		if refs[i].Name == name {
			return &refs[i]
		}
	}
	return nil
}

// --- Real pods -------------------------------------------------------------

// Every pod in kind-culprit mounts kube-root-ca.crt through a projected
// volume, and runs as the default service account. Both exist.
func TestRefs_RealProjectedConfigMapAndServiceAccount(t *testing.T) {
	snap := collectFor(t, "crash-api", fakeLogs{})

	cm := findRef(snap.ConfigMaps, "kube-root-ca.crt")
	if cm == nil || cm.State != snapshot.Found {
		t.Fatalf("configMaps = %+v, want kube-root-ca.crt found", snap.ConfigMaps)
	}
	if len(cm.Keys) != 1 || cm.Keys[0] != "ca.crt" {
		t.Errorf("keys = %v, want [ca.crt]", cm.Keys)
	}
	if len(cm.UsedBy) != 1 || cm.UsedBy[0].Via != "projected" || !strings.HasPrefix(cm.UsedBy[0].Name, "kube-api-access-") {
		t.Errorf("usedBy = %+v, want the projected kube-api-access volume", cm.UsedBy)
	}
	if sa := snap.ServiceAccount; sa == nil || sa.Name != "default" || sa.State != snapshot.Found {
		t.Errorf("serviceAccount = %+v, want default/found", sa)
	}
	if len(snap.Secrets) != 0 {
		t.Errorf("crash-api uses no secrets, got %+v", snap.Secrets)
	}
}

// --- Rule 6: never store Secret values --------------------------------------

func TestRefs_SecretValuesNeverStored(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) {
		p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}}}}
	})
	snap := collectRefsFor(t, pod, secret("db", map[string]string{"password": secretValue, "username": "admin-user-xyz"}))

	s := findRef(snap.Secrets, "db")
	if s == nil || s.State != snapshot.Found {
		t.Fatalf("secrets = %+v, want db found", snap.Secrets)
	}
	if len(s.Keys) != 2 || s.Keys[0] != "password" || s.Keys[1] != "username" {
		t.Errorf("keys = %v, want [password username]", s.Keys)
	}
	if s.Type != "Opaque" {
		t.Errorf("type = %q, want Opaque", s.Type)
	}

	// The whole snapshot, as it would be written to disk.
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secretValue, "admin-user-xyz", "last-applied-configuration"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("snapshot contains %q: secret data leaked", leak)
		}
	}
}

// --- Finding references ------------------------------------------------------

func TestRefs_FindsEveryKindOfReference(t *testing.T) {
	no := false
	yes := true
	pod := podWith(func(p *corev1.Pod) {
		p.Spec.InitContainers = []corev1.Container{{
			Name: "init",
			Env: []corev1.EnvVar{{Name: "MODE", ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}, Key: "mode", Optional: &no},
			}}},
		}}
		p.Spec.Containers[0].Env = []corev1.EnvVar{
			{Name: "PLAIN", Value: "not a reference"},
			{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "api"}, Key: "token"},
			}},
		}
		p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}},
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "extra"}, Optional: &yes}},
		}
		p.Spec.Volumes = []corev1.Volume{
			{Name: "certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: "tls", Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "cert"}, {Key: "tls.key", Path: "key"}},
			}}},
			{Name: "conf", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "nginx-conf"},
			}}},
			{Name: "bundle", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
				{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "api"}}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}},
			}}}},
		}
		p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-login"}}
	})

	secrets, cms := findReferences(pod)

	want := map[string][]string{ // name → every "via" expected, in order
		"api":            {"env", "projected"},
		"extra":          {"envFrom"},
		"tls":            {"volume"},
		"registry-login": {"imagePullSecrets"},
	}
	check := func(kind string, got map[string][]snapshot.Usage, want map[string][]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: got %d names %v, want %d", kind, len(got), sortedKeys(got), len(want))
		}
		for name, vias := range want {
			uses := got[name]
			if len(uses) != len(vias) {
				t.Errorf("%s %q: got %d uses %+v, want %v", kind, name, len(uses), uses, vias)
				continue
			}
			for i, via := range vias {
				if uses[i].Via != via {
					t.Errorf("%s %q use %d: via %q, want %q", kind, name, i, uses[i].Via, via)
				}
			}
		}
	}
	check("secret", secrets, want)
	check("configmap", cms, map[string][]string{
		"settings":   {"env", "envFrom", "projected"},
		"nginx-conf": {"volume"},
	})

	// Details: which keys, which container, optional.
	if u := secrets["api"][0]; u.Container != "main" || u.Name != "TOKEN" || len(u.Keys) != 1 || u.Keys[0] != "token" {
		t.Errorf("api env use = %+v", u)
	}
	if u := cms["settings"][0]; u.Container != "init" || u.Keys[0] != "mode" || u.Optional {
		t.Errorf("settings init-container use = %+v", u)
	}
	if !secrets["extra"][0].Optional {
		t.Error("extra is optional")
	}
	if k := secrets["tls"][0].Keys; len(k) != 2 || k[0] != "tls.crt" || k[1] != "tls.key" {
		t.Errorf("tls keys = %v, want [tls.crt tls.key]", k)
	}
}

// One name used in several places is fetched once and listed once.
func TestRefs_SameNameFetchedOnce(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) {
		ref := corev1.LocalObjectReference{Name: "db"}
		p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: ref}}}
		p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{ref}
	})
	client := fake.NewSimpleClientset(pod, secret("db", map[string]string{"k": "v"}))
	gets := 0
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		return false, nil, nil
	})
	c := New(client)
	c.FetchLogs = fakeLogs{}.fetch
	snap, err := c.Collect(context.Background(), ns, "app")
	if err != nil {
		t.Fatal(err)
	}
	if gets != 1 {
		t.Errorf("fetched secret db %d times, want 1", gets)
	}
	if len(snap.Secrets) != 1 || len(snap.Secrets[0].UsedBy) != 2 {
		t.Errorf("secrets = %+v, want db once with 2 uses", snap.Secrets)
	}
}

// --- Rule 1: missing is evidence ---------------------------------------------

func TestRefs_MissingConfigMapAndServiceAccount(t *testing.T) {
	pod := podWith(func(p *corev1.Pod) {
		p.Spec.ServiceAccountName = "deployer"
		p.Spec.Volumes = []corev1.Volume{{Name: "conf", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "nginx-conf"}},
		}}}
	})
	snap := collectRefsFor(t, pod) // neither exists

	if cm := findRef(snap.ConfigMaps, "nginx-conf"); cm == nil || cm.State != snapshot.Missing || len(cm.Keys) != 0 {
		t.Errorf("configMaps = %+v, want nginx-conf missing", snap.ConfigMaps)
	}
	if sa := snap.ServiceAccount; sa == nil || sa.Name != "deployer" || sa.State != snapshot.Missing {
		t.Errorf("serviceAccount = %+v, want deployer/missing", sa)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("missing objects are evidence, not errors: %+v", snap.Errors)
	}
}

// No serviceAccountName means the pod runs as "default".
func TestRefs_EmptyServiceAccountMeansDefault(t *testing.T) {
	var sa corev1.ServiceAccount
	loadYAML(t, "serviceaccount-default.yaml", &sa)
	snap := collectRefsFor(t, podWith(func(*corev1.Pod) {}), &sa)

	if snap.ServiceAccount == nil || snap.ServiceAccount.Name != "default" || snap.ServiceAccount.State != snapshot.Found {
		t.Errorf("serviceAccount = %+v, want default/found", snap.ServiceAccount)
	}
}

// --- Rule 3: denied reads are recorded, collection keeps going ---------------

func TestRefs_DeniedSecretsAreUnreadableNotFatal(t *testing.T) {
	client := fake.NewSimpleClientset(append([]runtime.Object{loadPod(t, "secret-api")}, realClusterObjects(t)...)...)
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "db-credentials", errors.New("RBAC"))
	})
	c := New(client)
	c.FetchLogs = fakeLogs{}.fetch

	snap, err := c.Collect(context.Background(), ns, "secret-api")
	if err != nil {
		t.Fatalf("Collect should not fail when secrets are denied: %v", err)
	}
	s := findRef(snap.Secrets, "db-credentials")
	if s == nil || s.State != snapshot.Unreadable {
		t.Errorf("secret = %+v, want unreadable (NOT missing: we don't know)", s)
	}
	if len(snap.Errors) != 1 || snap.Errors[0].Step != "secrets" || !snap.Errors[0].Denied {
		t.Errorf("errors = %+v, want one denied secrets error", snap.Errors)
	}
	// Everything else still arrives.
	if snap.ServiceAccount.State != snapshot.Found || findRef(snap.ConfigMaps, "kube-root-ca.crt") == nil || len(snap.Nodes) != 1 {
		t.Error("service account, configmaps and nodes should still be collected")
	}
}
