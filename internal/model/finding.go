package model

// Confidence expresses how certain a Rule is about its Finding.
type Confidence string

const (
	High   Confidence = "High"
	Medium Confidence = "Medium"
	Low    Confidence = "Low"
)

// Finding is a single diagnostic conclusion produced by a Rule.
// CausedBy holds the ID of the upstream Finding that explains *why* this
// Finding exists, enabling the causal chain to be reconstructed.
type Finding struct {
	ID          string     `json:"id"`
	Rule        string     `json:"rule"`
	Confidence  Confidence `json:"confidence"`
	Message     string     `json:"message"`
	EvidenceIDs []string   `json:"evidenceIds,omitempty"`
	CausedBy    string     `json:"causedBy,omitempty"`
}
