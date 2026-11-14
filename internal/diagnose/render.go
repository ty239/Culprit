package diagnose

import (
	"fmt"
	"io"
	"strings"
)

// Render writes the diagnosis as plain text: each chain from the symptom
// down to the root cause, with the evidence under every finding.
func Render(w io.Writer, d *Diagnosis) {
	fmt.Fprintf(w, "Pod %s/%s: %s\n", d.Namespace, d.PodName, d.Status)

	if d.Healthy {
		fmt.Fprintln(w, "\nHealthy: the pod is running and ready. Nothing to trace.")
	}

	evidence := map[string]Evidence{}
	for _, e := range d.Evidence {
		evidence[e.ID] = e
	}

	const indent = "            " // as wide as the label column
	for i, f := range d.Findings {
		if f.Kind == Symptom {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%-12s%s\n", label(f.Kind), f.Summary)
		fmt.Fprintf(w, "%s%s\n", indent, f.Object)
		for _, id := range f.Evidence {
			e := evidence[id]
			fmt.Fprintf(w, "%s%s  %s = %s\n", indent, e.ID, e.Source, e.Value)
		}
		if f.Fix != "" {
			fmt.Fprintf(w, "%sFix: %s\n", indent, f.Fix)
		}

		switch {
		case f.CausedBy != "":
			fmt.Fprintln(w, "  |\n  caused by\n  |")
		case f.Kind != RootCause && (i == len(d.Findings)-1 || d.Findings[i+1].Kind == Symptom):
			fmt.Fprintf(w, "\n%sRoot cause not found: the snapshot doesn't say what is behind this.\n", indent)
		}
	}

	if len(d.Incomplete) > 0 {
		fmt.Fprintln(w, "\nThe snapshot is incomplete, so the cause may be in something that couldn't be read:")
		for _, s := range d.Incomplete {
			fmt.Fprintf(w, "  - %s\n", s)
		}
	}
}

func label(kind string) string {
	switch kind {
	case Symptom:
		return "Symptom"
	case RootCause:
		return "Root cause"
	}
	return "Cause"
}

// String returns the rendered diagnosis.
func (d *Diagnosis) String() string {
	var sb strings.Builder
	Render(&sb, d)
	return sb.String()
}
