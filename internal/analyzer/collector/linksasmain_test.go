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
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
)

// The decision of 2026-11-21 on #432 (Count and Decision, head 5caef316) narrows the crash loop link: a confirmed
// non-zero exit is a cause of BackOff events of its container and of nothing else. Property, one line (worded on
// 2026-11-21): every event that is not a BackOff links, folds and emits as main does with the same room in the
// hold table.
//
// TestNonBackOffLinksAsMain checks it over generated sequences. Each seed drives one pod through a random series of
// steps: its app container's status changes (running, a waiting cause, CrashLoopBackOff, an OOM kill, a non-zero
// exit or exit 0, the old state moving to the last state, so a sample during a hold can see the status move on to
// a newer exit), its status.reason set at times (Evicted, Terminated, DeadlineExceeded, Shutdown), BackOff and
// Unhealthy events (liveness, readiness, startup) at times around the terminations, samples of those events with
// their series growing, the clock moving, the pod going. Each sequence runs with one way of CauseSeen (none, yes,
// no, an error, a timeout, or a book of the causes the pod reconciler delivered) and one way of holding (on, off,
// or a table of one that overflows). Every reconcile of an event that is not a BackOff (its result, its error and
// each observation it emits, every field) and every observation of the pod reconciler is written as a line.
//
// The answers come from main: testdata/links-before-432.golden holds, per seed, the SHA-256 of those lines and
// their number, written by this same file with WRITE_LINKS_GOLDEN=1 on a checkout of origin/main at f593769a
// (before #432; this file and race_on_test.go, race_off_test.go copied in), so they come from the code before the
// change, not from the code under test. The one difference allowed is the class of a non-zero exit read from the
// status of a pod whose status.reason is empty, a cause since #432: that one class is mapped back. A seed that
// differs is shown in full; LINKS_DUMP=<dir> writes every seed's lines to compare with a run on main.
//
// The BackOff events (added on 2026-11-22, when the rule was narrowed: a BackOff links as main unless it is held
// with room in the table and its only candidates are non-zero exits). Each sequence writes the lines of its BackOff
// reconciles too (the result and each observation) and keeps those of the events the narrowed rule links as main:
// every one in a sequence with no hold table or a CauseSeen that never confirms (none, no), else the events whose
// pod, at every reconcile of the event, showed a cause as main reads it or no non-zero exit at all
// (linkSeq.classify, from the inputs alone, so main and the code under test keep the same events). Each sequence
// then runs a second time with the same draws and a table of one kept full by two holds of events it never names
// (fillHolds), so that every consequence, BackOff or not, overflows, and every line of that run is kept. The
// answers are in testdata/backoff-links-before-432.golden, per seed the SHA-256 and count of the kept lines of the
// first run, a separator line and all lines of the second, written with WRITE_BACKOFF_GOLDEN=1 the same way as
// the first golden: a throwaway detached worktree of f593769a (git worktree add --detach <dir> f593769a), this
// file, race_on_test.go, race_off_test.go and testdata/links-before-432.golden copied into its
// internal/analyzer/collector, and TestNonBackOffLinksAsMain run there with WRITE_BACKOFF_GOLDEN=1 (it checks the
// first golden on main as it writes the second). The first golden is not written again.
//
// The room each sequence draws: holds on (room for every event), holds off (no table), or a table of one. The table
// of one only runs with the ways of CauseSeen that never confirm an exit, so the room is the same as on main: a
// BackOff linked to a confirmed exit is emitted at once instead of held, which frees the one place, and an
// Unhealthy event that main emitted at once for want of room is then held, and dropped when its container recovers
// within the grace. That is the hold's designed result with more room, not another link, and
// TestTableOfOneConfirmedExitFreesRoom pins it.

// linksGolden is the file of per-seed digests of the lines the sequences gave on main.
const linksGolden = "testdata/links-before-432.golden"

