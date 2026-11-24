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

package incidentcontext

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// The order of the log reads (#943, decision of 2026-11-20). The properties:
//
//   - O1: every container's first read comes before any second read, in the order of the containers, with
//     the deadline a read of its own would have (one call timeout from its start): no log that one read per
//     container, of the instance chosen first, would get is lost or read later.
//   - O2: a second read (the other instance after an answer without lines, or the previous instance after
//     the excerpt of a terminated current one) runs only in the time the section's budget has left, and none
//     starts when no time is left.
//   - O3: a second read alone never marks the log excerpt failed.

// exitedBeforeWindow makes the pod's container exited just now, after an earlier instance that terminated
// long before the window.
func exitedBeforeWindow(p *corev1.Pod) *corev1.Pod {
	p = exited(p)
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated.StartedAt = longAgo
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = longAgo
	return p
}

// bothExitedBeforeWindow is the web fixture with the containers of both pods exited, each after an instance
// that terminated long before the window.
func bothExitedBeforeWindow() []client.Object {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	return []client.Object{
		d, rs,
		exitedBeforeWindow(pod(crashPod, rs.Labels, longAgo, ref)),
		exitedBeforeWindow(pod(secondPod, rs.Labels, longAgo, ref)),
	}
}

// O1, O2, O3: the case the third review of the fix proved. Both current instances answer with lines in 240 ms,
// both previous instances hang, the call timeout is 400 ms. One read per container takes 0.48 s and keeps both
// excerpts. Reading the first container's previous instance before the second container's current one used
// up the budget and cut that read short.
func TestFirstReadsComeBeforeSecondReads(t *testing.T) {
	r, k := underLoad(t, func(k time.Duration) (timedRun, bool) {
		l := &timedLogs{
			plan:    map[string][2]outcome{crashPod: {slow, hangs}, secondPod: {slow, hangs}},
			slowFor: 240 * time.Millisecond * k,
		}
		c, _, _, reached := timedBuildOf(t, bothExitedBeforeWindow(), l, 400*time.Millisecond*k, 10*time.Second*k)
		return timedRun{c: c, l: l}, reached
	})
	c, l, call := r.c, r.l, 400*time.Millisecond*k
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) < 2 || l.calls[0].Pod != crashPod || l.calls[0].Previous ||
		l.calls[1].Pod != secondPod || l.calls[1].Previous {
		t.Fatalf("calls %+v, want the current instance of each container first", l.calls)
	}
	checkOwnTimeout(t, l, 2, call)
	if len(l.calls) != 3 {
		t.Errorf("calls %+v: want the first container's previous instance read, and no read started after the budget", l.calls)
	}
	var data []string
	for _, blk := range blocks(c, SectionLogs) {
		data = append(data, blk.Data()...)
	}
	for _, p := range []string{crashPod, secondPod} {
		if !slices.Contains(data, p+" current instance failed\n") {
			t.Errorf("excerpt of %s missing: %q", p, data)
		}
	}
	if slices.Contains(c.FailedSources, SectionLogs) {
		t.Errorf("failed sources %v: only a second read failed", c.FailedSources)
	}
	checkDeadlines(t, l, call, 2)
	checkStatusAgrees(t, c)
}

// bothRestarted is the web fixture with the containers of both pods running again after an instance that
// terminated inside the window: the previous instance is read first.
func bothRestarted() []client.Object {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	return []client.Object{
		d, rs, restarted(pod(crashPod, rs.Labels, longAgo, ref)), restarted(pod(secondPod, rs.Labels, longAgo, ref)),
	}
}

// fixtureWindow is the window of the fixtures' Incident, which a read of a current instance asks for.
const fixtureWindow = 40 * time.Minute

