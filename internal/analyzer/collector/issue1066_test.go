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
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shadewright-labs/shadewright/internal/incidents/causereason"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
)

// The tests of #1066 (ADR-0039 point 12, amended 2026-11-21): one container that cannot pull its image opens one
// Incident, and no kubelet Failed event that is not about that pull is hidden by it.

const (
	pullReasonFailed = "Failed"
	pullImageDigest  = "ghcr.io/shadewright-labs/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pullErrDigest    = `Failed to pull image "` + pullImageDigest + `": ` +
		`failed to pull and unpack image "ghcr.io/shadewright-labs/web@sha256:` +
		`0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": not found`
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

// TestImagePullFailedMessageCorpus checks the pull-failure matcher through the event collector with a generated
// corpus whose expected classification is known by construction: only the kubelet's four fixed pull forms on a
// named container (Failed to pull image "<image>": <err>, Error: ErrImagePull, Error: ImagePullBackOff,
// Error: InvalidImageName) are ClassConsequence; every other Failed note, every mutation of a valid form, every
// other controller, reason or missing container stays ClassNone.
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

	// Generated valid Failed to pull image "<image>": <err> messages.
	images := []string{
		"nginx",
		"nginx:latest",
		"nginx:1.25.3-alpine",
		"registry.k8s.io/pause:3.9",
		"localhost:5000/my_org/sub-app:v1.0.0_rc1",
		"ghcr.io/shadewright-labs/shadewright-analyzer@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"my-acr.azurecr.io/team/svc:v2.4.1@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		"[::1]:5000/app:v1",
		"a",
		strings.Repeat("a", 63) + ".example.com:5000/" + strings.Repeat("b", 120) + ":" + strings.Repeat("c", 64),
	}
	errors := []string{
		"not found",
		`rpc error: code = NotFound desc = failed to pull and unpack image "nginx:bad": manifest unknown`,
		"pull access denied, repository does not exist or may require authorization",
		"context deadline exceeded",
		"http: server gave HTTP response to HTTPS client\nretry later",
		"unauthorized: token=synthetic-secret-value-12345",
		"x",
	}
	rng := rand.New(rand.NewPCG(1066, 12)) // #nosec G404 -- seeded corpus, reproducible by design
	for i := range 200 {
		img := images[rng.IntN(len(images))]
		errMsg := errors[rng.IntN(len(errors))]
		add(fmt.Sprintf("gen-valid-%d", i), fmt.Sprintf("Failed to pull image %q: %s", img, errMsg), ClassConsequence)
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
		`Failed to apply default image tag "bad::image": couldn't parse image name`,
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

	// Hostile mutations of Failed to pull image "<image>": <err>: every mutation must be ClassNone.
	pullMutations := []string{
		`Failed to pull image "": not found`,
		`Failed to pull image "nginx:bad":`,
		`Failed to pull image "nginx:bad": `,
		`Failed to pull image "nginx:bad":    `,
		"Failed to pull image \"nginx:bad\": \t\n\r ",
		`Failed to pull image "nginx:bad"`,
		`Failed to pull image "nginx:bad" not found`,
		`Failed to pull image "nginx:bad":not found`,
		`Failed to pull image nginx:bad: not found`,
		`Failed to pull image 'nginx:bad': not found`,
		`Failed to pull image "nginx bad": not found`,
		"Failed to pull image \"nginx\tbad\": not found",
		"Failed to pull image \"nginx\nbad\": not found",
		"Failed to pull image \"nginx\rbad\": not found",
		"Failed to pull image \"nginx\x00bad\": not found",
		`Failed to pull image "nginx\"bad": not found`,
		`Failed to pull image "nginx\bad": not found`,
		`Failed to pull image "` + strings.Repeat("a", 1200) + `": not found`,
		`failed to pull image "nginx:bad": not found`,
		`FAILED TO PULL IMAGE "nginx:bad": not found`,
		` Failed to pull image "nginx:bad": not found`,
		`xFailed to pull image "nginx:bad": not found`,
		`Error: Failed to pull image "nginx:bad": not found`,
		`Failed to pull image "unterminated`,
		`Failed to pull image "unterminated: not found`,
		`Failed to pull image "nginx:bad": ` + strings.Repeat(" ", 4096),
	}
	for i, n := range pullMutations {
		add(fmt.Sprintf("mut-pull-%d", i), n, ClassNone)
	}

	// Non-kubelet controller, non-Failed reason, missing/unknown container: all ClassNone even with valid note.
	cases = append(cases,
		tc{name: "bad-controller", reason: pullReasonFailed, controller: "intruder-controller",
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrImagePull, want: ClassNone},
		tc{name: "controller-kubelet2", reason: pullReasonFailed, controller: "kubelet2",
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrDigest, want: ClassNone},
		tc{name: "reason-pulling", reason: "Pulling", controller: reportingKubelet,
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrImagePull, want: ClassNone},
		tc{name: "reason-errimagepull", reason: causePullErr, controller: reportingKubelet,
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrImagePull, want: ClassNone},
		tc{name: "reason-failedmount", reason: "FailedMount", controller: reportingKubelet,
			fieldPath: "spec.containers{" + containerApp + "}", note: pullErrDigest, want: ClassNone},
		tc{name: "no-fieldpath", reason: pullReasonFailed, controller: reportingKubelet,
			fieldPath: "", note: pullErrImagePull, want: ClassNone},
		tc{name: "unknown-container", reason: pullReasonFailed, controller: reportingKubelet,
			fieldPath: "spec.containers{ghost}", note: pullErrImagePull, want: ClassNone},
	)

	pos, neg := 0, 0
	for i, c := range cases {
		// Give even malformed quoted notes their candidate image so a mismatch cannot
		// mask a regression in the grammar checks.
		image := pullImageDigest
		if _, rest, ok := strings.Cut(c.note, `image "`); ok {
			image, _, _ = strings.Cut(rest, `"`)
		}
		h, _ := causeHarness(t, func(p *corev1.Pod) {
			for _, spec := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
				spec[0].Image = image
			}
			wait := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: causePullErr}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp, Image: image, State: wait}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: containerInit, Image: image, State: wait}}
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