// backOffGolden is the file of per-seed digests of the BackOff lines and the full-table lines main gave.
const backOffGolden = "testdata/backoff-links-before-432.golden"

// linkSeeds is how many sequences TestNonBackOffLinksAsMain runs: 500, or the first 150 of them under the race
// detector.
func linkSeeds() uint64 {
	if raceOn {
		return 150
	}
	return 500
}

// Ways of CauseSeen of a sequence.
const (
	seenNone    = "none"
	seenYes     = "yes"
	seenNo      = "no"
	seenError   = "error"
	seenTimeout = "timeout"
	seenBook    = "book"
)

// Ways of holding of a sequence.
const (
	holdsOn       = "on"
	holdsOff      = "off"
	holdsOverflow = "overflow"
	// holdsFull is a table kept full for the whole sequence (runLinkSeq with full).
	holdsFull = "full"
)

// linkWaitingReasons are waiting reasons of the app container: the causes of ADR-0039 point 3 and two that are not.
var linkWaitingReasons = []string{
	reasonImagePull, causePullErr, "InvalidImageName", reasonConfig, "CreateContainerError", linkCrashLoop,
	linkCreating,
}

// Strings of the sequences.
const (
	linkCrashLoop = "CrashLoopBackOff"
	linkCreating  = "ContainerCreating"
	linkCompleted = "Completed"
	linkPodGone   = "pod gone"
)

// linkCauseWaits are the waiting reasons of linkWaitingReasons that are a cause, as main lists them.
var linkCauseWaits = map[string]bool{
	reasonImagePull: true, causePullErr: true, "InvalidImageName": true, reasonConfig: true, "CreateContainerError": true,
}

// linkPodReasons are the values of status.reason a step may set; "" most often.
var linkPodReasons = []string{"", "", "", "", "Evicted", "Terminated", "DeadlineExceeded", "Shutdown"}

// Kinds of consequence event a step may create.
const (
	linkBackOff   = "BackOff"
	linkLiveness  = "liveness"
	linkReadiness = "readiness"
	linkStartup   = "startup"
)

// linkSeq is one sequence on a fresh harness.
type linkSeq struct {
	t      *testing.T
	h      *harness
	rng    *rand.Rand
	seen   string
	book   map[string]bool
	events map[string]string // name to kind
	names  []string
	lines  []string
	ids    int
	// podReason is the pod's status.reason as the last status step set it.
	podReason string
	// gone is set once the pod is deleted.
	gone   bool
	counts map[string]int
	// backOff holds the lines of each BackOff event; flagged and mainCause say what its pod showed at any of its
	// reconciles (linkSeq.classify); compareAll is set for a sequence whose BackOff events all link as main.
	backOff    map[string][]string
	flagged    map[string]bool
	mainCause  map[string]bool
	compareAll bool
}

// linkMainCause reports whether the app container's status shows a cause as main reads it for an event at its own
// time at (causeFingerprint on f593769a): a waiting cause reason, or, with hasAt, an OOM kill, current or last,
// that finished at most 10 minutes before at.
func linkMainCause(p *corev1.Pod, at time.Time, hasAt bool) bool {
	for _, st := range p.Status.ContainerStatuses {
		if st.Name != containerApp {
			continue
		}
		if w := st.State.Waiting; w != nil && linkCauseWaits[w.Reason] {
			return true
		}
		for _, t := range []*corev1.ContainerStateTerminated{st.State.Terminated, st.LastTerminationState.Terminated} {
			if hasAt && t != nil && t.Reason == ReasonOOMKilled && !t.FinishedAt.IsZero() &&
				!at.Before(t.FinishedAt.Time) && !at.After(t.FinishedAt.Add(10*time.Minute)) {
				return true
			}
		}
	}
	return false
}

