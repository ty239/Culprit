package collect

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/culprit/culprit/internal/snapshot"
)

// collectEvents lists the events whose involvedObject is this pod, like
// `kubectl get events --field-selector involvedObject.name=POD`.
func (c *Collector) collectEvents(ctx context.Context, snap *snapshot.Snapshot) {
	pod := snap.Pod

	// Ask the API server for this pod's events only. Matching on kind and
	// uid too means we skip events about another object with the same name
	// (e.g. a Service called crash-api) or an older, deleted pod.
	selector := fields.Set{
		"involvedObject.kind": "Pod",
		"involvedObject.name": pod.Name,
		"involvedObject.uid":  string(pod.UID),
	}.AsSelector().String()

	list, err := c.Client.CoreV1().Events(snap.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		record(snap, "events", fmt.Sprintf("events?fieldSelector=%s", selector), err)
		return
	}

	for _, e := range list.Items {
		// Check again ourselves, in case the server ignored the selector.
		if !isAbout(e, pod) {
			continue
		}
		snap.Events = append(snap.Events, toEvent(e))
	}

	sort.SliceStable(snap.Events, func(i, j int) bool {
		return snap.Events[i].LastSeen.Before(snap.Events[j].LastSeen)
	})
}

func isAbout(e corev1.Event, pod *corev1.Pod) bool {
	o := e.InvolvedObject
	if o.Kind != "Pod" || o.Name != pod.Name || o.Namespace != pod.Namespace {
		return false
	}
	return o.UID == "" || pod.UID == "" || o.UID == pod.UID
}

// toEvent copies the useful fields. Kubernetes has two event formats: the
// old one fills Count/FirstTimestamp/LastTimestamp, the newer one fills
// EventTime/Series. We read whichever is set.
func toEvent(e corev1.Event) snapshot.Event {
	out := snapshot.Event{
		Type:      e.Type,
		Reason:    e.Reason,
		Message:   e.Message,
		Count:     e.Count,
		FirstSeen: firstTime(e.FirstTimestamp.Time, e.EventTime.Time),
		LastSeen:  firstTime(e.LastTimestamp.Time, e.EventTime.Time, e.FirstTimestamp.Time),
		Source:    firstString(e.Source.Component, e.ReportingController),
		FieldPath: e.InvolvedObject.FieldPath,
	}
	if e.Series != nil {
		out.Count = e.Series.Count
		if t := e.Series.LastObservedTime.Time; !t.IsZero() {
			out.LastSeen = t
		}
	}
	if out.Count == 0 {
		out.Count = 1
	}
	out.FirstSeen, out.LastSeen = out.FirstSeen.UTC(), out.LastSeen.UTC()
	return out
}

func firstTime(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func firstString(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
