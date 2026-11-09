// Package cluster connects Culprit to a Kubernetes cluster.
//
// It finds the cluster the same way kubectl does: the --kubeconfig flag, then
// the KUBECONFIG environment variable, then ~/.kube/config. The kubeconfig only
// says *how* to reach the cluster (API server address + credentials); the
// cluster's data comes from the API server itself.
package cluster

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/version"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// DefaultTimeout is how long Connect waits for the API server to answer.
const DefaultTimeout = 5 * time.Second

// Connection is a checked, ready-to-use link to a cluster.
type Connection struct {
	Client        kubernetes.Interface // used for every read request
	Context       string               // kubeconfig context in use
	Server        string               // API server address
	Namespace     string               // namespace to look in
	ServerVersion string               // e.g. "v1.31.0", proves the server answered
}

// Connect loads the kubeconfig chosen by flags, builds a client and checks
// that the API server answers within timeout. It fails fast with a clear
// message instead of hanging when the cluster is down.
func Connect(flags *genericclioptions.ConfigFlags, timeout time.Duration) (*Connection, error) {
	loader := flags.ToRawKubeConfigLoader()

	raw, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	contextName := raw.CurrentContext
	if flags.Context != nil && *flags.Context != "" {
		contextName = *flags.Context
	}

	cfg, err := flags.ToRESTConfig()
	if err != nil {
		if clientcmd.IsEmptyConfig(err) {
			return nil, errors.New("no kubeconfig found: set KUBECONFIG, use --kubeconfig, or create ~/.kube/config")
		}
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	ns, _, err := loader.Namespace()
	if err != nil {
		return nil, fmt.Errorf("resolve namespace: %w", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	// Check the connection on a copy of the config that has a hard timeout,
	// so a dead cluster gives an error instead of a hang.
	pingCfg := rest.CopyConfig(cfg)
	pingCfg.Timeout = timeout
	pinger, err := discovery.NewDiscoveryClientForConfig(pingCfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	info, err := checkConnection(pinger)
	if err != nil {
		return nil, fmt.Errorf("cluster %q at %s did not answer within %s (is it running?): %w",
			contextName, cfg.Host, timeout, err)
	}

	return &Connection{
		Client:        client,
		Context:       contextName,
		Server:        cfg.Host,
		Namespace:     ns,
		ServerVersion: info.GitVersion,
	}, nil
}

// checkConnection asks the API server for its version: the cheapest request
// that proves the server is up and our credentials are accepted.
func checkConnection(v discovery.ServerVersionInterface) (*version.Info, error) {
	return v.ServerVersion()
}
