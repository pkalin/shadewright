/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shadewright-labs/shadewright/internal/incidents/causereason"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
)

const (
	invalidPullImage    = "bad::image"
	pullReasonFailed    = "Failed"
	pullImageDigest     = "ghcr.io/shadewright-labs/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pullErrImagePull    = "Error: ErrImagePull"
	pullErrBackOff      = "Error: ImagePullBackOff"
	pullErrInvalidImage = "Error: InvalidImageName"
	pullBackOffNote     = `Back-off pulling image "ghcr.io/shadewright-labs/web@sha256:0123456789abcdef"`
	nonPullFailedNote   = `Error: secret "db-creds" not found`
	nonPullConfigNote   = "Error: CreateContainerConfigError"
	nonPullSandboxNote  = "Failed to create pod sandbox: rpc error: code = Unknown desc = network not ready"
	nonPullStartNote    = "Error: failed to create containerd task: exec: \"/bin/missing\": stat /bin/missing: no such file"
)

// failedEvent returns a kubelet Failed Warning event about the web pod's container named by fieldPath with note,
// observed at at.
func failedEvent(name, fieldPath, note string, at time.Time) *eventsv1.Event {
	ev := warning(name, 0)
	ev.Reason, ev.Action, ev.Note = pullReasonFailed, "", note
	ev.CreationTimestamp = metav1.NewTime(at)
	ev.EventTime = metav1.NewMicroTime(at)
	ev.Regarding.FieldPath = fieldPath
	return ev
}

// pullWaitingPod returns specPod with container (init or app) waiting with reason.
func pullWaitingPod(container string, init bool, reason string) *corev1.Pod {
	p := specPod()
	st := corev1.ContainerStatus{
		Name: container, Image: pullImageDigest,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
	}
	if init {
		p.Spec.InitContainers[0].Image = pullImageDigest
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{st}
	} else {
		p.Spec.Containers[0].Image = pullImageDigest
		p.Status.ContainerStatuses = []corev1.ContainerStatus{st}
	}
	return p
}

