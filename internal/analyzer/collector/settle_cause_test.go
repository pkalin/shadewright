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
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/pkalin/shadewright/internal/incidents/causereason"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// The decision of 2026-11-21 on #432 (Count and Decision, head 30f136dd): one function removes a non-zero-exit
// cause (settleExitCause), and only after it asked Config.CauseSeen for that fingerprint in the same reconcile.
//
// Properties, one line each:
//   - S1: every event main links to an OOM kill or a wait links to the same cause here, while no non-zero exit is
//     confirmed (an unconfirmed exit falls back to the OOM kill in the window).
//   - S2: no non-zero-exit cause is removed without a CauseSeen ask for it in that reconcile, on any path: end of
//     the hold, a cause first seen in the expiry reconcile, overflow, hold off, pod gone, a delivery retry.
//   - S3: an ask that fails at the end does not wait again: the consequence goes on without the exit, with an
//     error log naming the fingerprint and the label cause_unanswered; an answer no (or no CauseSeen) logs at info
//     with the label cause_unconfirmed.
//   - S4: no code outside settleExitCause clears a cause (TestOnlySettleRemovesACause).

// mainCauseFingerprint is causeFingerprint as origin/main (f2d69cd7) has it, kept as the reference of S1: a
// current waiting reason of WaitingCauseReasons first, else the newest OOM kill of the state or the last state
// that finished at most KillCauseWindow before at, the state's on a tie.
func mainCauseFingerprint(c *Collector, pod *corev1.Pod, owner, container string, init bool, at time.Time,
	hasAt bool) string {
	statuses := pod.Status.ContainerStatuses
	if init {
		statuses = pod.Status.InitContainerStatuses
	}
	i := slices.IndexFunc(statuses, func(s corev1.ContainerStatus) bool { return s.Name == container })
	if i < 0 {
		return ""
	}
	st := statuses[i]
	if w := st.State.Waiting; w != nil && slices.Contains(causereason.Waiting(), w.Reason) {
		return fingerprint.Fingerprint(c.podStatusInput(pod, owner, w.Reason, waitingNote(container, w.Reason)))
	}
	if !hasAt {
		return ""
	}
	var kill *corev1.ContainerStateTerminated
	for _, t := range []*corev1.ContainerStateTerminated{st.State.Terminated, st.LastTerminationState.Terminated} {
		if t == nil || t.Reason != ReasonOOMKilled || t.FinishedAt.IsZero() {
			continue
		}
		finished := t.FinishedAt.Time
		if at.Before(finished) || at.After(finished.Add(KillCauseWindow)) {
			continue
		}
		if kill == nil || finished.After(kill.FinishedAt.Time) {
			kill = t
		}
	}
	if kill == nil {
		return ""
	}
	return fingerprint.Fingerprint(c.podStatusInput(pod, owner, ReasonOOMKilled,
		terminationNote(container, kill.ExitCode)))
}

// terminated is a terminated container state with reason and exit code, finished at finished.
func terminated(id, reason string, exit int32, finished time.Time) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{
		ContainerID: "containerd://" + id, Reason: reason, ExitCode: exit,
		StartedAt: metav1.NewTime(finished.Add(-time.Minute)), FinishedAt: metav1.NewTime(finished),
	}
}

// oomThenExitPod is the web pod whose app container was OOM killed at t0 (the last state) and then exited with
// code 1 at t0+3m (the state), its pod restarting it.
func oomThenExitPod() *corev1.Pod {
	p := specPod()
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerApp, RestartCount: 2,
		State:                corev1.ContainerState{Terminated: terminated("exit", reasonError, 1, t0.Add(3*time.Minute))},
		LastTerminationState: corev1.ContainerState{Terminated: terminated("oom-kill", ReasonOOMKilled, exitOOM, t0)},
	}}
	return p
}

