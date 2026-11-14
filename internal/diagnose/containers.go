package diagnose

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/culprit/culprit/internal/snapshot"
)

// A container that is Running at the moment of the snapshot still counts as
// crash-looping when it has restarted at least this often and last died
// this recently. (A crash-looping pod shows Running for a moment on every
// restart.)
const (
	loopRestarts = 3
	loopWindow   = 10 * time.Minute
)

// container is one container of the pod: its spec, its status, and where
// that status sits in the snapshot.
type container struct {
	spec   corev1.Container
	status corev1.ContainerStatus
	path   string // e.g. "pod.status.containerStatuses[python-api]"
	spath  string // e.g. "pod.spec.containers[python-api]"
	init   bool
}

// containers runs the container rules, init containers first.
func (b *builder) containers() {
	pod := b.snap.Pod
	if pod.Status.Phase == corev1.PodSucceeded {
		return
	}

	before := len(b.d.Findings)
	for _, c := range pair(pod.Spec.InitContainers, pod.Status.InitContainerStatuses, "initContainer", true) {
		b.container(c)
	}
	if len(b.d.Findings) > before {
		// The normal containers are only waiting for the init container.
		return
	}
	for _, c := range pair(pod.Spec.Containers, pod.Status.ContainerStatuses, "container", false) {
		b.container(c)
	}
}

func pair(specs []corev1.Container, statuses []corev1.ContainerStatus, word string, init bool) []container {
	out := []container{}
	for _, st := range statuses {
		for _, spec := range specs {
			if spec.Name == st.Name {
				out = append(out, container{
					spec:   spec,
					status: st,
					path:   fmt.Sprintf("pod.status.%sStatuses[%s]", word, st.Name),
					spath:  fmt.Sprintf("pod.spec.%ss[%s]", word, st.Name),
					init:   init,
				})
			}
		}
	}
	return out
}

// container picks the rule that matches the container's state.
func (b *builder) container(c container) {
	st := c.status
	w, t, last := st.State.Waiting, st.State.Terminated, st.LastTerminationState.Terminated

	switch {
	case w != nil && isImagePull(w.Reason):
		b.imagePull(c)

	case w != nil && w.Reason == "CreateContainerConfigError":
		b.config(c, false)

	case w != nil && w.Reason == "ContainerCreating":
		b.config(c, true)

	case w != nil && (w.Reason == "RunContainerError" || w.Reason == "CreateContainerError"):
		b.cannotStart(c)

	case w != nil && w.Reason == "CrashLoopBackOff":
		b.crash(c, last, false)

	case t != nil && t.ExitCode != 0:
		b.crash(c, t, false)

	case st.State.Running != nil && last != nil && last.ExitCode != 0 &&
		st.RestartCount >= loopRestarts && b.snap.CollectedAt.Sub(last.FinishedAt.Time) < loopWindow:
		// Caught between two crashes.
		b.crash(c, last, true)

	case st.State.Running != nil && !st.Ready && !c.init:
		evidence := []string{b.ev(c.path+".ready", "false")}
		if e := b.lastEvent("Unhealthy", ""); e != nil {
			evidence = append(evidence, b.ev("events[Unhealthy]", e.Message))
		}
		b.chain(link{
			object:   b.podObject(),
			summary:  fmt.Sprintf("Container %q is running but not ready", st.Name),
			evidence: evidence,
		})
	}
}

func isImagePull(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
		return true
	}
	return false
}