// classify records, before a reconcile of the BackOff event name, what its pod shows, from the inputs only (the
// pod and the event as the sequence wrote them), so that main and the code under test pick the same events:
// mainCause when the status shows a cause as main reads it, flagged when it shows none and the app container has
// a non-zero exit, current or last, in any window (a superset of the exits a BackOff may be linked to).
func (s *linkSeq) classify(name string) {
	ev := &eventsv1.Event{}
	if err := s.h.cl.Get(s.t.Context(), types.NamespacedName{Namespace: testNS, Name: name}, ev); err != nil {
		s.t.Fatal(err)
	}
	if s.gone {
		return
	}
	p := readPod(s.t, s.h, webPod)
	at, ok := kubeletTime(ev)
	if linkMainCause(p, at, ok) {
		s.mainCause[name] = true
		return
	}
	for _, st := range p.Status.ContainerStatuses {
		for _, t := range []*corev1.ContainerStateTerminated{st.State.Terminated, st.LastTerminationState.Terminated} {
			if t != nil && t.ExitCode != 0 {
				s.flagged[name] = true
			}
		}
	}
}

// causeSeen answers by the sequence's way of CauseSeen, the same on main and here.
func (s *linkSeq) causeSeen(_ context.Context, o Observation) (bool, error) {
	switch s.seen {
	case seenYes:
		return true, nil
	case seenNo:
		return false, nil
	case seenError:
		return false, errors.New("synthetic: the Incident store is not loaded")
	case seenTimeout:
		return false, context.DeadlineExceeded
	}
	return s.book[o.CauseFingerprint], nil
}

// render is one line with every field of o, the class of a non-zero exit of a pod with no status.reason mapped back
// to none (the one difference allowed).
func (s *linkSeq) render(step string, o Observation) string {
	class := o.Class
	if o.Origin == fingerprint.SourcePodStatus && strings.HasPrefix(o.Occurrence, "termination:") &&
		o.Reason != ReasonOOMKilled && !linkCauseWaits[o.Reason] && class == ClassCause && s.podReason == "" {
		class = ClassNone
	}
	return fmt.Sprintf("%s: reason=%s origin=%s class=%q container=%q fingerprint=%s cause=%q subject=%s/%s/%s "+
		"source=%s type=%s reporter=%s occurrence=%s total=%d count=%d began=%s observed=%s mayRepeat=%t "+
		"foldOnly=%t immediate=%t symptom=%q",
		step, o.Reason, o.Origin, class, o.Container, o.Fingerprint, o.CauseFingerprint, o.Subject.Kind,
		o.Subject.Name, o.Subject.UID, o.Source, o.Type, o.ReportingController, o.Occurrence, o.Total, o.Count,
		o.Began.UTC().Format(time.RFC3339), o.ObservedAt.UTC().Format(time.RFC3339), o.MayRepeat, o.FoldOnly,
		o.Immediate, o.Symptom)
}

// state draws a container state at now.
func (s *linkSeq) state(now time.Time, terminatedOnly bool) corev1.ContainerState {
	r := s.rng.IntN(10)
	if terminatedOnly {
		r = 4 + s.rng.IntN(6)
	}
	switch {
	case r == 0:
		return running(now.Add(-time.Duration(s.rng.IntN(8)) * time.Minute))
	case r <= 3:
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: linkWaitingReasons[s.rng.IntN(len(linkWaitingReasons))]}}
	}
	s.ids++
	finished := now.Add(-time.Duration(s.rng.IntN(23)) * 30 * time.Second)
	t := &corev1.ContainerStateTerminated{
		ContainerID: fmt.Sprintf("containerd://c-%d", s.ids),
		StartedAt:   metav1.NewTime(finished.Add(-time.Minute)), FinishedAt: metav1.NewTime(finished),
	}
	switch r {
	case 4, 5:
		t.Reason, t.ExitCode = ReasonOOMKilled, exitOOM
		s.counts["oom kill"]++
	case 6:
		t.Reason, t.ExitCode = linkCompleted, 0
	default:
		t.Reason, t.ExitCode = reasonError, []int32{1, 2, 137, 143}[s.rng.IntN(4)]
		s.counts["non-zero exit"]++
	}
	return corev1.ContainerState{Terminated: t}
}