// ownerOf returns the verified owner UID the collector reads for the web pod, which the cause fingerprints take.
func ownerOf(t *testing.T, h *harness) string {
	t.Helper()
	ev := consequence("owner-probe", reasonBack, appPath, t0)
	_, owner, _, _, err := h.c.owners.subjectAndPod(t.Context(), testNS, ev.Regarding)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

// consequenceCauses returns the cause fingerprints of the consequences in obs, one entry per consequence.
func consequenceCauses(obs []Observation) []string {
	var out []string
	for _, o := range obs {
		if o.Class == ClassConsequence {
			out = append(out, o.CauseFingerprint)
		}
	}
	return out
}

// TestOOMKillNotHiddenByUnconfirmedExit is finding 2 of the fifth pass on #432: an OOM kill at t0 in the last
// state, an exit 1 at t0+3m in the state, a BackOff at t0+1m, the exit not confirmed. On main the BackOff carries
// the OOM kill's fingerprint (the controller has its Incident, so at once). It must not lose that link because a
// newer, unconfirmed exit is in the window.
func TestOOMKillNotHiddenByUnconfirmedExit(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	pod := oomThenExitPod()
	at := t0.Add(time.Minute)
	ev := consequence("ev-backoff", reasonBack, appPath, at)
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod, ev)
	oomFP := mainCauseFingerprint(h.c, pod, ownerOf(t, h), containerApp, false, at, true)
	if oomFP == "" {
		t.Fatal("main links nothing here, so the test proves nothing")
	}
	book.record(oomFP)
	h.clock.set(t0.Add(3*time.Minute + 10*time.Second))
	h.reconcileEvent(ev.Name)
	got := consequenceCauses(h.drain())
	h.clock.add(ConsequenceGrace + time.Second)
	h.reconcileEvent(ev.Name)
	got = append(got, consequenceCauses(h.drain())...)
	if len(got) != 1 || got[0] != oomFP {
		t.Fatalf("the BackOff was emitted with causes %q, want once with the OOM kill's %s, as on main", got, oomFP)
	}
}

// causeSequences is how many generated pod statuses TestCauseLinksAsMainForKillsAndWaits checks: 600, or 200
// under the race detector (seeds 0 to 199, a part of the same set).
func causeSequences() uint64 {
	if raceOn {
		return 200
	}
	return 600
}

// Kinds of a generated case of TestCauseLinksAsMainForKillsAndWaits, by construction.
const (
	kindNone     = "none"
	kindWait     = "wait"
	kindOOM      = "oom"
	kindOOMExit  = "oom, newer exit"
	crashBackOff = "CrashLoopBackOff"
)

// genState is one generated container state, with what it is by construction.
type genState struct {
	state corev1.ContainerState
	// oom and wait say what the state is: an OOM kill (finished at finished), or a waiting cause reason.
	oom, wait bool
	finished  time.Time
}

func genTerminated(rng *rand.Rand, id string) genState {
	finished := t0.Add(time.Duration(rng.IntN(25)-12) * time.Minute)
	switch rng.IntN(5) {
	case 0, 1:
		return genState{state: corev1.ContainerState{Terminated: terminated(id, ReasonOOMKilled, exitOOM, finished)},
			oom: true, finished: finished}
	case 2:
		return genState{state: corev1.ContainerState{Terminated: terminated(id, "Completed", 0, finished)}}
	default:
		return genState{state: corev1.ContainerState{Terminated: terminated(id, reasonError, 1+rng.Int32N(3), finished)},
			finished: finished}
	}
}