// imagePull: the container's image can't be downloaded.
func (b *builder) imagePull(c container) {
	snap, w := b.snap, c.status.State.Waiting
	image := c.spec.Image

	symptom := link{
		object:   b.podObject(),
		summary:  fmt.Sprintf("Container %q can't start: its image can't be pulled (%s)", c.status.Name, w.Reason),
		evidence: []string{b.ev(c.path+".state.waiting", joinNonEmpty(": ", w.Reason, w.Message))},
	}
	object := "Image " + image

	if w.Reason == "InvalidImageName" {
		b.rootChain(symptom, link{
			object:   object,
			summary:  fmt.Sprintf("%q is not a valid image name", image),
			evidence: []string{b.ev(c.spath+".image", image)},
			fix:      "Fix the image name in the pod: registry/name:tag, lower case, no spaces.",
		})
		return
	}

	// The pod names a login for the registry that doesn't exist.
	for _, s := range snap.Secrets {
		if s.State != snapshot.Missing {
			continue
		}
		for _, u := range s.UsedBy {
			if u.Via != "imagePullSecrets" {
				continue
			}
			b.rootChain(symptom, link{
				object:  fmt.Sprintf("Secret %s/%s", snap.Namespace, s.Name),
				summary: fmt.Sprintf("imagePullSecret %q doesn't exist, so the pod can't log in to the registry for %q", s.Name, image),
				evidence: []string{
					b.ev(fmt.Sprintf("secrets[%s].state", s.Name), snapshot.Missing),
					b.ev(fmt.Sprintf("secrets[%s].usedBy", s.Name), "imagePullSecrets"),
				},
				fix: fmt.Sprintf("Create the registry Secret %q in namespace %q (`kubectl create secret docker-registry`).", s.Name, snap.Namespace),
			})
			return
		}
	}

	// Otherwise it's the image itself. Find the runtime's error: it is on
	// the pod while the state is ErrImagePull, and in the events otherwise.
	evidence := []string{b.ev(c.spath+".image", image)}
	pullErr := ""
	if w.Reason == "ErrImagePull" {
		pullErr = w.Message
	} else if e := b.lastEvent("Failed", "Failed to pull image"); e != nil {
		pullErr = e.Message
		evidence = append(evidence, b.ev("events[Failed]", e.Message))
	}

	onNode := true
	nodeName := snap.Pod.Spec.NodeName
	for _, n := range snap.Nodes {
		if n.Name != nodeName || n.Missing {
			continue
		}
		if onNode = hasImage(n.Images, image); !onNode {
			note := fmt.Sprintf("%q is not among the %d images on the node", image, len(n.Images))
			if n.ImagesMayBeIncomplete {
				note += " (the list may be incomplete)"
			}
			evidence = append(evidence, b.ev(fmt.Sprintf("nodes[%s].images", n.Name), note))
		}
	}

	summary := fmt.Sprintf("Image %q can't be pulled", image)
	if !onNode {
		summary = fmt.Sprintf("Image %q isn't on node %s and can't be pulled", image, nodeName)
	}
	if why := pullReason(pullErr); why != "" {
		summary += ": " + why
	}
	b.rootChain(symptom, link{
		object:   object,
		summary:  summary,
		evidence: evidence,
		fix: fmt.Sprintf("Check the image name and tag. If it was built on this machine, load it onto the node (kind: `kind load docker-image %s`). "+
			"If it's in a private registry, add an imagePullSecret.", image),
	})
}

// pullReason turns the runtime's pull error into plain words.
func pullReason(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case m == "":
		return ""
	case strings.Contains(m, "no such host"), strings.Contains(m, "timeout"),
		strings.Contains(m, "connection refused"), strings.Contains(m, "dial tcp"):
		return "the registry can't be reached"
	case strings.Contains(m, "pull access denied"), strings.Contains(m, "unauthorized"),
		strings.Contains(m, "authorization"), strings.Contains(m, "denied"):
		return "the registry says the repository doesn't exist or needs a login"
	case strings.Contains(m, "not found"), strings.Contains(m, "manifest unknown"):
		return "the registry has no such image or tag"
	}
	return ""
}

// hasImage reports whether the node's image list holds the pod's image.
// Nodes list full names ("docker.io/library/python:3.12-slim") where a pod
// may just say "python:3.12-slim".
func hasImage(nodeImages []string, image string) bool {
	if !strings.Contains(image, "@") {
		if i := strings.LastIndex(image, ":"); i < 0 || strings.Contains(image[i:], "/") {
			image += ":latest"
		}
	}
	names := map[string]bool{
		image:                        true,
		"docker.io/" + image:         true,
		"docker.io/library/" + image: true,
	}
	for _, img := range nodeImages {
		if names[img] {
			return true
		}
	}
	return false
}

