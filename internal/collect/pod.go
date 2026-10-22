package collect

import (
	"bytes"
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const logTailLines int64 = 100

// collectPod fetches the pod object, its events, and the last 100 lines of
// logs for each container (including init containers).
func collectPod(ctx context.Context, client kubernetes.Interface, ns, name string) (
	*corev1.Pod, map[string]string, error,
) {
	pod, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("get pod %s/%s: %w", ns, name, err)
	}

	logs := make(map[string]string)
	containers := append(pod.Spec.InitContainers, pod.Spec.Containers...)
	for _, c := range containers {
		key := fmt.Sprintf("%s/%s", name, c.Name)
		log, err := fetchContainerLogs(ctx, client, ns, name, c.Name)
		if err != nil {
			// Log fetch failures are non-fatal; record the error as the log text.
			logs[key] = fmt.Sprintf("<error fetching logs: %v>", err)
			continue
		}
		logs[key] = log
	}
	return pod, logs, nil
}

// collectEvents returns all events in ns that reference the named pod.
func collectEvents(ctx context.Context, client kubernetes.Interface, ns, podName string) ([]corev1.Event, error) {
	fieldSelector := fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=%s", podName, ns)
	list, err := client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: fieldSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("list events for pod %s/%s: %w", ns, podName, err)
	}
	return list.Items, nil
}

// fetchContainerLogs retrieves the last logTailLines lines from a single container.
func fetchContainerLogs(ctx context.Context, client kubernetes.Interface, ns, pod, container string) (string, error) {
	tail := logTailLines
	opts := &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
	}
	req := client.CoreV1().Pods(ns).GetLogs(pod, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, stream); err != nil {
		return "", err
	}
	return buf.String(), nil
}