// oneReadPerContainer is the reference: one read per container with the instance choice of #943. It is not
// the log section before #943, which read the previous instance of a container whose current instance had
// terminated; a log that only that instance gives can come later or not at all (the known limits in
// limits_test.go). It models:
//
//   - the containers in the order of the pick, the first pod's container first (both have the same score
//     and restarts);
//   - one read per container, of the instance read first (the current one when it has terminated, the
//     previous one when the container runs again after a termination inside the window), asking for an equal
//     share of the log bound, the window for a current instance and none for a previous one;
//   - each read with a deadline of one call timeout from its start (the build deadline is far off);
//   - an excerpt for a read that answers with lines within its deadline (answers, slow);
//   - the log excerpt failed when a read failed (forbidden, hangs), not when it answered without lines.
//
// It returns the calls, the excerpts and whether the log excerpt is failed.
func oneReadPerContainer(plan map[string][2]outcome, previousFirst bool) ([]providers.LogQuery, []string, bool) {
	calls := make([]providers.LogQuery, 0, 2)
	var excerpts []string
	failed := false
	for _, p := range []string{crashPod, secondPod} {
		q := providers.LogQuery{
			Namespace: "shop", Pod: p, Container: appName, Previous: previousFirst, MaxBytes: DefaultLogBytes / MaxLogExcerpts,
		}
		o := plan[p][0]
		if previousFirst {
			o = plan[p][1]
		} else {
			q.Since = fixtureWindow
		}
		calls = append(calls, q)
		switch {
		case o == answers || o == slow:
			excerpts = append(excerpts, p+" "+instanceName(previousFirst)+" failed\n")
		case o.fails():
			failed = true
		}
	}
	return calls, excerpts, failed
}

// O1, O2, O3 and B1, B2 over the outcomes of the reads, against the reference [oneReadPerContainer]: every
// container plan (the current and the previous instance's outcome) for each of two containers, in each order,
// with the current instance read first and with the previous one read first. The section's first calls are the
// reference's calls in order, each with the reference's deadline; every excerpt the reference gets is in the
// context; the log excerpt is failed exactly when the reference's is; the second reads follow only a first
// read that did not fail, at most one per container, ending within two call timeouts of the first call.
func TestLogReadsMatchOneReadPerContainer(t *testing.T) {
	// A slow answer comes 90 ms before the call timeout, decided by the call's deadline. The reads of the
	// target run with the call timeout too: a build that makes no log call runs again with longer durations
	// (underLoad).
	const call, slowFor = 150 * time.Millisecond, 60 * time.Millisecond
	// The table of the budget tests (each outcome for both instances, and after a current instance that
	// answers), with slow answers before a second read. A plan is the current, then the previous instance's
	// outcome.
	var plans [][2]outcome
	for o := answers; o <= hangs; o++ {
		plans = append(plans, [2]outcome{o, o})
		if o != answers {
			plans = append(plans, [2]outcome{answers, o})
		}
	}
	plans = append(plans, [2]outcome{slow, hangs}, [2]outcome{hangs, answers},
		[2]outcome{slow, empty}, [2]outcome{slow, collected}, [2]outcome{slow, forbidden})
	// With the previous instance read first, the plans that tell its outcomes apart and those of the current
	// instance read after it (the full product under -race takes a minute).
	restartedPlans := [][2]outcome{
		{answers, answers}, {answers, slow}, {answers, hangs}, {answers, forbidden}, {slow, collected},
		{hangs, empty}, {forbidden, refused}, {hangs, hangs}, {slow, slow},
	}
	fixtures := []struct {
		name          string
		objs          func() []client.Object
		previousFirst bool
		plans         [][2]outcome
	}{{"exited", bothExited, false, plans}, {"restarted", bothRestarted, true, restartedPlans}}
	for _, f := range fixtures {
		for _, a := range f.plans {
			for _, b := range f.plans {
				t.Run(fmt.Sprintf("%s/%v-%v_%v-%v", f.name, a[0], a[1], b[0], b[1]), func(t *testing.T) {
					t.Parallel()
					plan := map[string][2]outcome{crashPod: a, secondPod: b}
					r, k := underLoad(t, func(k time.Duration) (timedRun, bool) {
						l := &timedLogs{plan: plan, slowFor: slowFor * k}
						c, metricReads, _, reached := timedBuildOf(t, f.objs(), l, call*k, 10*time.Second*k)
						return timedRun{c, l, metricReads}, reached
					})
					c, l, metricReads, call := r.c, r.l, r.metricReads, call*k
					l.mu.Lock()
					defer l.mu.Unlock()
					want, excerpts, failed := oneReadPerContainer(plan, f.previousFirst)
					checkFirstReads(t, l, want, call)
					checkSecondReads(t, l, plan, len(want))
					checkDeadlines(t, l, call, len(want))
					if metricReads == 0 {
						t.Error("no metric reads")
					}
					var data []string
					for _, blk := range blocks(c, SectionLogs) {
						if strings.Count(blk.Text, "instance\n") == 0 {
							t.Errorf("block names no instance: %q", blk.Text)
						}
						data = append(data, blk.Data()...)
					}
					for _, e := range excerpts {
						if !slices.Contains(data, e) {
							t.Errorf("excerpt %q of one read per container is missing: %q", e, data)
						}
					}
					if got := slices.Contains(c.FailedSources, SectionLogs); got != failed {
						t.Errorf("log excerpt failed %t, one read per container %t", got, failed)
					}
					checkStatusAgrees(t, c)
				})
			}
		}
	}
}

