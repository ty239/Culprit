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

	// Nodes holds the pod's node, or every node when the pod hasn't been
	// placed on one yet (a Pending pod: the nodes explain why).
	Nodes []Node `json:"nodes"`

	// Secrets and ConfigMaps the pod uses, sorted by name. Only names and
	// key names are kept, never values.
	Secrets    []Reference `json:"secrets"`
	ConfigMaps []Reference `json:"configMaps"`

	// ServiceAccount is the account the pod runs as.
	ServiceAccount *ServiceAccountRef `json:"serviceAccount,omitempty"`

	// Storage chain: the pod's PVCs → the PVs they're bound to → the
	// StorageClasses they ask for. Each sorted by name.
	PVCs           []PVC          `json:"pvcs"`
	PVs            []PV           `json:"pvs"`
	StorageClasses []StorageClass `json:"storageClasses"`

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

// Node is the API server's record of one machine that runs pods.
type Node struct {
	Name string `json:"name"`

	// Missing is true when the pod names this node but the node no longer
	// exists. That's evidence, not a collect failure.
	Missing bool `json:"missing,omitempty"`

	// Allocatable is what pods can actually use; Capacity is the machine's
	// total. e.g. {"cpu": "16", "memory": "10182740Ki", "pods": "110"}
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`
	Capacity    corev1.ResourceList `json:"capacity,omitempty"`

	Conditions    []NodeCondition   `json:"conditions,omitempty"` // Ready, MemoryPressure, DiskPressure, ...
	Unschedulable bool              `json:"unschedulable,omitempty"`
	Taints        []corev1.Taint    `json:"taints,omitempty"` // rules that keep pods off this node
	Labels        map[string]string `json:"labels,omitempty"` // what nodeSelector / affinity match on

	// Images are the image names already on the node, e.g.
	// "docker.io/library/python:3.12-slim".
	Images []string `json:"images,omitempty"`

	// ImagesMayBeIncomplete is true when the list hit the kubelet's limit
	// (50 by default), so an image missing from it may still be on the node.
	ImagesMayBeIncomplete bool `json:"imagesMayBeIncomplete,omitempty"`
}

// NodeCondition is one health check the kubelet reports for its node.
type NodeCondition struct {
	Type    string `json:"type"`   // e.g. "Ready", "MemoryPressure"
	Status  string `json:"status"` // "True", "False" or "Unknown"
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Lookup results for an object the pod refers to.
const (
	Found      = "found"      // it exists
	Missing    = "missing"    // the API server says it doesn't exist (evidence)
	Unreadable = "unreadable" // the read failed; see Snapshot.Errors
)

// Reference is a Secret or ConfigMap the pod uses.
type Reference struct {
	Name  string `json:"name"`
	State string `json:"state"` // Found, Missing or Unreadable

	// Keys are the key NAMES the object holds (only when Found). Values
	// are never collected.
	Keys []string `json:"keys,omitempty"`

	// Type is the Secret's type, e.g. "Opaque" or
	// "kubernetes.io/dockerconfigjson". Empty for ConfigMaps.
	Type string `json:"type,omitempty"`

	// UsedBy lists every place in the pod spec that refers to it.
	UsedBy []Usage `json:"usedBy"`
}

// Usage is one place in the pod spec that refers to a Secret or ConfigMap.
type Usage struct {
	Via       string   `json:"via"`                 // "env", "envFrom", "volume", "projected", "imagePullSecrets"
	Container string   `json:"container,omitempty"` // for env / envFrom
	Name      string   `json:"name,omitempty"`      // env var or volume name
	Keys      []string `json:"keys,omitempty"`      // the keys it asks for, if specific
	Optional  bool     `json:"optional,omitempty"`  // the pod still starts without it
}

// ServiceAccountRef is the service account the pod runs as.
type ServiceAccountRef struct {
	Name  string `json:"name"`
	State string `json:"state"` // Found, Missing or Unreadable
}

// PVC is a PersistentVolumeClaim: the pod's request for a disk.
type PVC struct {
	Name   string   `json:"name"`
	State  string   `json:"state"`  // Found, Missing or Unreadable
	UsedBy []string `json:"usedBy"` // the pod volume names that mount it

	Phase string `json:"phase,omitempty"` // Pending, Bound or Lost

	// StorageClassName is the class it asks for. When the PVC doesn't name
	// one, UsesDefaultClass is true and the cluster's default class applies.
	StorageClassName string `json:"storageClassName,omitempty"`
	UsesDefaultClass bool   `json:"usesDefaultClass,omitempty"`

	VolumeName  string      `json:"volumeName,omitempty"` // the PV it's bound to
	Requested   string      `json:"requested,omitempty"`  // e.g. "1Gi"
	Capacity    string      `json:"capacity,omitempty"`   // what it actually got
	AccessModes []string    `json:"accessModes,omitempty"`
	Conditions  []Condition `json:"conditions,omitempty"`
}

// PV is a PersistentVolume: an actual disk.
type PV struct {
	Name             string   `json:"name"`
	State            string   `json:"state"`
	Phase            string   `json:"phase,omitempty"` // Available, Bound, Released, Failed
	Reason           string   `json:"reason,omitempty"`
	Message          string   `json:"message,omitempty"`
	StorageClassName string   `json:"storageClassName,omitempty"`
	Capacity         string   `json:"capacity,omitempty"`
	AccessModes      []string `json:"accessModes,omitempty"`
	ReclaimPolicy    string   `json:"reclaimPolicy,omitempty"`
	ClaimRef         string   `json:"claimRef,omitempty"` // "namespace/name" of the PVC holding it
}

// StorageClass describes how disks of one kind are created.
type StorageClass struct {
	Name              string `json:"name"`
	State             string `json:"state"`
	Provisioner       string `json:"provisioner,omitempty"`       // who creates the disks
	VolumeBindingMode string `json:"volumeBindingMode,omitempty"` // Immediate or WaitForFirstConsumer
	ReclaimPolicy     string `json:"reclaimPolicy,omitempty"`
	IsDefault         bool   `json:"isDefault,omitempty"`
}

// Condition is a generic status condition.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// CollectError records one read that failed.
type CollectError struct {
	Step    string `json:"step"`             // which part of collect, e.g. "logs"
	Target  string `json:"target"`           // what was being read
	Denied  bool   `json:"denied,omitempty"` // true when RBAC refused the read
	Message string `json:"message"`
}
