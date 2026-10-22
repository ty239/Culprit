package model

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

// Snapshot holds every Kubernetes object collected for a single pod investigation.
type Snapshot struct {
	Pod            *corev1.Pod                              `json:"pod,omitempty"`
	Events         []corev1.Event                           `json:"events,omitempty"`
	Nodes          []corev1.Node                            `json:"nodes,omitempty"`
	PVCs           map[string]*corev1.PersistentVolumeClaim `json:"pvcs,omitempty"`
	PVs            map[string]*corev1.PersistentVolume      `json:"pvs,omitempty"`
	StorageClasses map[string]*storagev1.StorageClass       `json:"storageClasses,omitempty"`
	ConfigMaps     map[string]*corev1.ConfigMap             `json:"configMaps,omitempty"`
	Secrets        map[string]*corev1.Secret                `json:"secrets,omitempty"`
	ServiceAccount *corev1.ServiceAccount                   `json:"serviceAccount,omitempty"`
	ContainerLogs  map[string]string                        `json:"containerLogs,omitempty"`
}

// snapshotAlias prevents infinite recursion in MarshalJSON.
type snapshotAlias Snapshot

// MarshalJSON omits nil/empty map and slice fields from the output.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	cp := snapshotAlias(s)
	if len(cp.PVCs) == 0 {
		cp.PVCs = nil
	}
	if len(cp.PVs) == 0 {
		cp.PVs = nil
	}
	if len(cp.StorageClasses) == 0 {
		cp.StorageClasses = nil
	}
	if len(cp.ConfigMaps) == 0 {
		cp.ConfigMaps = nil
	}
	if len(cp.Secrets) == 0 {
		cp.Secrets = nil
	}
	if len(cp.ContainerLogs) == 0 {
		cp.ContainerLogs = nil
	}
	if len(cp.Events) == 0 {
		cp.Events = nil
	}
	if len(cp.Nodes) == 0 {
		cp.Nodes = nil
	}
	return json.Marshal(cp)
}