// status changes the app container's status and reconciles the pod.
func (s *linkSeq) status(step string) {
	now := s.h.clock.Now()
	p := readPod(s.t, s.h, webPod)
	var st corev1.ContainerStatus
	if len(p.Status.ContainerStatuses) > 0 {
		st = p.Status.ContainerStatuses[0]
	}
	st.Name = containerApp
	prev := st.State
	st.State = s.state(now, false)
	switch {
	case prev.Terminated != nil && s.rng.IntN(4) != 0:
		// The status moves on: the old termination becomes the last state.
		st.LastTerminationState = prev
		if st.State.Terminated != nil && len(s.names) > 0 {
			s.counts["moved on during a hold"]++
		}
	case s.rng.IntN(3) == 0:
		st.LastTerminationState = s.state(now, true)
	}
	st.RestartCount += s.rng.Int32N(3)
	st.Ready = st.State.Running != nil && s.rng.IntN(2) == 0
	p.Status.ContainerStatuses = []corev1.ContainerStatus{st}
	s.podReason = linkPodReasons[s.rng.IntN(len(linkPodReasons))]
	if s.podReason != "" {
		s.counts["status.reason set"]++
	}
	p.Status.Reason = s.podReason
	p.Status.Phase = corev1.PodRunning
	if s.podReason != "" {
		p.Status.Phase = corev1.PodFailed
	}
	start := metav1.NewTime(now.Add(-time.Duration(s.rng.IntN(12)) * time.Minute))
	p.Status.StartTime = &start
	ready := corev1.ConditionFalse
	if st.Ready {
		ready = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready,
		LastTransitionTime: metav1.NewTime(now.Add(-time.Duration(s.rng.IntN(4)) * time.Minute))}}
	s.h.update(p)
	res, err := s.h.pods.Reconcile(s.t.Context(), reqFor(webPod))
	s.lines = append(s.lines, fmt.Sprintf("%s: pod res=%s err=%t", step, res.RequeueAfter, err != nil))
	s.collect(step, "", "")
}

// collect drains the observations, records the pod reconciler's ones in the book, and writes the lines of those
// not of a BackOff event, and those of the BackOff event name apart; kind is the kind of the event reconciled, ""
// for the pod.
func (s *linkSeq) collect(step, kind, name string) {
	for _, o := range s.h.drain() {
		if o.Origin == fingerprint.SourcePodStatus {
			// The book has about two in three of the delivered causes, chosen by the fingerprint, not by a draw:
			// the draws stay the same on main and here.
			if sum := sha256.Sum256([]byte(o.Fingerprint)); sum[0]%3 != 0 {
				s.book[o.Fingerprint] = true
				s.h.c.ReleaseCause(o.Fingerprint)
			}
		}
		if kind == linkBackOff {
			s.backOff[name] = append(s.backOff[name], s.render(step, o))
			continue
		}
		s.lines = append(s.lines, s.render(step, o))
	}
}

// event creates a consequence event of a drawn kind and reconciles it.
func (s *linkSeq) event(step string) {
	now := s.h.clock.Now()
	kind := []string{linkBackOff, linkBackOff, linkLiveness, linkReadiness, linkStartup}[s.rng.IntN(5)]
	name := fmt.Sprintf("ev-%d", len(s.names))
	reason := reasonBack
	if kind != linkBackOff {
		reason = ReasonUnhealthy
	}
	path := appPath
	if s.rng.IntN(10) == 0 {
		path = "spec.containers{" + containerSidecar + "}"
	}
	at := now.Add(time.Duration(s.rng.IntN(27)-22) * 30 * time.Second)
	ev := consequence(name, reason, path, at)
	switch kind {
	case linkReadiness:
		ev.Action, ev.Note = "", "Readiness probe failed: HTTP probe failed with statuscode: 503"
	case linkStartup:
		ev.Action, ev.Note = "", "Startup probe failed: HTTP probe failed with statuscode: 503"
	}
	if s.rng.IntN(10) == 0 {
		ev.EventTime = metav1.MicroTime{}
	}
	ev.CreationTimestamp = metav1.NewTime(now)
	ev.Series = nil
	s.h.create(ev)
	s.names = append(s.names, name)
	s.events[name] = kind
	s.counts[kind]++
	s.reconcile(step, name)
}

