// Package cli wires the kubectl-culprit command line to the collectors.
package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/culprit/culprit/internal/collect"
)

type options struct {
	kubeconfig string
	context    string
	namespace  string
}

// Execute runs the root command.
func Execute() error {
	return newRootCmd().Execute()
}

func newRootCmd() *cobra.Command {
	opts := &options{}

	cmd := &cobra.Command{
		Use:          "kubectl-culprit POD",
		Short:        "Trace a failing pod back to its root cause",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, opts, args[0])
		},
	}

	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	cmd.Flags().StringVar(&opts.context, "context", "", "kubeconfig context to use")
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "namespace of the pod (defaults to the context namespace)")
	return cmd
}

func run(cmd *cobra.Command, opts *options, podName string) error {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = opts.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.context}
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	ns := opts.namespace
	if ns == "" {
		var err error
		if ns, _, err = clientConfig.Namespace(); err != nil {
			return fmt.Errorf("resolve namespace: %w", err)
		}
	}

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	snap, err := collect.Collect(cmd.Context(), client, ns, podName)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}