// TestCauseLinksAsMainForKillsAndWaits is S1 over generated statuses whose answer is known by construction: the
// app container's state is running, a waiting cause, a CrashLoopBackOff or a termination (an OOM kill, an exit 0
// or a non-zero exit), its last state none or a termination, at times within 12 minutes of t0, the pod evicted
// at times, the BackOff at a time from 6 minutes before t0 to 12 minutes after, or with none. The controller has the Incident of every
// OOM kill and wait and of no exit. Where the construction (and main's code, mainCauseFingerprint) links the
// BackOff to an OOM kill or a wait, the collector emits it once, at once, with that cause.
func TestCauseLinksAsMainForKillsAndWaits(t *testing.T) {
	counts := map[string]int{}
	for seed := range causeSequences() {
		rng := rand.New(rand.NewPCG(seed, 1081)) // #nosec G404 -- a seeded corpus, not a secret
		pod := specPod()
		st := corev1.ContainerStatus{Name: containerApp, RestartCount: 3}
		var cur, last genState
		switch rng.IntN(6) {
		case 0:
			cur.state = running(t0)
		case 1:
			reasons := causereason.Waiting()
			cur.state = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasons[rng.IntN(len(reasons))]}}
			cur.wait = true
		case 2:
			cur.state = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: crashBackOff}}
		default:
			cur = genTerminated(rng, "cur")
		}
		if rng.IntN(5) != 0 {
			last = genTerminated(rng, "last")
		}
		st.State, st.LastTerminationState = cur.state, last.state
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{st}
		if rng.IntN(6) == 0 {
			pod.Status.Phase, pod.Status.Reason = corev1.PodFailed, podReasonEvicted
		}
		at := t0.Add(time.Duration(rng.IntN(37)-12) * 30 * time.Second)
		hasAt := rng.IntN(10) != 0
		ev := consequence("ev-gen", reasonBack, appPath, at)
		if !hasAt {
			ev.EventTime = metav1.MicroTime{}
		}
		// By construction: the wait, else the newest OOM kill with at in [finished, finished+window], the
		// state's on a tie.
		kind := kindNone
		var kill *genState
		for _, g := range []*genState{&cur, &last} {
			if g.oom && hasAt && !at.Before(g.finished) && !at.After(g.finished.Add(KillCauseWindow)) &&
				(kill == nil || g.finished.After(kill.finished)) {
				kill = g
			}
		}
		switch {
		case cur.wait:
			kind = kindWait
		case kill != nil:
			kind = kindOOM
			for _, g := range []*genState{&cur, &last} {
				if !g.oom && !g.wait && !g.finished.IsZero() && g.finished.After(kill.finished) &&
					g.state.Terminated.ExitCode != 0 {
					kind = kindOOMExit
				}
			}
		}
		book := &causeBook{}
		d, rs, _ := workload()
		h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod, ev)
		want := mainCauseFingerprint(h.c, pod, ownerOf(t, h), containerApp, false, at, hasAt)
		if (want == "") != (kind == kindNone) {
			t.Fatalf("seed %d: the reference and the construction disagree: %q, %s", seed, want, kind)
		}
		counts[kind]++
		if want == "" {
			continue
		}
		book.record(want)
		h.clock.set(t0.Add(30 * time.Minute))
		h.reconcileEvent(ev.Name)
		got := consequenceCauses(h.drain())
		h.clock.add(ConsequenceGrace + time.Second)
		h.reconcileEvent(ev.Name)
		got = append(got, consequenceCauses(h.drain())...)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("seed %d (%s): the BackOff was emitted with causes %q, want once with %s, as on main",
				seed, kind, got, want)
		}
	}
	for _, k := range []string{kindWait, kindOOM, kindOOMExit} {
		if counts[k] == 0 {
			t.Errorf("no generated case of kind %q: %v", k, counts)
		}
	}
	t.Logf("cases: %v", counts)
}

// logLine is one line a reconcile logged, with its keys and values.
type logLine struct {
	msg   string
	err   error
	error bool
	kv    map[string]any
}

// logRecord holds the lines of a kvSink and of the sinks WithValues made from it.
type logRecord struct {
	mu    sync.Mutex
	lines []logLine
}

// kvSink records every line with its keys and values.
type kvSink struct {
	rec  *logRecord
	base []any
}

func newKVSink() *kvSink { return &kvSink{rec: &logRecord{}} }

