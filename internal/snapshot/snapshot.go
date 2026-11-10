// Package snapshot defines the evidence file that the collect step writes and
// the diagnose step reads. It holds facts only, no conclusions.
package snapshot

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Snapshot is everything collected about one pod at one moment.
type Snapshot struct {
	CollectedAt time.Time `json:"collectedAt"`
	Context     string    `json:"context,omitempty"`
	Namespace   string    `json:"namespace"`
	PodName     string    `json:"podName"`

	// Pod is the pod exactly as the API server stored it: phase, conditions,
	// per-container state, last termination, resources. This never expires,
	// unlike events.
	Pod *corev1.Pod `json:"pod"`

	// Logs is keyed by container name (init containers included).
	Logs map[string]ContainerLogs `json:"logs"`

	// Events are the cluster's notes about this pod, oldest first.
	// Kubernetes deletes events after about an hour, so an empty list does
	// not mean nothing happened; Pod.Status is the lasting record.
	Events []Event `json:"events"`

	// Errors lists every read that failed. A failed read never stops the
	// collection; it is recorded here and collect moves on.
	Errors []CollectError `json:"errors"`
}

// ContainerLogs holds the logs of one container's current run and the run
// before it (what `kubectl logs --previous` shows after a crash).
type ContainerLogs struct {
	Current  Log `json:"current"`
	Previous Log `json:"previous"`
}

// Log is the tail of one container run. When there is no log, Unavailable
// says why. That is normal: a container that never started has no logs.
type Log struct {
	Text        string `json:"text,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
}

// Event is one event about the pod, copied as-is (no interpretation).
type Event struct {
	Type      string    `json:"type"`                // "Normal" or "Warning"
	Reason    string    `json:"reason"`              // e.g. "FailedScheduling", "BackOff"
	Message   string    `json:"message"`             // the full text from the cluster
	Count     int32     `json:"count"`               // how many times it happened
	FirstSeen time.Time `json:"firstSeen,omitzero"`  // when it first happened
	LastSeen  time.Time `json:"lastSeen,omitzero"`   // when it last happened
	Source    string    `json:"source,omitempty"`    // who reported it, e.g. "kubelet"
	FieldPath string    `json:"fieldPath,omitempty"` // which part of the pod, e.g. "spec.containers{python-api}"
}

// CollectError records one read that failed.
type CollectError struct {
	Step    string `json:"step"`             // which part of collect, e.g. "logs"
	Target  string `json:"target"`           // what was being read
	Denied  bool   `json:"denied,omitempty"` // true when RBAC refused the read
	Message string `json:"message"`
}