// TestImagePullFailedMessageCorpus checks exact notes, nearby notes and the event identity
// required to classify a kubelet Failed event as a pull consequence.
func TestImagePullFailedMessageCorpus(t *testing.T) {
	type tc struct {
		name       string
		reason     string
		controller string
		fieldPath  string
		note       string
		want       Class
	}
	var cases []tc
	add := func(name, note string, want Class) {
		cases = append(cases, tc{
			name: name, reason: pullReasonFailed, controller: reportingKubelet,
			fieldPath: "spec.containers{" + containerApp + "}", note: note, want: want,
		})
	}

	// Exact Error: forms.
	exactValid := []string{pullErrImagePull, pullErrBackOff, pullErrInvalidImage}
	for _, n := range exactValid {
		add("exact:"+n, n, ClassConsequence)
	}

	// Init container valid case.
	cases = append(cases, tc{
		name: "init-container", reason: pullReasonFailed, controller: reportingKubelet,
		fieldPath: "spec.initContainers{" + containerInit + "}", note: pullErrImagePull, want: ClassConsequence,
	})

	// Real kubelet Failed events that are NOT about an image pull: must stay ClassNone.
	nonPull := []string{
		"",
		nonPullFailedNote,
		nonPullConfigNote,
		nonPullSandboxNote,
		nonPullStartNote,
		"Error: CreateContainerError",
		"Error: RunContainerError",
		"Error: ImageInspectError",
		"Error: ErrImageNeverPull",
		"Error: RegistryUnavailable",
		"Error: CrashLoopBackOff",
		`Error: configmap "app-cfg" not found`,
		`Failed to inspect image "nginx:latest": rpc error: code = Unknown`,
		`Failed to start container app: containerd error`,
		`Failed to kill container app: timeout`,
		`Failed to apply default image tag invalidPullImage: couldn't parse image name`,
		`Failed to sync pod: context deadline exceeded`,
		`Pulling image "nginx:latest"`,
		pullBackOffNote,
	}
	for i, n := range nonPull {
		add(fmt.Sprintf("non-pull-%d", i), n, ClassNone)
	}

	// Hostile mutations of exact forms: every mutation must be ClassNone by construction.
	exactMutations := []func(string) string{
		func(s string) string { return " " + s },
		func(s string) string { return s + " " },
		func(s string) string { return s + "\n" },
		func(s string) string { return s + ": extra" },
		func(s string) string { return s + " extra" },
		func(s string) string { return "x" + s },
		strings.ToLower,
		strings.ToUpper,
		func(s string) string { return strings.Replace(s, ": ", ":", 1) },
		func(s string) string { return strings.Replace(s, "Error: ", "", 1) },
		func(s string) string { return s + "\nFailed to start container" },
	}
	for _, base := range exactValid {
		for i, m := range exactMutations {
			mut := m(base)
			if mut == base {
				t.Fatalf("mutation %d left %q unchanged", i, base)
			}
			add(fmt.Sprintf("mut-exact-%s-%d", base, i), mut, ClassNone)
		}
	}

	// Non-kubelet controller, non-Failed reason, missing/unknown container: all ClassNone even with valid note.
	cases = append(cases,
		tc{name: "bad-controller", reason: pullReasonFailed, controller: "intruder-controller",
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrImagePull, want: ClassNone},
		tc{name: "controller-kubelet2", reason: pullReasonFailed, controller: "kubelet2",
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrImagePull, want: ClassNone},
		tc{name: "reason-pulling", reason: "Pulling", controller: reportingKubelet,
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrImagePull, want: ClassNone},
		tc{name: "reason-errimagepull", reason: causePullErr, controller: reportingKubelet,
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrImagePull, want: ClassNone},
		tc{name: "reason-failedmount", reason: "FailedMount", controller: reportingKubelet,
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrImagePull, want: ClassNone},
		tc{name: "no-fieldpath", reason: pullReasonFailed, controller: reportingKubelet,
			fieldPath: "", note: pullErrImagePull, want: ClassNone},
		tc{name: "unknown-container", reason: pullReasonFailed, controller: reportingKubelet,
			fieldPath: "spec.containers{ghost}", note: pullErrImagePull, want: ClassNone},
	)

	pos, neg := 0, 0
	for i, c := range cases {
		h, _ := causeHarness(t, func(p *corev1.Pod) {
			for _, spec := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
				spec[0].Image = pullImageDigest
			}
			wait := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: causePullErr}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp, Image: pullImageDigest, State: wait}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: containerInit, Image: pullImageDigest, State: wait}}
		})
		ev := failedEvent(fmt.Sprintf("corpus-%d", i), c.fieldPath, c.note, t0)
		ev.Reason, ev.ReportingController = c.reason, c.controller
		h.create(ev)
		h.reconcileEvent(ev.Name)
		got := h.one()
		if got.Class != c.want || got.Origin != fingerprint.SourceEvent {
			t.Errorf("case %s (reason=%q controller=%q path=%q note=%.80q): class %q origin %q, want %q event",
				c.name, c.reason, c.controller, c.fieldPath, c.note, got.Class, got.Origin, c.want)
		}
		if c.want == ClassConsequence {
			pos++
		} else {
			neg++
		}
	}
	if pos == 0 || neg == 0 {
		t.Fatalf("corpus missing positives (%d) or negatives (%d)", pos, neg)
	}
}