func (s *kvSink) Init(logr.RuntimeInfo) {}
func (s *kvSink) Enabled(int) bool      { return true }
func (s *kvSink) add(err error, isError bool, msg string, kvs []any) {
	l := logLine{msg: msg, err: err, error: isError, kv: map[string]any{}}
	all := slices.Concat(s.base, kvs)
	for i := 0; i+1 < len(all); i += 2 {
		l.kv[fmt.Sprint(all[i])] = all[i+1]
	}
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.rec.lines = append(s.rec.lines, l)
}
func (s *kvSink) Info(_ int, msg string, kvs ...any)      { s.add(nil, false, msg, kvs) }
func (s *kvSink) Error(err error, msg string, kvs ...any) { s.add(err, true, msg, kvs) }
func (s *kvSink) WithValues(kvs ...any) logr.LogSink {
	return &kvSink{rec: s.rec, base: slices.Concat(s.base, kvs)}
}
func (s *kvSink) WithName(string) logr.LogSink { return s }
func (s *kvSink) lines() []logLine {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return slices.Clone(s.rec.lines)
}

// eventLogged reconciles ev-hold at the given offset from at, logging into sink, and keeps what was emitted.
func (e *exitHold) eventLogged(sink *kvSink, after time.Duration) reconcile.Result {
	e.t.Helper()
	e.clock.set(e.at.Add(after))
	ctx := logf.IntoContext(e.t.Context(), logr.New(sink))
	res, err := e.events.Reconcile(ctx, reconcile.Request{NamespacedName: reqFor("ev-hold").NamespacedName})
	if err != nil {
		e.t.Fatalf("event reconcile: %v", err)
	}
	e.got = append(e.got, e.drain()...)
	return res
}

