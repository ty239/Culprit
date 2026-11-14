// Package diagnose reads a snapshot and works out why the pod is failing.
// It starts at the visible symptom and follows the causedBy chain to the
// root cause. It never talks to the cluster: everything it says comes from
// the snapshot, and every finding points at the evidence behind it.
package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/culprit/culprit/internal/snapshot"
)

// Kinds of finding, from what you see first to where the chain ends.
const (
	Symptom   = "symptom"
	Cause     = "cause"
	RootCause = "rootCause"
)

// Diagnosis is the answer for one pod.
type Diagnosis struct {
	Namespace string `json:"namespace"`
	PodName   string `json:"podName"`

	// Status is the one word `kubectl get pod` would show, e.g.
	// "CrashLoopBackOff" or "Pending".
	Status string `json:"status"`

	// Healthy is true when the pod is running and ready (or finished
	// successfully) and nothing was found.
	Healthy bool `json:"healthy"`

	// Findings holds every chain, each in order: symptom first, then the
	// finding that caused it, down to the root cause.
	Findings []Finding `json:"findings"`

	// Evidence is what the findings point at.
	Evidence []Evidence `json:"evidence"`

	// RootCauses are the ids of the findings where a chain ends in a known
	// cause. Empty when the pod is healthy or the cause wasn't found.
	RootCauses []string `json:"rootCauses"`

	// Incomplete lists the reads collect couldn't make. The cause may be in
	// something that couldn't be read.
	Incomplete []string `json:"incomplete,omitempty"`
}

// Finding is one link in a chain.
type Finding struct {
	ID      string `json:"id"`      // "f-1"
	Kind    string `json:"kind"`    // Symptom, Cause or RootCause
	Object  string `json:"object"`  // e.g. "StorageClass fast-ssd"
	Summary string `json:"summary"` // one plain sentence

	// CausedBy is the id of the finding that caused this one. Empty on the
	// last finding of a chain.
	CausedBy string `json:"causedBy,omitempty"`

	// Evidence lists the ids of the records that back this finding up.
	Evidence []string `json:"evidence"`

	// Fix says what to change. Only set on root causes.
	Fix string `json:"fix,omitempty"`
}

// Evidence is one fact copied from the snapshot.
type Evidence struct {
	ID     string `json:"id"`     // "ev-1"
	Source string `json:"source"` // where in the snapshot, e.g. "pvcs[data].phase"
	Value  string `json:"value"`  // what was found there
}

// Diagnose works out why the pod in snap is failing.
func Diagnose(snap *snapshot.Snapshot) *Diagnosis {
	d := &Diagnosis{
		Namespace:  snap.Namespace,
		PodName:    snap.PodName,
		Findings:   []Finding{},
		Evidence:   []Evidence{},
		RootCauses: []string{},
	}
	for _, e := range snap.Errors {
		why := e.Message
		if e.Denied {
			why = "access denied"
		}
		d.Incomplete = append(d.Incomplete, fmt.Sprintf("%s: could not read %s (%s)", e.Step, e.Target, why))
	}
	if snap.Pod == nil {
		return d
	}
	d.Status = podStatus(snap.Pod)

	b := &builder{d: d, snap: snap, seen: map[string]string{}}
	b.scheduling()
	if !b.evicted() {
		b.containers()
		b.failedPod()
	}

	d.Healthy = len(d.Findings) == 0 && isHealthy(snap.Pod)
	if !d.Healthy && len(d.Findings) == 0 {
		// Not healthy, but no rule recognised why. Say what we can see.
		b.chain(link{
			object:   b.podObject(),
			summary:  fmt.Sprintf("Pod is %s, and Culprit has no rule that explains why", d.Status),
			evidence: []string{b.ev("pod.status.phase", string(snap.Pod.Status.Phase))},
		})
	}
	return d
}

// link is one finding while a chain is being built.
type link struct {
	object   string
	summary  string
	evidence []string
	fix      string
}

// builder collects findings and evidence for one diagnosis.
type builder struct {
	d    *Diagnosis
	snap *snapshot.Snapshot
	seen map[string]string // source+value -> evidence id, so nothing is listed twice
}

// ev records one fact from the snapshot and returns its id.
func (b *builder) ev(source, value string) string {
	key := source + "\x00" + value
	if id, ok := b.seen[key]; ok {
		return id
	}
	id := fmt.Sprintf("ev-%d", len(b.d.Evidence)+1)
	b.d.Evidence = append(b.d.Evidence, Evidence{ID: id, Source: source, Value: value})
	b.seen[key] = id
	return id
}

// chain adds a chain whose root cause wasn't found: the first link is the
// symptom and every later one is a cause.
func (b *builder) chain(links ...link) { b.add(false, links) }

// rootChain adds a chain that ends in a known root cause: the first link is
// the symptom and the last one is the root cause.
func (b *builder) rootChain(links ...link) { b.add(true, links) }

func (b *builder) add(rooted bool, links []link) {
	first := len(b.d.Findings)
	for i, l := range links {
		f := Finding{
			ID:       fmt.Sprintf("f-%d", first+i+1),
			Kind:     Cause,
			Object:   l.object,
			Summary:  l.summary,
			Evidence: l.evidence,
		}
		switch {
		case i == 0:
			f.Kind = Symptom
		case rooted && i == len(links)-1:
			f.Kind = RootCause
			f.Fix = l.fix
			b.d.RootCauses = append(b.d.RootCauses, f.ID)
		}
		if i < len(links)-1 {
			f.CausedBy = fmt.Sprintf("f-%d", first+i+2)
		}
		b.d.Findings = append(b.d.Findings, f)
	}
}

func (b *builder) podObject() string {
	return fmt.Sprintf("Pod %s/%s", b.snap.Namespace, b.snap.PodName)
}

// isHealthy reports whether the pod is running with every container ready,
// or has finished successfully. Past restarts don't count against it.
func isHealthy(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return true
	case corev1.PodRunning:
		if len(pod.Status.ContainerStatuses) == 0 {
			return false
		}
		for _, s := range pod.Status.ContainerStatuses {
			if !s.Ready {
				return false
			}
		}
		return true
	}
	return false
}

// podStatus is the one word `kubectl get pod` shows in its STATUS column.
func podStatus(pod *corev1.Pod) string {
	status := string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		status = pod.Status.Reason
	}
	for _, s := range pod.Status.InitContainerStatuses {
		switch {
		case s.State.Terminated != nil && s.State.Terminated.ExitCode == 0:
			continue
		case s.State.Terminated != nil && s.State.Terminated.Reason != "":
			return "Init:" + s.State.Terminated.Reason
		case s.State.Waiting != nil && s.State.Waiting.Reason != "" && s.State.Waiting.Reason != "PodInitializing":
			return "Init:" + s.State.Waiting.Reason
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		switch {
		case s.State.Waiting != nil && s.State.Waiting.Reason != "":
			return s.State.Waiting.Reason
		case s.State.Terminated != nil && s.State.Terminated.Reason != "":
			return s.State.Terminated.Reason
		}
	}
	return status
}
