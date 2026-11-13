// Package collect reads everything about one pod from the API server and
// saves it as a snapshot. It only ever reads (get/list), and it never
// works out the root cause; that is the diagnose step's job.
package collect

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/culprit/culprit/internal/snapshot"
)

// Collector gathers the evidence for one pod.
type Collector struct {
	Client kubernetes.Interface

	// FetchLogs reads one container's logs. Leave it nil to read from the
	// API server; tests set it to return real log text.
	FetchLogs LogFetcher

	// Now returns the current time. Leave it nil to use time.Now.
	Now func() time.Time
}

// New returns a Collector that reads from the cluster behind client.
func New(client kubernetes.Interface) *Collector {
	return &Collector{Client: client}
}

// Collect builds the snapshot for pod name in namespace ns.
//
// The pod itself is required: if it can't be read, Collect returns an error,
// because there is nothing to follow. Every other read that fails is
// recorded in Snapshot.Errors and collection carries on.
func (c *Collector) Collect(ctx context.Context, ns, name string) (*snapshot.Snapshot, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}

	pod, err := c.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("pod %q not found in namespace %q", name, ns)
	case apierrors.IsForbidden(err):
		return nil, fmt.Errorf("not allowed to read pod %q in namespace %q: %w", name, ns, err)
	case err != nil:
		return nil, fmt.Errorf("get pod %s/%s: %w", ns, name, err)
	}
	cleanPod(pod)

	snap := &snapshot.Snapshot{
		CollectedAt: now().UTC(),
		Namespace:   ns,
		PodName:     name,
		Pod:         pod,
		Logs:        map[string]snapshot.ContainerLogs{},
		Events:      []snapshot.Event{},
		Nodes:       []snapshot.Node{},
		Errors:      []snapshot.CollectError{},
	}

	c.collectLogs(ctx, snap)
	c.collectEvents(ctx, snap)
	c.collectNodes(ctx, snap)

	return snap, nil
}

// record adds a failed read to the snapshot.
func record(snap *snapshot.Snapshot, step, target string, err error) {
	snap.Errors = append(snap.Errors, snapshot.CollectError{
		Step:    step,
		Target:  target,
		Denied:  apierrors.IsForbidden(err),
		Message: err.Error(),
	})
}