// TestExitCauseRemovedOnlyAfterAsking is S2 and S3, a row per path that can end a hold of a non-zero-exit cause,
// or emit one without a hold: each row checks how it ends, that CauseSeen was asked in the reconcile that
// decided, and what was counted and logged when the exit was removed.
func TestExitCauseRemovedOnlyAfterAsking(t *testing.T) {
	grace := ConsequenceGrace + time.Second
	type logWant struct {
		label string // the outcome counted for the removal, "" for none
		error bool   // the removal logged at error level
	}
	rows := []struct {
		name          string
		withCauseSeen bool
		cfg           Config
		run           func(t *testing.T, e *exitHold, sink *kvSink)
		want          string
		log           logWant
	}{
		{"cause first seen in the expiry reconcile, controller has it", true, Config{}, func(t *testing.T, e *exitHold, sink *kvSink) {
			cur := readPod(t, e.harness, webPod)
			cur.Status = holdPod(reasonBackOff, sampleFailing, 0).Status
			e.update(cur)
			e.eventLogged(sink, 0)
			e.eventLogged(sink, time.Minute)
			cur = readPod(t, e.harness, webPod)
			cur.Status = crashLoopPod(t0, 1).Status
			e.update(cur)
			e.log.book.record(e.fp)
			e.eventLogged(sink, grace)
		}, endCause, logWant{}},
		{"overflow, controller has it", true, Config{MaxHeldConsequences: 1}, func(t *testing.T, e *exitHold, sink *kvSink) {
			other := consequence("ev-other", ReasonUnhealthy, appPath, e.at)
			e.create(other)
			e.reconcileEvent(other.Name)
			if e.c.holds.len() != 1 {
				t.Fatal("the first consequence was not held, so the room is not full")
			}
			e.log.book.record(e.fp)
			e.eventLogged(sink, 0)
		}, endCause, logWant{}},
		{"overflow, not confirmed", true, Config{MaxHeldConsequences: 1}, func(t *testing.T, e *exitHold, sink *kvSink) {
			other := consequence("ev-other", ReasonUnhealthy, appPath, e.at)
			e.create(other)
			e.reconcileEvent(other.Name)
			e.eventLogged(sink, 0)
		}, endBare, logWant{label: holdCauseUnconfirmed}},
		{"hold off, controller has it", true, Config{}, func(_ *testing.T, e *exitHold, sink *kvSink) {
			e.c.holdOff = true
			e.log.book.record(e.fp)
			e.eventLogged(sink, 0)
		}, endCause, logWant{}},
		{"pod gone, controller has it, no release mark", true, Config{}, func(t *testing.T, e *exitHold, sink *kvSink) {
			e.eventLogged(sink, 0)
			e.log.book.record(e.fp)
			e.podGone(t)
			e.eventLogged(sink, time.Minute)
		}, endCause, logWant{}},
		{"delivery retry after the exit was removed, confirmed in between", true, Config{Buffer: 1}, func(t *testing.T, e *exitHold, sink *kvSink) {
			e.eventLogged(sink, 0)
			e.eventLogged(sink, time.Minute)
			e.c.out <- Observation{Fingerprint: fillerFingerprint}
			if res := e.eventLogged(sink, grace); res.RequeueAfter != retryAfter {
				t.Fatalf("requeue after %s, want the delivery retry", res.RequeueAfter)
			}
			e.log.book.record(e.fp)
			e.drain()
			e.eventLogged(sink, grace+retryAfter)
		}, endCause, logWant{label: holdCauseUnconfirmed}},
		{"end of hold, CauseSeen fails", true, Config{}, func(t *testing.T, e *exitHold, sink *kvSink) {
			e.eventLogged(sink, 0)
			e.eventLogged(sink, time.Minute)
			e.log.book.fail(errors.New("synthetic: the Incident store is not loaded"))
			if res := e.eventLogged(sink, grace); res.RequeueAfter != 0 {
				t.Fatalf("requeue after %s at the end of the hold, want no further wait", res.RequeueAfter)
			}
		}, endBare, logWant{label: holdCauseUnanswered, error: true}},
		{"end of hold, released, CauseSeen runs out of time", true, Config{}, func(t *testing.T, e *exitHold, sink *kvSink) {
			e.eventLogged(sink, 0)
			e.eventLogged(sink, time.Minute)
			e.log.book.fail(context.DeadlineExceeded)
			e.c.ReleaseCause(e.fp)
			if res := e.eventLogged(sink, grace); res.RequeueAfter != 0 {
				t.Fatalf("requeue after %s at the end of the hold, want no further wait", res.RequeueAfter)
			}
		}, endBare, logWant{label: holdCauseUnanswered, error: true}},
		{"end of hold, answer no", true, Config{}, func(_ *testing.T, e *exitHold, sink *kvSink) {
			e.eventLogged(sink, 0)
			e.eventLogged(sink, time.Minute)
			e.eventLogged(sink, grace)
		}, endBare, logWant{label: holdCauseUnconfirmed}},
		{"end of hold, CauseSeen nil", false, Config{}, func(_ *testing.T, e *exitHold, sink *kvSink) {
			e.eventLogged(sink, 0)
			e.eventLogged(sink, time.Minute)
			e.eventLogged(sink, grace)
		}, endBare, logWant{label: holdCauseUnconfirmed}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			e := newExitHold(t, r.withCauseSeen, r.cfg)
			sink := newKVSink()
			before := holdOutcomeCounts()
			r.run(t, e, sink)
			if bad := e.log.unconfirmed(e.got); len(bad) > 0 {
				t.Errorf("emitted with unconfirmed causes %v", bad)
			}
			if got := e.end(); got != r.want {
				t.Errorf("the hold ended %s, want %s", got, r.want)
			}
			if r.withCauseSeen && e.log.book.asked == 0 {
				t.Error("CauseSeen was never asked")
			}
			d := outcomesSince(before)
			for _, l := range []string{holdCauseUnconfirmed, holdCauseUnanswered} {
				want := 0.0
				if l == r.log.label {
					want = 1
				}
				if d[l] != want {
					t.Errorf("outcome %s counted %v, want %v (all: %v)", l, d[l], want, d)
				}
			}
			var removal []logLine
			for _, l := range sink.lines() {
				if l.kv["causeFingerprint"] == e.fp && strings.Contains(l.msg, "non-zero exit") {
					removal = append(removal, l)
				}
			}
			switch {
			case r.log.label == "" && len(removal) > 0:
				t.Errorf("logged a removal that did not happen: %+v", removal)
			case r.log.label != "" && len(removal) != 1:
				t.Errorf("logged %d removals naming the fingerprint, want 1: %+v", len(removal), sink.lines())
			case r.log.label != "" && removal[0].error != r.log.error:
				t.Errorf("the removal logged at error level %v, want %v", removal[0].error, r.log.error)
			case r.log.error && removal[0].err == nil:
				t.Error("the error-level removal names no error")
			}
		})
	}
}