// config: the container is blocked because a Secret or ConfigMap it needs
// is missing, or lacks a key. With volumesOnly (a container stuck in
// ContainerCreating) only mounted ones are checked, and nothing is reported
// unless one is found: ContainerCreating is also just a normal startup step.
func (b *builder) config(c container, volumesOnly bool) {
	snap, w := b.snap, c.status.State.Waiting

	symptom := link{
		object:   b.podObject(),
		summary:  fmt.Sprintf("Container %q is blocked before it starts (%s)", c.status.Name, w.Reason),
		evidence: []string{b.ev(c.path+".state.waiting", joinNonEmpty(": ", w.Reason, w.Message))},
	}
	if volumesOnly {
		if e := b.lastEvent("FailedMount", ""); e != nil {
			symptom.evidence = append(symptom.evidence, b.ev("events[FailedMount]", e.Message))
		}
	}

	type group struct {
		kind, src string
		refs      []snapshot.Reference
	}
	groups := []group{{"Secret", "secrets", snap.Secrets}, {"ConfigMap", "configMaps", snap.ConfigMaps}}

	// Two passes: first the objects the kubelet's message names, then the rest.
	for _, named := range []bool{true, false} {
		for _, g := range groups {
			for _, ref := range g.refs {
				if strings.Contains(w.Message, `"`+ref.Name+`"`) != named {
					continue
				}
				for _, u := range ref.UsedBy {
					isVolume := u.Via == "volume" || u.Via == "projected"
					if u.Optional || u.Via == "imagePullSecrets" || (volumesOnly && !isVolume) ||
						(u.Container != "" && u.Container != c.status.Name) {
						continue
					}
					src := fmt.Sprintf("%s[%s]", g.src, ref.Name)
					object := fmt.Sprintf("%s %s/%s", g.kind, snap.Namespace, ref.Name)
					use := usageText(u)

					if ref.State == snapshot.Missing {
						fix := fmt.Sprintf("Create %s %q in namespace %q", g.kind, ref.Name, snap.Namespace)
						if len(u.Keys) > 0 {
							fix += fmt.Sprintf(" with key %s", quoted(u.Keys))
						}
						b.rootChain(symptom, link{
							object:  object,
							summary: fmt.Sprintf("%s %q doesn't exist, and the pod needs it (%s)", g.kind, ref.Name, use),
							evidence: []string{
								b.ev(src+".state", snapshot.Missing),
								b.ev(src+".usedBy", use),
							},
							fix: fix + ", or point the pod at one that exists.",
						})
						return
					}
					if ref.State != snapshot.Found {
						continue
					}
					for _, k := range u.Keys {
						if slices.Contains(ref.Keys, k) {
							continue
						}
						b.rootChain(symptom, link{
							object:  object,
							summary: fmt.Sprintf("%s %q exists but has no key %q, which the pod asks for (%s)", g.kind, ref.Name, k, use),
							evidence: []string{
								b.ev(src+".keys", orNone(strings.Join(ref.Keys, ", "))),
								b.ev(src+".usedBy", use),
							},
							fix: fmt.Sprintf("Add key %q to %s %q, or fix the key name in the pod.", k, g.kind, ref.Name),
						})
						return
					}
				}
			}
		}
	}

	if !volumesOnly {
		b.chain(symptom) // the kubelet's message is all we have
	}
}

// usageText describes one place the pod uses a Secret or ConfigMap.
func usageText(u snapshot.Usage) string {
	var s string
	switch u.Via {
	case "env":
		s = fmt.Sprintf("env var %s in container %q", u.Name, u.Container)
	case "envFrom":
		s = fmt.Sprintf("envFrom in container %q", u.Container)
	default:
		s = fmt.Sprintf("volume %q", u.Name)
	}
	if len(u.Keys) > 0 {
		s += ", key " + quoted(u.Keys)
	}
	return s
}

// cannotStart: the runtime refused to start the container at all.
func (b *builder) cannotStart(c container) {
	w := c.status.State.Waiting
	symptom := link{
		object:   b.podObject(),
		summary:  fmt.Sprintf("Container %q can't be started (%s)", c.status.Name, w.Reason),
		evidence: []string{b.ev(c.path+".state.waiting", joinNonEmpty(": ", w.Reason, w.Message))},
	}
	if w.Message == "" {
		b.chain(symptom)
		return
	}
	b.rootChain(symptom, link{
		object:   fmt.Sprintf("Container %s (image %s)", c.status.Name, c.spec.Image),
		summary:  "The container runtime refuses to start it: " + oneLine(w.Message),
		evidence: symptom.evidence,
		fix:      "Check the container's command, args and mounts: the program it runs must exist in the image.",
	})
}

