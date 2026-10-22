package model

import "fmt"

// Ref is a lightweight pointer to a Kubernetes object.
type Ref struct {
	Kind      string
	Namespace string
	Name      string
}

// String returns a human-readable representation of the Ref.
func (r Ref) String() string {
	if r.Namespace == "" {
		return fmt.Sprintf("%s/%s", r.Kind, r.Name)
	}
	return fmt.Sprintf("%s/%s/%s", r.Kind, r.Namespace, r.Name)
}