// TestIssue1066ImagePullLinksToOneIncident checks that short Failed notes and BackOff
// events link to the pull cause for both app and init containers.
func TestIssue1066ImagePullLinksToOneIncident(t *testing.T) {
	invalidImage := causereason.ImagePull()[2]
	for _, tc := range []struct {
		name      string
		container string
		init      bool
		wait      string
		notes     []string
	}{
		{
			name: reasonImagePull, container: containerApp, wait: reasonImagePull,
			notes: []string{pullErrImagePull, pullErrBackOff},
		},
		{
			name: causePullErr, container: containerApp, wait: causePullErr,
			notes: []string{pullErrImagePull},
		},
		{
			name: invalidImage, container: containerApp, wait: invalidImage,
			notes: []string{pullErrInvalidImage},
		},
		{
			name: "init container " + reasonImagePull, container: containerInit, init: true, wait: reasonImagePull,
			notes: []string{pullErrImagePull, pullErrBackOff},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, rs, _ := workload()
			pod := pullWaitingPod(tc.container, tc.init, tc.wait)
			book, see := seeingController()
			h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod)

			h.reconcilePod(webPod)
			got := h.drain()
			if len(got) != 1 || got[0].Class != ClassCause || got[0].Reason != tc.wait {
				t.Fatalf("pod reconcile gave %+v, want one %s cause", got, tc.wait)
			}
			see(got)

			fieldPath := "spec.containers{" + tc.container + "}"
			if tc.init {
				fieldPath = "spec.initContainers{" + tc.container + "}"
			}
			for i, note := range tc.notes {
				ev := failedEvent(fmt.Sprintf("ev-failed-%d", i), fieldPath, note, t0.Add(time.Duration(i+1)*time.Second))
				h.create(ev)
				h.reconcileEvent(ev.Name)
				got = append(got, h.drain()...)
			}
			bo := consequence("ev-backoff", reasonBack, fieldPath, t0.Add(10*time.Second))
			bo.Note = pullBackOffNote
			h.create(bo)
			h.reconcileEvent(bo.Name)
			got = append(got, h.drain()...)

			wantObs := 1 + len(tc.notes) + 1
			if len(got) != wantObs {
				t.Fatalf("got %d observations, want %d: %+v", len(got), wantObs, got)
			}
			if keys := incidentKeys(got); len(keys) != 1 {
				t.Errorf("one failing image pull lands on %d Incidents, want 1: %v", len(keys), keys)
			}
		})
	}
}

// TestIssue1066PullFailedDoesNotLinkToNonPullCause checks that a kubelet image-pull Failed event links ONLY to
// an image-pull waiting cause of the same container, never to OOMKilled, a non-zero exit, CreateContainerConfigError,
// CreateContainerError, CrashLoopBackOff or another container's ImagePullBackOff; and that a non-pull Failed event
// never links to ImagePullBackOff.
func TestIssue1066PullFailedDoesNotLinkToNonPullCause(t *testing.T) {
	t.Run("non-pull causes on same container", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			setPod func(*corev1.Pod)
		}{
			{"OOMKilled", func(p *corev1.Pod) {
				withTermination(p, "containerd://oom-1", ReasonOOMKilled, exitOOM, t0.Add(-time.Minute))
			}},
			{"non-zero exit", func(p *corev1.Pod) {
				withTermination(p, "containerd://err-1", reasonError, 2, t0.Add(-time.Minute))
			}},
			{"CreateContainerConfigError", func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  containerApp,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonConfig}},
				}}
			}},
			{causereason.Waiting()[4], func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  containerApp,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: causereason.Waiting()[4]}},
				}}
			}},
			{linkCrashLoop, func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  containerApp,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: linkCrashLoop}},
				}}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h, _ := causeHarness(t, tc.setPod)
				ev := failedEvent("ev-pull", "spec.containers{"+containerApp+"}", pullErrImagePull, t0)
				h.create(ev)
				h.reconcileEvent(ev.Name)
				got := h.one()
				if got.Class != ClassNone {
					t.Fatalf("class %q, want none without a pull wait", got.Class)
				}
				if got.CauseFingerprint != "" {
					t.Errorf("pull Failed linked to %s cause %q, want none", tc.name, got.CauseFingerprint)
				}
			})
		}
	})

	t.Run("another container waiting in ImagePullBackOff", func(t *testing.T) {
		h, _ := causeHarness(t, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  containerSidecar,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonImagePull}},
			}}
		})
		ev := failedEvent("ev-pull-app", "spec.containers{"+containerApp+"}", pullErrImagePull, t0)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		got := h.one()
		if got.Class != ClassNone || got.CauseFingerprint != "" {
			t.Errorf("app pull Failed has class %q and sidecar cause %q, want neither", got.Class, got.CauseFingerprint)
		}
	})

	t.Run("non-pull Failed event on container waiting in ImagePullBackOff", func(t *testing.T) {
		h, _ := causeHarness(t, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  containerApp,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonImagePull}},
			}}
		})
		for i, note := range []string{nonPullFailedNote, nonPullConfigNote, nonPullSandboxNote, nonPullStartNote} {
			ev := failedEvent(fmt.Sprintf("ev-nonpull-%d", i), "spec.containers{"+containerApp+"}", note, t0)
			h.create(ev)
			h.reconcileEvent(ev.Name)
			got := h.one()
			if got.Class != ClassNone || got.CauseFingerprint != "" {
				t.Errorf("non-pull Failed %q: class %q cause %q, want none", note, got.Class, got.CauseFingerprint)
			}
		}
	})
}

