package collect

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// collectNodes returns the node the pod is scheduled on. If the pod has not
// been scheduled yet, it returns every node so scheduling failures (taints,
// selectors, capacity) can be diagnosed.
func collectNodes(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod) ([]corev1.Node, error) {
	if pod.Spec.NodeName != "" {
		node, err := client.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get node %s: %w", pod.Spec.NodeName, err)
		}
		return []corev1.Node{*node}, nil
	}

	list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return list.Items, nil
}
