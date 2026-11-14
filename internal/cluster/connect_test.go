package cluster

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes/fake"
)

// writeKubeconfig writes a kubeconfig whose only context points at server.
func writeKubeconfig(t *testing.T, server, namespace string) string {
	t.Helper()
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test-ctx
  context:
    cluster: test
    user: test
    namespace: %s
current-context: test-ctx
users:
- name: test
  user:
    token: fake-token
`, server, namespace)
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func flagsFor(path string) *genericclioptions.ConfigFlags {
	f := genericclioptions.NewConfigFlags(false)
	f.KubeConfig = &path
	return f
}

// fakeAPIServer answers /version like a real API server.
func fakeAPIServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"major":"1","minor":"31","gitVersion":"v1.31.0"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConnect(t *testing.T) {
	srv := fakeAPIServer(t, 0)
	path := writeKubeconfig(t, srv.URL, "culprit-lab")

	conn, err := Connect(flagsFor(path), time.Second)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.Context != "test-ctx" {
		t.Errorf("Context = %q, want test-ctx", conn.Context)
	}
	if conn.Server != srv.URL {
		t.Errorf("Server = %q, want %q", conn.Server, srv.URL)
	}
	if conn.Namespace != "culprit-lab" {
		t.Errorf("Namespace = %q, want culprit-lab (from the context)", conn.Namespace)
	}
	if conn.ServerVersion != "v1.31.0" {
		t.Errorf("ServerVersion = %q, want v1.31.0", conn.ServerVersion)
	}
	if conn.Client == nil {
		t.Error("Client is nil")
	}
}

func TestConnectNamespaceFlagWins(t *testing.T) {
	srv := fakeAPIServer(t, 0)
	flags := flagsFor(writeKubeconfig(t, srv.URL, "culprit-lab"))
	ns := "other"
	flags.Namespace = &ns

	conn, err := Connect(flags, time.Second)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.Namespace != "other" {
		t.Errorf("Namespace = %q, want other (from --namespace)", conn.Namespace)
	}
}

func TestConnectTimesOut(t *testing.T) {
	srv := fakeAPIServer(t, 2*time.Second) // answers too slowly
	path := writeKubeconfig(t, srv.URL, "default")

	start := time.Now()
	_, err := Connect(flagsFor(path), 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Connect took %s, should give up after ~200ms", took)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("error %q should say the cluster did not answer", err)
	}
}

func TestConnectClusterDown(t *testing.T) {
	srv := fakeAPIServer(t, 0)
	url := srv.URL
	srv.Close() // nothing listening any more

	_, err := Connect(flagsFor(writeKubeconfig(t, url, "default")), time.Second)
	if err == nil {
		t.Fatal("expected an error for a stopped cluster, got nil")
	}
}

func TestConnectMissingKubeconfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := Connect(flagsFor(missing), time.Second)
	if err == nil {
		t.Fatal("expected an error for a missing kubeconfig, got nil")
	}
}

func TestCheckConnection(t *testing.T) {
	client := fake.NewSimpleClientset()

	info, err := checkConnection(client.Discovery())
	if err != nil {
		t.Fatalf("checkConnection: %v", err)
	}
	if info == nil {
		t.Fatal("version info is nil")
	}
}