// TestDropLogNamesTheRemovedExit is finding 6 of the fifth pass on #432: a hold of an unconfirmed exit whose
// container recovered was logged as dropped with cause=false, and only not_opened was counted. The drop log says
// it had a cause, and the removal is counted cause_unconfirmed beside not_opened.
func TestDropLogNamesTheRemovedExit(t *testing.T) {
	e := newExitHold(t, true, Config{})
	sink := newKVSink()
	before := holdOutcomeCounts()
	e.eventLogged(sink, 0)
	p := readPod(t, e.harness, webPod)
	p.Status.ContainerStatuses[0].State = running(e.at)
	p.Status.ContainerStatuses[0].Ready = true
	e.update(p)
	e.eventLogged(sink, time.Minute)
	e.eventLogged(sink, ConsequenceGrace+time.Second)
	wantOutcomes(t, before, map[string]float64{holdNotOpened: 1, holdCauseUnconfirmed: 1})
	for _, l := range sink.lines() {
		if strings.HasPrefix(l.msg, "Dropped held consequence") {
			if l.kv["cause"] != true {
				t.Errorf("the drop log has cause=%v, want true: %+v", l.kv["cause"], l.kv)
			}
			return
		}
	}
	t.Fatalf("no drop log: %+v", sink.lines())
}

// TestOnlySettleRemovesACause is S4, over the collector's source: outside settleExitCause, no statement assigns
// an empty string to a cause field (CauseFingerprint, causeFallback, a hold's cause), or an Observation or hold
// literal to a hold's obs, and only the named functions assign the cause fields at all: fromEvent builds them,
// copyCause copies them, holdConsequence and settleHold keep a hold's cause equal to its observation's.
func TestOnlySettleRemovesACause(t *testing.T) {
	const settle = "settleExitCause"
	allowed := map[string][]string{
		"CauseFingerprint": {"fromEvent", "copyCause", settle},
		"causeFallback":    {"fromEvent", "copyCause", settle},
		"cause":            {"holdConsequence", "settleHold"},
		"obs":              {"holdConsequence"},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, lhs := range as.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					fns, watched := allowed[sel.Sel.Name]
					if !watched {
						continue
					}
					checked++
					at := fset.Position(as.Pos())
					if !slices.Contains(fns, fn.Name.Name) {
						t.Errorf("%s: %s assigns %s; only %v may", at, fn.Name.Name, sel.Sel.Name, fns)
					}
					if fn.Name.Name == settle || len(as.Rhs) != len(as.Lhs) {
						continue
					}
					switch rhs := as.Rhs[i].(type) {
					case *ast.BasicLit:
						if rhs.Value == `""` {
							t.Errorf("%s: %s clears %s", at, fn.Name.Name, sel.Sel.Name)
						}
					case *ast.CompositeLit:
						t.Errorf("%s: %s replaces %s with a literal", at, fn.Name.Name, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("no assignment of a cause field found: the check reads nothing")
	}
}