// sample reconciles an event again, its series grown at times.
func (s *linkSeq) sample(step string) {
	name := s.names[s.rng.IntN(len(s.names))]
	if s.rng.IntN(2) == 0 {
		ev := &eventsv1.Event{}
		if err := s.h.cl.Get(s.t.Context(), types.NamespacedName{Namespace: testNS, Name: name}, ev); err != nil {
			s.t.Fatal(err)
		}
		n := int32(1)
		if ev.Series != nil {
			n = ev.Series.Count
		}
		ev.Series = &eventsv1.EventSeries{Count: n + 1, LastObservedTime: metav1.NewMicroTime(s.h.clock.Now())}
		s.h.update(ev)
	}
	s.reconcile(step, name)
}

// reconcile reconciles the event name and writes its result, unless it is a BackOff.
func (s *linkSeq) reconcile(step, name string) {
	kind := s.events[name]
	if kind == linkBackOff {
		s.classify(name)
	}
	res, err := s.h.events.Reconcile(s.t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: testNS, Name: name}})
	line := fmt.Sprintf("%s: %s %s res=%s err=%t", step, name, kind, res.RequeueAfter, err != nil)
	if kind != linkBackOff {
		s.lines = append(s.lines, line)
	} else {
		s.backOff[name] = append(s.backOff[name], line)
	}
	s.collect(step+" "+name, kind, name)
}

// runLinkSeq runs the sequence of seed and returns its lines, and the lines of the BackOff events that link as main
// (linkSeq.classify). full runs the same draws with a table of one that two holds of events the sequence never
// names keep full, one in each share, so that every consequence overflows.
func runLinkSeq(t *testing.T, seed uint64, counts map[string]int, full bool) ([]string, []string) {
	rng := rand.New(rand.NewPCG(seed, 432)) // #nosec G404 -- a seeded corpus, not a secret
	s := &linkSeq{t: t, rng: rng, book: map[string]bool{}, events: map[string]string{}, counts: counts,
		backOff: map[string][]string{}, flagged: map[string]bool{}, mainCause: map[string]bool{}}
	s.seen = []string{seenNone, seenYes, seenNo, seenError, seenTimeout, seenBook}[rng.IntN(6)]
	holding := []string{holdsOn, holdsOn, holdsOff, holdsOverflow}[rng.IntN(4)]
	if holding == holdsOverflow && (s.seen == seenYes || s.seen == seenBook) {
		holding = holdsOn
	}
	if full {
		holding = holdsFull
	}
	cfg := Config{}
	if s.seen != seenNone {
		cfg.CauseSeen = s.causeSeen
	}
	if holding == holdsOverflow || holding == holdsFull {
		cfg.MaxHeldConsequences = 1
	}
	counts["CauseSeen "+s.seen]++
	counts["holds "+holding]++
	// Every BackOff links as main: no table, a full one, or a CauseSeen that never confirms (an unconfirmed exit
	// leaves the hold as main has it).
	s.compareAll = holding == holdsOff || holding == holdsFull || s.seen == seenNone || s.seen == seenNo
	d, rs, _ := workload()
	pod := specPod()
	s.h = newHarness(t, cfg, d, rs, pod)
	s.h.c.holdOff = holding == holdsOff
	if full {
		fillHolds(t, s.h)
	}
	s.h.clock.set(t0)
	s.status("0")
	steps := 8 + rng.IntN(8)
	for i := 1; i <= steps; i++ {
		step := strconv.Itoa(i)
		switch r := rng.IntN(10); {
		case r < 3:
			if !s.gone {
				s.status(step)
			}
		case r < 6:
			if !s.gone {
				s.event(step)
			}
		case r < 8:
			if len(s.names) > 0 {
				s.sample(step)
			}
		case r < 9:
			s.h.clock.add(time.Duration(10+rng.IntN(170)) * time.Second)
		default:
			if !s.gone && i > steps/2 && rng.IntN(3) == 0 {
				if err := s.h.cl.Delete(t.Context(), readPod(t, s.h, webPod)); err != nil {
					t.Fatal(err)
				}
				s.gone = true
				counts[linkPodGone]++
			}
		}
	}
	// The end of every hold, then a second look.
	for _, after := range []time.Duration{ConsequenceGrace + time.Second, retryAfter} {
		s.h.clock.add(after)
		for _, name := range s.names {
			s.reconcile("end", name)
		}
	}
	return s.lines, s.backOffCompared(full)
}

