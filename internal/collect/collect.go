// Package collect gathers all Kubernetes objects needed to diagnose a pod failure.
package collect

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"

	"github.com/culprit/culprit/internal/model"
)

// Collect builds a complete Snapshot for the named pod by running all
// sub-collectors in parallel.
func Collect(ctx context.Context, client kubernetes.Interface, ns, name string) (*model.Snapshot, error) {
	snap := &model.Snapshot{}

	// Phase 1: fetch the pod itself (everything else depends on it).
	pod, logs, err := collectPod(ctx, client, ns, name)
	if err != nil {
		return nil, fmt.Errorf("collect pod: %w", err)
	}
	snap.Pod = pod
	snap.ContainerLogs = logs

	// Phase 2: run remaining collectors in parallel.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		events, err := collectEvents(gctx, client, ns, name)
		if err != nil {
			return fmt.Errorf("collect events: %w", err)
		}
		snap.Events = events
		return nil
	})

	g.Go(func() error {
		nodes, err := collectNodes(gctx, client, pod)
		if err != nil {
			return fmt.Errorf("collect nodes: %w", err)
		}
		snap.Nodes = nodes
		return nil
	})

	g.Go(func() error {
		pvcs, pvs, scs, err := collectVolumes(gctx, client, ns, pod)
		if err != nil {
			return fmt.Errorf("collect volumes: %w", err)
		}
		snap.PVCs = pvcs
		snap.PVs = pvs
		snap.StorageClasses = scs
		return nil
	})

	g.Go(func() error {
		cms, secrets, sa, err := collectRefs(gctx, client, ns, pod)
		if err != nil {
			return fmt.Errorf("collect refs: %w", err)
		}
		snap.ConfigMaps = cms
		snap.Secrets = secrets
		snap.ServiceAccount = sa
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return snap, nil
}
