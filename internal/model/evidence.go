package model

import "encoding/json"

// Evidence is a single piece of raw data that supports one or more Findings.
// Kind describes the source type (e.g. "Event", "PodStatus", "ContainerLog",
// "PVCStatus", "StorageClass").
type Evidence struct {
	ID      string          `json:"id"`
	Ref     Ref             `json:"ref"`
	Kind    string          `json:"kind"`
	Summary string          `json:"summary"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}