// fillHolds fills a table of one: a hold in each share, of events the sequence never names, which no reconcile
// ends.
func fillHolds(t *testing.T, h *harness) {
	t.Helper()
	for i, cause := range []string{"", "blocker-cause"} {
		key := types.NamespacedName{Namespace: testNS, Name: fmt.Sprintf("blocker-%d", i)}
		if _, ok := h.c.holds.put(key, consequenceHold{event: types.UID(key.Name), pod: "blocker-pod",
			container: containerApp, reason: reasonBack, cause: cause, since: t0}); !ok {
			t.Fatal("no room for a blocker hold")
		}
	}
}

// backOffCompared returns the lines of the BackOff events that link as main, in the order the sequence created
// them: all of them in a sequence with compareAll, else those classify never flagged.
func (s *linkSeq) backOffCompared(full bool) []string {
	var out []string
	for _, name := range s.names {
		if s.events[name] != linkBackOff {
			continue
		}
		switch {
		case s.compareAll:
		case s.flagged[name]:
			s.counts["backoff exit not compared"]++
			continue
		case s.mainCause[name]:
			s.counts["backoff oom kill or wait, CauseSeen "+s.seen]++
		}
		if full {
			s.counts["backoff overflow, CauseSeen "+s.seen]++
		}
		out = append(out, s.backOff[name]...)
	}
	return out
}