// TestIssue1066ImagePullLinksToOneIncident reproduces the rc.3 bug: one pod whose image cannot be pulled emits
// the waiting cause (ImagePullBackOff, ErrImagePull or InvalidImageName) plus kubelet Failed events ("Failed to
// pull image", "Error: ErrImagePull", "Error: ImagePullBackOff" or "Error: InvalidImageName") and BackOff. All
// of them must land on the single pull cause Incident, for both regular and init containers.
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
			notes: []string{pullErrDigest, pullErrImagePull, pullErrBackOff},
		},
		{
			name: causePullErr, container: containerApp, wait: causePullErr,
			notes: []string{pullErrDigest, pullErrImagePull},
		},
		{
			name: invalidImage, container: containerApp, wait: invalidImage,
			notes: []string{pullErrInvalidImage},
		},
		{
			name: "init container " + reasonImagePull, container: containerInit, init: true, wait: reasonImagePull,
			notes: []string{pullErrDigest, pullErrImagePull, pullErrBackOff},
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
				ev := failedEvent("ev-pull", "spec.containers{"+containerApp+"}", pullErrDigest, t0)
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
		ev := failedEvent("ev-pull-app", "spec.containers{"+containerApp+"}", pullErrDigest, t0)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		got := h.one()
		if got.CauseFingerprint != "" {
			t.Errorf("app pull Failed linked to sidecar's cause %q, want none", got.CauseFingerprint)
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

		ev := failedEvent("ev-pull", "spec.containers{"+containerApp+"}", pullErrDigest, t0)
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

func TestFailedRequiresCurrentPullImage(t *testing.T) {
	for _, tc := range []struct{ name, quoted, status, wait string }{
		{"unrelated quote", "web:b", "web:a", causePullErr},
		{"stale status", "web:a", "web:b", causePullErr},
		{"no image in status", "web:a", "", causePullErr},
		{"configuration wait", "web:a", "web:a", reasonConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, rs, _ := workload()
			p := pullWaitingPod(containerApp, false, tc.wait)
			p.Spec.Containers[0].Image, p.Status.ContainerStatuses[0].Image = "web:a", tc.status
			book, see := seeingController()
			h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, p)
			h.reconcilePod(webPod)
			see(h.drain())
			ev := failedEvent("quoted", appPath, fmt.Sprintf("Failed to pull image %q: not found", tc.quoted), t0)
			h.create(ev)
			h.reconcileEvent(ev.Name)
			got := h.one()
			if got.Class != ClassNone || got.CauseFingerprint != "" {
				t.Fatalf("class %q, link %q", got.Class, got.CauseFingerprint)
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
	ev := failedEvent("series", appPath, pullErrDigest, t0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	h.none()

	// Another report arrives after the pod switches to another image. The first
	// occurrence is still evidence of the old pull, the second is unverified.
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
	got := h.drain()
	if len(got) != 2 || got[0].CauseFingerprint != cause.Fingerprint || got[0].Total != 1 || got[1].Class != ClassNone || got[1].Count != 1 || got[1].CauseFingerprint != "" {
		t.Fatalf("series across image change: %+v", got)
	}
}

func TestCauseFingerprintChangesWithImage(t *testing.T) {
	keys := map[string]bool{}
	for _, image := range []string{"web:v123", "web:v456"} {
		p := pullWaitingPod(containerApp, false, causePullErr)
		p.Spec.Containers[0].Image, p.Status.ContainerStatuses[0].Image = image, image
		d, rs, _ := workload()
		h := newHarness(t, Config{}, d, rs, p)
		h.reconcilePod(webPod)
		keys[h.one().Fingerprint] = true
	}
	if len(keys) != 2 {
		t.Fatal("two rollout images share a cause fingerprint")
	}
}