// TestIssue1066PullFailedHold checks the collector hold for image-pull Failed events:
//   - held when the pull cause is not yet seen by the Incident controller, and released once ReleaseCause fires;
//   - dropped at the end of ConsequenceGrace when the image pulled and the container started running, even if it
//     later restarted twice in a crash loop (restarts prove the pull succeeded);
//   - emitted with the pull cause when the container is still waiting in an image-pull reason at the end of the hold.
func TestIssue1066PullFailedHold(t *testing.T) {
	t.Run("released when pull cause opens", func(t *testing.T) {
		d, rs, _ := workload()
		pod := pullWaitingPod(containerApp, false, reasonImagePull)
		book, see := seeingController()
		h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod)

		h.reconcilePod(webPod)
		cause := h.one()

		ev := failedEvent("ev-pull", "spec.containers{"+containerApp+"}", pullErrImagePull, t0)
		h.create(ev)
		res := h.reconcileEvent(ev.Name)
		if res.RequeueAfter != ConsequenceGrace {
			t.Fatalf("first sight requeue %v, want %v", res.RequeueAfter, ConsequenceGrace)
		}
		h.none()

		see([]Observation{cause})
		h.c.ReleaseCause(cause.Fingerprint)
		h.clock.add(30 * time.Second)
		h.reconcileEvent(ev.Name)
		got := h.one()
		if got.Class != ClassConsequence || got.CauseFingerprint != cause.Fingerprint {
			t.Errorf("released observation: class %q cause %q, want consequence / %q",
				got.Class, got.CauseFingerprint, cause.Fingerprint)
		}
	})

	t.Run("dropped when image pulls and container later restarts", func(t *testing.T) {
		before := holdOutcomeCounts()
		d, rs, _ := workload()
		pod := pullWaitingPod(containerApp, false, causePullErr)
		book, _ := seeingController()
		h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod)

		ev := failedEvent("ev-pull", "spec.containers{"+containerApp+"}", pullErrImagePull, t0)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		h.none()

		// Second sample still failing to pull.
		h.clock.add(20 * time.Second)
		h.reconcileEvent(ev.Name)
		h.none()

		// Image pulled and container started, then restarted twice in a crash loop: the pull succeeded.
		h.clock.add(20 * time.Second)
		cur := readPod(t, h, webPod)
		cur.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: containerApp, RestartCount: 2, Ready: false,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: linkCrashLoop}},
		}}
		h.update(cur)
		h.reconcileEvent(ev.Name)
		h.none()

		// End of hold: still in CrashLoopBackOff (not an image pull), must NOT open a pull Failed Incident.
		h.clock.set(t0.Add(ConsequenceGrace))
		h.reconcileEvent(ev.Name)
		h.none()
		wantOutcomes(t, before, map[string]float64{holdNotOpened: 1})
	})

	t.Run("emitted with cause when still failing at end of hold", func(t *testing.T) {
		d, rs, _ := workload()
		pod := pullWaitingPod(containerApp, false, reasonImagePull)
		book, _ := seeingController()
		h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod)

		h.reconcilePod(webPod)
		cause := h.one()

		ev := failedEvent("ev-pull", "spec.containers{"+containerApp+"}", pullErrBackOff, t0)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		h.none()

		h.clock.add(ConsequenceGrace)
		h.reconcileEvent(ev.Name)
		got := h.one()
		if got.Class != ClassConsequence || got.CauseFingerprint != cause.Fingerprint {
			t.Errorf("end-of-hold observation: class %q cause %q, want consequence / %q",
				got.Class, got.CauseFingerprint, cause.Fingerprint)
		}
	})
}