// digest is the SHA-256 of lines, one per line.
func digest(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readLinksGolden returns the digest and line count main gave per seed in the golden file name.
func readLinksGolden(t *testing.T, name string) map[uint64]string {
	t.Helper()
	f, err := os.Open(name) // #nosec G304 -- one of the two golden files of this test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[uint64]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		seed, rest, ok := strings.Cut(sc.Text(), " ")
		if !ok || strings.HasPrefix(seed, "#") {
			continue
		}
		n, err := strconv.ParseUint(seed, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out[n] = rest
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNonBackOffLinksAsMain is the comparison above.
func TestNonBackOffLinksAsMain(t *testing.T) {
	write := os.Getenv("WRITE_LINKS_GOLDEN") != ""
	writeBackOff := os.Getenv("WRITE_BACKOFF_GOLDEN") != ""
	dump := os.Getenv("LINKS_DUMP")
	var want, wantBackOff map[uint64]string
	if !write {
		want = readLinksGolden(t, linksGolden)
	}
	if !writeBackOff {
		wantBackOff = readLinksGolden(t, backOffGolden)
	}
	n := linkSeeds()
	results := make([][]string, n)
	backOffs := make([][]string, n)
	seedCounts := make([]map[string]int, n)
	// The seeds are independent, each on its own harness: run in parallel, compared in order afterwards.
	t.Run("seeds", func(t *testing.T) {
		for seed := range n {
			t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
				t.Parallel()
				seedCounts[seed] = map[string]int{}
				var held []string
				results[seed], held = runLinkSeq(t, seed, seedCounts[seed], false)
				lines, fullBackOff := runLinkSeq(t, seed, seedCounts[seed], true)
				backOffs[seed] = slices.Concat(held, []string{"-- full table"}, lines, fullBackOff)
			})
		}
	})
	if t.Failed() {
		return
	}
	counts := map[string]int{}
	var out, outBackOff []string
	failed, failedBackOff, lines := 0, 0, 0
	for seed := range n {
		for k, v := range seedCounts[seed] {
			counts[k] += v
		}
		got := results[seed]
		lines += len(got)
		entry := fmt.Sprintf("%s %d", digest(got), len(got))
		out = append(out, fmt.Sprintf("%d %s", seed, entry))
		gotBackOff := backOffs[seed]
		lines += len(gotBackOff)
		entryBackOff := fmt.Sprintf("%s %d", digest(gotBackOff), len(gotBackOff))
		outBackOff = append(outBackOff, fmt.Sprintf("%d %s", seed, entryBackOff))
		if dump != "" {
			// #nosec G703 -- a directory the person running the test names, for a dump of its own lines
			if err := os.WriteFile(filepath.Join(dump, fmt.Sprintf("seed-%03d.txt", seed)),
				[]byte(strings.Join(got, "\n")+"\n-- backoff\n"+strings.Join(gotBackOff, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if !writeBackOff && wantBackOff[seed] != entryBackOff {
			failedBackOff++
			if failedBackOff <= 2 {
				t.Errorf("seed %d: the BackOff lines differ from main (%s, main %s):\n%s", seed, entryBackOff,
					wantBackOff[seed], strings.Join(gotBackOff, "\n"))
			}
		}
		if write || want[seed] == entry {
			continue
		}
		failed++
		if failed <= 2 {
			t.Errorf("seed %d differs from main (%s, main %s):\n%s", seed, entry, want[seed], strings.Join(got, "\n"))
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d seeds differ from main", failed, linkSeeds())
	}
	if failedBackOff > 0 {
		t.Errorf("%d of %d seeds differ from main in their BackOff lines", failedBackOff, linkSeeds())
	}
	if write {
		text := "# seed sha256-of-lines line-count, written on origin/main before #432\n" + strings.Join(out, "\n") + "\n"
		if err := os.WriteFile(linksGolden, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if writeBackOff {
		text := "# seed sha256-of-lines line-count, BackOff lines and the full table, written on origin/main at " +
			"f593769a\n" + strings.Join(outBackOff, "\n") + "\n"
		if err := os.WriteFile(backOffGolden, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{
		linkLiveness, linkReadiness, linkStartup, linkBackOff, "oom kill", "non-zero exit", "status.reason set",
		"moved on during a hold", linkPodGone,
		"CauseSeen " + seenNone, "CauseSeen " + seenYes, "CauseSeen " + seenNo, "CauseSeen " + seenError,
		"CauseSeen " + seenTimeout, "CauseSeen " + seenBook, "holds " + holdsOn, "holds " + holdsOff,
		"holds " + holdsOverflow, "holds " + holdsFull, "backoff exit not compared",
		"backoff oom kill or wait, CauseSeen " + seenYes, "backoff oom kill or wait, CauseSeen " + seenBook,
		"backoff overflow, CauseSeen " + seenYes, "backoff overflow, CauseSeen " + seenBook,
		"backoff overflow, CauseSeen " + seenError,
	} {
		if counts[k] == 0 {
			t.Errorf("no generated case of %q: %v", k, counts)
		}
	}
	t.Logf("%d sequences, %d lines compared; cases: %v", linkSeeds(), lines, counts)
}
