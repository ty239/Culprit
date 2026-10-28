package collect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCollectNodes(t *testing.T) {
	tests := []struct {
		name      string
		nodeName  string
		wantNodes []string
		wantErr   bool
	}{
		{name: "scheduled pod returns only its node", nodeName: "node-b", wantNodes: []string{"node-b"}},
		{name: "unscheduled pod returns every node", nodeName: "", wantNodes: []string{"node-a", "node-b", "node-c"}},
		{name: "scheduled on a node that no longer exists", nodeName: "gone", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(newNode("node-a"), newNode("node-b"), newNode("node-c"))
			pod := newPod("web", func(p *corev1.Pod) { p.Spec.NodeName = tt.nodeName })

			nodes, err := collectNodes(context.Background(), client, pod)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("collectNodes: %v", err)
			}

			got := map[string]bool{}
			for _, n := range nodes {
				got[n.Name] = true
			}
			if len(got) != len(tt.wantNodes) {
				t.Fatalf("got nodes %v, want %v", got, tt.wantNodes)
			}
			for _, want := range tt.wantNodes {
				if !got[want] {
					t.Errorf("missing node %q in %v", want, got)
				}
			}
		})
	}
}