func TestDetailedPullFailuresStayIndependent(t *testing.T) {
	for _, tc := range []struct{ name, image string }{
		{"matching", pullImageDigest},
		{"different", "web:other"},
		{"unparsable", invalidPullImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := pullWaitingPod(containerApp, false, causePullErr)
			d, rs, _ := workload()
			book, see := seeingController()
			h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod)
			h.reconcilePod(webPod)
			cause := h.one()
			see([]Observation{cause})

			ev := failedEvent("detailed", appPath, fmt.Sprintf("Failed to pull image %q: not found", tc.image), t0)
			h.create(ev)
			h.reconcileEvent(ev.Name)
			got := h.one()
			if got.Class != ClassNone || got.CauseFingerprint != "" || got.Fingerprint == cause.Fingerprint {
				t.Fatalf("detailed pull failure was grouped with its cause: %+v", got)
			}
		})
	}
}

func TestExactPullFailedNotesRequireCurrentPullWait(t *testing.T) {
	invalidImage := causereason.ImagePull()[2]
	for _, note := range []string{pullErrImagePull, pullErrBackOff, pullErrInvalidImage} {
		t.Run(note, func(t *testing.T) {
			for _, wait := range append(causereason.ImagePull(), reasonConfig, "running") {
				t.Run(wait, func(t *testing.T) {
					pod := pullWaitingPod(containerApp, false, wait)
					status := &pod.Status.ContainerStatuses[0]
					// The status may still carry the previous image.
					status.Image = "web:old"
					if wait == invalidImage {
						pod.Spec.Containers[0].Image = invalidPullImage
					}
					if wait == "running" {
						status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
					}
					d, rs, _ := workload()
					book, see := seeingController()
					h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod)
					h.reconcilePod(webPod)
					causes := h.drain()
					see(causes)

					ev := failedEvent("exact", appPath, note, t0)
					h.create(ev)
					h.reconcileEvent(ev.Name)
					got := h.one()
					if causereason.IsImagePull(wait) {
						if len(causes) != 1 || got.Class != ClassConsequence || got.CauseFingerprint != causes[0].Fingerprint {
							t.Fatalf("pull wait did not link to its cause: %+v", got)
						}
					} else if got.Class != ClassNone || got.CauseFingerprint != "" {
						t.Fatalf("event linked without a pull wait: %+v", got)
					}
				})
			}
		})
	}
}

func TestHeldFailedSeriesRechecksImage(t *testing.T) {
	d, rs, _ := workload()
	p := pullWaitingPod(containerApp, false, causePullErr)
	book, see := seeingController()
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, p)
	h.reconcilePod(webPod)
	cause := h.one()
	ev := failedEvent("series", appPath, pullErrImagePull, t0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	h.none()

	// Another report arrives after the pod switches to another image. The first
	// occurrence belongs to the old pull and the second belongs to the new one.
	p = readPod(t, h, webPod)
	p.Spec.Containers[0].Image = "web:new"
	if err := h.cl.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	p.Status.ContainerStatuses[0].Image = "web:new"
	h.update(p)
	ev.Series = &eventsv1.EventSeries{Count: 2, LastObservedTime: metav1.NewMicroTime(t0.Add(time.Second))}
	h.update(ev)
	see([]Observation{cause})
	h.c.ReleaseCause(cause.Fingerprint)
	h.reconcileEvent(ev.Name)
	first := h.one()
	if first.CauseFingerprint != cause.Fingerprint || first.Total != 1 {
		t.Fatalf("old pull received the wrong prefix: %+v", first)
	}
	h.reconcilePod(webPod)
	next := h.one()
	if next.Fingerprint == cause.Fingerprint {
		t.Fatal("changed spec image kept the original pull cause")
	}
	see([]Observation{next})
	h.c.ReleaseCause(next.Fingerprint)
	h.reconcileEvent(ev.Name)
	second := h.one()
	if second.Class != ClassConsequence || second.CauseFingerprint != next.Fingerprint || second.Total != 2 || second.Count != 1 {
		t.Fatalf("new pull received the wrong increment: %+v", second)
	}
}