// crash: the container starts and then dies. term is how it last ended.
// running is true when the snapshot caught it between two crashes.
func (b *builder) crash(c container, term *corev1.ContainerStateTerminated, running bool) {
	st := c.status
	name := st.Name

	symptom := link{object: b.podObject()}
	switch {
	case st.RestartCount > 0:
		symptom.summary = fmt.Sprintf("Container %q keeps crashing: it has restarted %d times", name, st.RestartCount)
		symptom.evidence = append(symptom.evidence, b.ev(c.path+".restartCount", fmt.Sprint(st.RestartCount)))
	default:
		symptom.summary = fmt.Sprintf("Container %q crashed", name)
	}
	if w := st.State.Waiting; w != nil {
		symptom.evidence = append(symptom.evidence, b.ev(c.path+".state.waiting", joinNonEmpty(": ", w.Reason, w.Message)))
	}
	if term == nil {
		// Restarting, but the record of how it ended is gone.
		if len(symptom.evidence) == 0 {
			symptom.evidence = []string{b.ev(c.path+".restartCount", fmt.Sprint(st.RestartCount))}
		}
		b.chain(symptom)
		return
	}

	termSrc := c.path + ".lastState.terminated"
	if term == st.State.Terminated {
		termSrc = c.path + ".state.terminated"
	}
	termEv := b.ev(termSrc, fmt.Sprintf("reason %s, exit code %d", term.Reason, term.ExitCode))
	object := fmt.Sprintf("Container %s (image %s)", name, c.spec.Image)

	switch {
	case term.Reason == "OOMKilled":
		killed := link{
			object:   object,
			summary:  fmt.Sprintf("Container %q was killed for using too much memory (OOMKilled, exit code %d)", name, term.ExitCode),
			evidence: []string{termEv},
		}
		limit, ok := c.spec.Resources.Limits[corev1.ResourceMemory]
		if ok {
			b.rootChain(symptom, killed, link{
				object:   object,
				summary:  fmt.Sprintf("The app needs more memory than its limit of %s", limit.String()),
				evidence: []string{b.ev(c.spath+".resources.limits.memory", limit.String())},
				fix:      "Raise the container's memory limit, or make the app use less memory.",
			})
			return
		}
		b.rootChain(symptom, killed, link{
			object:   object,
			summary:  "The container has no memory limit, so it was killed when its node ran out of memory",
			evidence: []string{b.ev(c.spath+".resources.limits.memory", "(not set)")},
			fix:      "Give the container a memory request and limit that match what it uses, or free up memory on the node.",
		})

	case (term.Reason == "StartError" || term.Reason == "ContainerCannotRun") && term.Message != "":
		b.rootChain(symptom, link{
			object:   object,
			summary:  "The container can't be started: " + oneLine(term.Message),
			evidence: []string{termEv, b.ev(termSrc+".message", oneLine(term.Message))},
			fix:      "Check the container's command and args: the program it runs must exist in the image.",
		})

	default:
		exited := link{
			object:   object,
			summary:  fmt.Sprintf("Container %q exits with code %d%s", name, term.ExitCode, exitHint(term.ExitCode)),
			evidence: []string{termEv},
		}
		line, src := b.lastLogLine(name, running)
		if line == "" {
			logs := b.snap.Logs[name]
			exited.evidence = append(exited.evidence,
				b.ev(fmt.Sprintf("logs[%s].current", name), "no log: "+orNone(logs.Current.Unavailable)),
				b.ev(fmt.Sprintf("logs[%s].previous", name), "no log: "+orNone(logs.Previous.Unavailable)))
			b.chain(symptom, exited)
			return
		}
		b.rootChain(symptom, exited, link{
			object:   object,
			summary:  "The app fails with: " + line,
			evidence: []string{b.ev(src, line)},
			fix:      "Fix what the message says: the command, the app's code, or what is installed in the image.",
		})
	}
}

// exitHint explains the common exit codes.
func exitHint(code int32) string {
	switch code {
	case 1:
		return " (the app stopped with an error)"
	case 126:
		return " (the command can't be executed)"
	case 127:
		return " (the command was not found)"
	case 137:
		return " (it was force-killed, SIGKILL)"
	case 139:
		return " (segmentation fault)"
	case 143:
		return " (it was asked to stop, SIGTERM)"
	}
	return ""
}

// lastLogLine returns the last line the crashed container printed, and
// where it was found. After a crash the error can be in either log: the
// current one belongs to the newest container, which is the crashed one
// unless a fresh run has already started (running).
func (b *builder) lastLogLine(container string, running bool) (line, source string) {
	logs := b.snap.Logs[container]
	order := []struct {
		text, source string
	}{
		{logs.Current.Text, fmt.Sprintf("logs[%s].current", container)},
		{logs.Previous.Text, fmt.Sprintf("logs[%s].previous", container)},
	}
	if running {
		order[0], order[1] = order[1], order[0]
	}
	for _, o := range order {
		lines := strings.Split(strings.TrimSpace(o.text), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				return oneLine(l), o.source
			}
		}
	}
	return "", ""
}

// failedPod covers a pod the kubelet gave up on, e.g. an evicted one.
func (b *builder) failedPod() {
	st := b.snap.Pod.Status
	if st.Phase != corev1.PodFailed || len(b.d.Findings) > 0 {
		return
	}
	b.chain(link{
		object:   b.podObject(),
		summary:  "Pod has failed: " + orNone(joinNonEmpty(": ", st.Reason, st.Message)),
		evidence: []string{b.ev("pod.status", joinNonEmpty(", ", "phase Failed", st.Reason, st.Message))},
	})
}

// lastEvent returns the newest event with this reason whose message starts
// with prefix, or nil. Events expire, so it is only ever extra evidence.
func (b *builder) lastEvent(reason, prefix string) *snapshot.Event {
	for i := len(b.snap.Events) - 1; i >= 0; i-- {
		if e := &b.snap.Events[i]; e.Reason == reason && strings.HasPrefix(e.Message, prefix) {
			return e
		}
	}
	return nil
}

// oneLine squeezes text onto one line and keeps it readable.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "..."
	}
	return s
}

func quoted(keys []string) string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("%q", k)
	}
	return strings.Join(out, ", ")
}
