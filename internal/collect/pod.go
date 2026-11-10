package collect

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/culprit/culprit/internal/snapshot"
)

const (
	// logTailLines is how many lines to keep from the end of each log.
	logTailLines int64 = 100
	// maxLogBytes caps one log, in case a single line is enormous.
	maxLogBytes int64 = 256 * 1024
)

// LogFetcher reads the logs of one container, as described by opts.
type LogFetcher func(ctx context.Context, ns, pod string, opts *corev1.PodLogOptions) (string, error)

// apiLogFetcher reads logs from the API server (GET .../pods/NAME/log).
func apiLogFetcher(client kubernetes.Interface) LogFetcher {
	return func(ctx context.Context, ns, pod string, opts *corev1.PodLogOptions) (string, error) {
		stream, err := client.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
		if err != nil {
			return "", err
		}
		defer stream.Close()
		b, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
		return string(b), err
	}
}

// cleanPod drops bookkeeping fields that add size but no evidence.
func cleanPod(pod *corev1.Pod) {
	pod.ManagedFields = nil
	delete(pod.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
}

// collectLogs reads the current and previous logs of every container.
func (c *Collector) collectLogs(ctx context.Context, snap *snapshot.Snapshot) {
	fetch := c.FetchLogs
	if fetch == nil {
		fetch = apiLogFetcher(c.Client)
	}

	pod := snap.Pod
	statuses := map[string]corev1.ContainerStatus{}
	for _, s := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		statuses[s.Name] = s
	}

	containers := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	for _, ctr := range containers {
		logs := snapshot.ContainerLogs{
			Current: c.readLog(ctx, fetch, snap, ctr.Name, false),
		}

		// Only ask for the previous run if there was one; otherwise we
		// already know the answer and save a request.
		if st, ok := statuses[ctr.Name]; ok && (st.RestartCount > 0 || st.LastTerminationState.Terminated != nil) {
			logs.Previous = c.readLog(ctx, fetch, snap, ctr.Name, true)
		} else {
			logs.Previous = snapshot.Log{Unavailable: "container has not restarted"}
		}

		snap.Logs[ctr.Name] = logs
	}
}

// readLog fetches one log. "No log" answers from the API server are normal
// and become Unavailable; real failures (e.g. access denied) are recorded
// in snap.Errors.
func (c *Collector) readLog(ctx context.Context, fetch LogFetcher, snap *snapshot.Snapshot, container string, previous bool) snapshot.Log {
	tail := logTailLines
	opts := &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &tail}

	text, err := fetch(ctx, snap.Namespace, snap.PodName, opts)
	switch {
	case err == nil:
		if text == "" {
			return snapshot.Log{Unavailable: "log is empty"}
		}
		return snapshot.Log{Text: text}

	// The API server answers 400 Bad Request when there is no log to give,
	// e.g. "container is waiting to start" or "previous terminated
	// container not found". That's evidence, not a failure.
	case apierrors.IsBadRequest(err), apierrors.IsNotFound(err):
		return snapshot.Log{Unavailable: err.Error()}

	default:
		target := fmt.Sprintf("pods/%s/log?container=%s&previous=%t", snap.PodName, container, previous)
		record(snap, "logs", target, err)
		return snapshot.Log{Unavailable: "could not read log (see errors)"}
	}
}