func TestCauseFingerprintChangesWithImage(t *testing.T) {
	keys := map[string]bool{}
	for _, image := range []string{"web", "web:latest", "docker.io/library/web:latest", "web:V1", "web:v1", invalidPullImage} {
		p := pullWaitingPod(containerApp, false, causePullErr)
		p.Spec.Containers[0].Image, p.Status.ContainerStatuses[0].Image = image, image
		d, rs, _ := workload()
		h := newHarness(t, Config{}, d, rs, p)
		h.reconcilePod(webPod)
		got := h.one()
		if keys[got.Fingerprint] {
			t.Errorf("spec image %q shares another image's cause fingerprint", image)
		}
		keys[got.Fingerprint] = true
	}
}

func TestPullCauseUsesSpecImage(t *testing.T) {
	for _, init := range []bool{false, true} {
		container, path := containerApp, appPath
		if init {
			container, path = containerInit, "spec.initContainers{"+containerInit+"}"
		}
		observe := func(spec, status string) Observation {
			p := pullWaitingPod(container, init, causePullErr)
			if init {
				p.Spec.InitContainers[0].Image, p.Status.InitContainerStatuses[0].Image = spec, status
			} else {
				p.Spec.Containers[0].Image, p.Status.ContainerStatuses[0].Image = spec, status
			}
			d, rs, _ := workload()
			h := newHarness(t, Config{}, d, rs, p)
			h.reconcilePod(webPod)
			cause := h.one()
			ev := failedEvent("backoff", path, pullBackOffNote, t0)
			ev.Reason = reasonBackOff
			h.create(ev)
			h.reconcileEvent(ev.Name)
			if got := h.one(); got.CauseFingerprint != cause.Fingerprint {
				t.Fatalf("BackOff on %s linked to %q, want %q", container, got.CauseFingerprint, cause.Fingerprint)
			}
			return cause
		}
		old := observe("web:a", "web:a")
		current := observe("web:b", "web:b")
		for _, status := range []string{"web:a", ""} {
			got := observe("web:b", status)
			if got.Fingerprint != current.Fingerprint || got.Occurrence != current.Occurrence ||
				got.Fingerprint == old.Fingerprint || got.Occurrence == old.Occurrence {
				t.Errorf("container %s, status image %q: spec image change did not identify the new pull", container, status)
			}
		}
	}
}

func TestRecoveredPullSeriesDropsOnlyVerifiedPrefix(t *testing.T) {
	before := holdOutcomeCounts()
	d, rs, _ := workload()
	p := pullWaitingPod(containerApp, false, causePullErr)
	book, _ := seeingController()
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, p)
	ev := failedEvent("series", appPath, pullErrImagePull, t0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	h.none()
	p = readPod(t, h, webPod)
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	h.update(p)
	ev.Series = &eventsv1.EventSeries{Count: 3, LastObservedTime: metav1.NewMicroTime(t0.Add(time.Second))}
	h.update(ev)
	h.reconcileEvent(ev.Name)
	got := h.drain()
	if len(got) != 1 || got[0].Class != ClassNone || got[0].Total != 3 || got[0].Count != 2 {
		t.Fatalf("after one failing sample and recovery: %+v, want only two unverified increments", got)
	}
	wantOutcomes(t, before, map[string]float64{holdNotOpened: 1})
}

func TestUnseenPullNeedsMatchingFinalSample(t *testing.T) {
	for _, removed := range []bool{false, true} {
		before := holdOutcomeCounts()
		d, rs, _ := workload()
		p := pullWaitingPod(containerApp, false, causePullErr)
		book, _ := seeingController()
		h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, p)
		ev := failedEvent("pull", appPath, pullErrImagePull, t0)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		h.reconcileEvent(ev.Name)
		h.none()
		if removed {
			if err := h.cl.Delete(t.Context(), p); err != nil {
				t.Fatal(err)
			}
		} else {
			ev.Type = corev1.EventTypeNormal
			h.update(ev)
		}
		h.reconcileEvent(ev.Name)
		h.none()
		wantOutcomes(t, before, map[string]float64{holdNotOpened: 1})
	}
}
