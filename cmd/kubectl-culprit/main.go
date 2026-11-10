// Command kubectl-culprit traces a failing pod back to its root cause.
// Installed on PATH, it also runs as `kubectl culprit`.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/culprit/culprit/internal/cluster"
	"github.com/culprit/culprit/internal/collect"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	// Gives us the standard kubectl flags for free:
	// --kubeconfig, --context, --namespace/-n, --server, --token, ...
	configFlags := genericclioptions.NewConfigFlags(true)
	var timeout = cluster.DefaultTimeout

	cmd := &cobra.Command{
		Use:          "kubectl-culprit POD",
		Short:        "Trace a failing pod back to its root cause",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := cluster.Connect(configFlags, timeout)
			if err != nil {
				return err
			}
			// Status goes to stderr so stdout is only the JSON snapshot
			// (safe to redirect: kubectl-culprit crash-api > snap.json).
			fmt.Fprintf(cmd.ErrOrStderr(), "connected to %s (%s, Kubernetes %s), namespace %q, pod %q\n",
				conn.Context, conn.Server, conn.ServerVersion, conn.Namespace, args[0])

			snap, err := collect.New(conn.Client).Collect(cmd.Context(), conn.Namespace, args[0])
			if err != nil {
				return err
			}
			snap.Context = conn.Context
			if n := len(snap.Errors); n > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d read(s) failed, see \"errors\" in the snapshot\n", n)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(snap)
		},
	}

	configFlags.AddFlags(cmd.Flags())
	cmd.Flags().DurationVar(&timeout, "connect-timeout", cluster.DefaultTimeout, "how long to wait for the cluster to answer")
	return cmd
}