// checkFirstReads checks O1: the first calls are want, in order, each with a deadline of one call timeout
// from its start.
func checkFirstReads(t *testing.T, l *timedLogs, want []providers.LogQuery, call time.Duration) {
	t.Helper()
	if len(l.calls) < len(want) || !slices.Equal(l.calls[:len(want)], want) {
		t.Fatalf("calls %+v, want first %+v", l.calls, want)
	}
	checkOwnTimeout(t, l, len(want), call)
}

// checkOwnTimeout checks that each of the first n calls had the deadline of a read of its own: one call
// timeout from its start. The builder sets a deadline after the call before returned and before the source
// sees the call, so it lies between the end of the call before plus the timeout and the start plus the
// timeout, whatever the load; a deadline cut to a budget shared with an earlier read falls below.
func checkOwnTimeout(t *testing.T, l *timedLogs, n int, call time.Duration) {
	t.Helper()
	for i := range n {
		d := l.deadlines[i]
		if d.After(l.starts[i].Add(call)) {
			t.Errorf("first read %d may run until %s after its start, want the call timeout %s", i, d.Sub(l.starts[i]), call)
		}
		if i > 0 && d.Before(l.ends[i-1].Add(call)) {
			t.Errorf("first read %d may run until %s after the read before ended, want the call timeout %s",
				i, d.Sub(l.ends[i-1]), call)
		}
	}
}

// checkSecondReads checks O2 and B2: the calls after the first reads are at most one per container, of the
// instance not read first, after a first read that did not fail. That they end within the budget is
// checkDeadlines'.
func checkSecondReads(t *testing.T, l *timedLogs, plan map[string][2]outcome, firstReads int) {
	t.Helper()
	seen := map[string]bool{}
	for i := firstReads; i < len(l.calls); i++ {
		q := l.calls[i]
		var first providers.LogQuery
		for _, f := range l.calls[:firstReads] {
			if f.Pod == q.Pod {
				first = f
			}
		}
		o := plan[q.Pod][0]
		if first.Previous {
			o = plan[q.Pod][1]
		}
		switch {
		case first.Pod == "" || seen[q.Pod] || q.Previous == first.Previous:
			t.Errorf("call %d %+v is not the one second read of a container", i, q)
		case o.fails():
			t.Errorf("call %d %+v follows a failed read (%v)", i, q, o)
		}
		seen[q.Pod] = true
	}
}

// checkStatusAgrees checks that the log excerpt is a failed source exactly when one of its blocks is marked
// could not fetch.
func checkStatusAgrees(t *testing.T, c *Context) {
	t.Helper()
	bad := false
	for _, blk := range blocks(c, SectionLogs) {
		bad = bad || blk.Availability == CouldNotFetch
	}
	if got := slices.Contains(c.FailedSources, SectionLogs); got != bad {
		t.Errorf("log excerpt failed %t, a block could not fetch %t", got, bad)
	}
}
