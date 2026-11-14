// Command kubectl-culprit traces a failing pod back to its root cause.
// Installed on PATH, it also runs as `kubectl culprit`.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/culprit/culprit/internal/cluster"
	"github.com/culprit/culprit/internal/collect"
	"github.com/culprit/culprit/internal/diagnose"
	"github.com/culprit/culprit/internal/snapshot"
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
	var (
		timeout = cluster.DefaultTimeout
		output  string
		from    string
	)

	cmd := &cobra.Command{
		Use:   "kubectl-culprit POD",
		Short: "Trace a failing pod back to its root cause",
		Example: `  kubectl culprit crash-api                  # the chain, symptom to root cause
  kubectl culprit crash-api -o json          # the diagnosis as JSON
  kubectl culprit crash-api -o snapshot > snap.json   # only collect the evidence
  kubectl culprit --from snap.json           # diagnose a saved snapshot, no cluster needed`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch output {
			case "text", "json", "snapshot":
			default:
				return fmt.Errorf("unknown output %q: use text, json or snapshot", output)
			}

			var snap *snapshot.Snapshot
			switch {
			case from != "" && len(args) > 0:
				return errors.New("give a pod name or --from, not both")
			case from != "":
				s, err := readSnapshot(from)
				if err != nil {
					return err
				}
				snap = s
			case len(args) == 0:
				return errors.New("which pod? usage: kubectl culprit POD")
			default:
				conn, err := cluster.Connect(configFlags, timeout)
				if err != nil {
					return err
				}
				// Status goes to stderr so stdout is only the answer
				// (safe to redirect: kubectl-culprit crash-api -o snapshot > snap.json).
				fmt.Fprintf(cmd.ErrOrStderr(), "connected to %s (%s, Kubernetes %s), namespace %q, pod %q\n",
					conn.Context, conn.Server, conn.ServerVersion, conn.Namespace, args[0])

				snap, err = collect.New(conn.Client).Collect(cmd.Context(), conn.Namespace, args[0])
				if err != nil {
					return err
				}
				snap.Context = conn.Context
				if n := len(snap.Errors); n > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d read(s) failed, see \"errors\" in the snapshot\n", n)
				}
			}

			if output == "snapshot" {
				return writeJSON(cmd, snap)
			}
			d := diagnose.Diagnose(snap)
			if output == "json" {
				return writeJSON(cmd, d)
			}
			diagnose.Render(cmd.OutOrStdout(), d)
			return nil
		},
	}

	configFlags.AddFlags(cmd.Flags())
	cmd.Flags().DurationVar(&timeout, "connect-timeout", cluster.DefaultTimeout, "how long to wait for the cluster to answer")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "what to print: text (the chain), json (the diagnosis) or snapshot (the collected evidence)")
	cmd.Flags().StringVar(&from, "from", "", "diagnose a saved snapshot file instead of a live pod")
	return cmd
}

func readSnapshot(path string) (*snapshot.Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap snapshot.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("%s is not a snapshot: %w", path, err)
	}
	if snap.Pod == nil {
		return nil, fmt.Errorf("%s is not a snapshot: it has no pod", path)
	}
	return &snap, nil
}

func writeJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
