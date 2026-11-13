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

package clearing

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

func TestGoodSamplesOpenAWindow(t *testing.T) {
	st := samples(t, 3)
	if st.Sample != 3 || st.PodsCovered != 3 || st.DesiredReplicas != 3 || len(st.Baselines) != 3 || st.Resets != 0 {
		t.Fatalf("status after three good samples: %+v", st)
	}
	if !st.WindowStart.Time.Equal(t0) || !st.SampledAt.Time.Equal(t0.Add(2*PollInterval)) {
		t.Fatalf("window %v to %v, want %v to %v", st.WindowStart, st.SampledAt, t0, t0.Add(2*PollInterval))
	}
	if span := ReportWindow(&st).Span(st.SampledAt.Time); span != 2*PollInterval {
		t.Fatalf("span = %v, want %v", span, 2*PollInterval)
	}
}

// A StatefulSet with a partition above 0 is complete for its controller with only the pods above the
// partition updated, but the pods below it keep the old template: such a sample is not evidence.
func TestPartitionedStatefulSetIsNotCovered(t *testing.T) {
	dbLabels := map[string]string{labelApp: "db"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: ns, UID: stsUID, Labels: dbLabels},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](3), Selector: &metav1.LabelSelector{MatchLabels: dbLabels},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2)},
			},
		},
		Status: appsv1.StatefulSetStatus{UpdateRevision: dbRevision, CurrentRevision: dbOldRevision},
	}
	ps := make([]corev1.Pod, 0, 3)
	for i, rev := range []string{dbOldRevision, dbOldRevision, dbRevision} {
		p := pod(i, 0)
		p.Name, p.Labels = fmt.Sprintf("db-%d", i), map[string]string{labelApp: "db", StatefulSetRevisionLabel: rev}
		p.OwnerReferences = controllerRef("StatefulSet", "db", stsUID)
		ps = append(ps, p)
	}
	s := spec()
	s.TargetUID, s.TemplateRevision = stsUID, dbRevision
	in := Input{Now: t0, Target: sts, Pods: ps, MemoryLimit: &limit}
	r := sample(t, s, v1alpha1.VerificationReportStatus{}, in)
	if r.Reason != v1alpha1.ResetPartialRollout || r.Status.PodsCovered != 1 || r.Status.DesiredReplicas != 3 {
		t.Fatalf("partitioned StatefulSet: reason %q, covered %d of %d; want PartialRollout, 1 of 3",
			r.Reason, r.Status.PodsCovered, r.Status.DesiredReplicas)
	}
	if r.Status.WindowStart != nil {
		t.Fatal("a window opened on a partitioned rollout")
	}

	// Once the owner lowers the partition and every pod runs the update revision, the sample is good.
	for i := range ps {
		ps[i].Labels[StatefulSetRevisionLabel] = dbRevision
	}
	if r := sample(t, s, r.Status, in); r.Reason != "" || r.Status.PodsCovered != 3 {
		t.Fatalf("every pod updated: reason %q, covered %d", r.Reason, r.Status.PodsCovered)
	}
}

// Owner references are untrusted: a pod whose controller reference does not verify up to the target adds no
// coverage, however many such pods there are.
func TestForgedOwnerReferenceIsNotCounted(t *testing.T) {
	forgeries := map[string]func(*corev1.Pod){
		"ReplicaSet UID that is not the ReplicaSet's": func(p *corev1.Pod) { p.OwnerReferences[0].UID = otherUID },
		"ReplicaSet that does not exist":              func(p *corev1.Pod) { p.OwnerReferences[0].Name = "web-forged" },
		"labels the ReplicaSet does not select":       func(p *corev1.Pod) { p.Labels = map[string]string{labelApp: "intruder"} },
		"reference not marked controller":             func(p *corev1.Pod) { p.OwnerReferences[0].Controller = nil },
		"names the Deployment directly":               func(p *corev1.Pod) { p.OwnerReferences = controllerRef("Deployment", "web", deployUID) },
	}
	for name, forge := range forgeries {
		t.Run(name, func(t *testing.T) {
			ps := pods(3)
			forge(&ps[2])
			r := sample(t, spec(), v1alpha1.VerificationReportStatus{}, input(t0, 3, ps))
			if r.Reason != v1alpha1.ResetPartialRollout || r.Status.PodsCovered != 2 {
				t.Fatalf("reason %q, covered %d; want PartialRollout, 2", r.Reason, r.Status.PodsCovered)
			}
		})
	}
	t.Run("ReplicaSet that names the Deployment with another UID", func(t *testing.T) {
		in := input(t0, 3, pods(3))
		rs := replicaSet(rsName, rsUID, revision)
		rs.OwnerReferences[0].UID = otherUID
		in.ReplicaSets = getter(rs)
		if r := sample(t, spec(), v1alpha1.VerificationReportStatus{}, in); r.Status.PodsCovered != 0 ||
			r.Reason != v1alpha1.ResetPartialRollout {
			t.Fatalf("reason %q, covered %d; want PartialRollout, 0", r.Reason, r.Status.PodsCovered)
		}
	})
	t.Run("ReplicaSet whose labels the Deployment does not select", func(t *testing.T) {
		in := input(t0, 3, pods(3))
		rs := replicaSet(rsName, rsUID, revision)
		// The ReplicaSet still selects the pods; only its own labels fall outside the Deployment's selector.
		rs.Labels = map[string]string{labelApp: "other"}
		in.ReplicaSets = getter(rs)
		if r := sample(t, spec(), v1alpha1.VerificationReportStatus{}, in); r.Status.PodsCovered != 0 {
			t.Fatalf("covered %d, want 0", r.Status.PodsCovered)
		}
	})
	t.Run("a copy of a real reference adds a pod above the desired replicas", func(t *testing.T) {
		r := sample(t, spec(), v1alpha1.VerificationReportStatus{}, input(t0, 3, pods(4)))
		if r.Reason != v1alpha1.ResetPartialRollout {
			t.Fatalf("four pods for three replicas: reason %q, want PartialRollout", r.Reason)
		}
	})
}

func TestPodsThatAreNotOfTheRollout(t *testing.T) {
	cases := map[string]struct {
		mod  func(*Input)
		want v1alpha1.ResetReason
	}{
		"a pod on the old ReplicaSet": {func(in *Input) {
			in.Pods[2].OwnerReferences = controllerRef("ReplicaSet", oldRSName, oldRSUID)
		}, v1alpha1.ResetPartialRollout},
		"a terminating pod": {func(in *Input) {
			in.Pods[2].DeletionTimestamp = mt(t0)
		}, v1alpha1.ResetPartialRollout},
		"a pod with another memory limit": {func(in *Input) {
			in.Pods[2].Spec.Containers[0].Resources.Limits = nil
		}, v1alpha1.ResetPartialRollout},
		"a pod without the action's container": {func(in *Input) {
			in.Pods[2].Spec.Containers[0].Name = "other"
		}, v1alpha1.ResetPartialRollout},
		"a pod without a status for every container": {func(in *Input) {
			in.Pods[2].Status.ContainerStatuses = in.Pods[2].Status.ContainerStatuses[:1]
		}, v1alpha1.ResetEvidenceMissing},
		"a pod that is not Ready": {func(in *Input) {
			in.Pods[2].Status.Conditions[0].Status = corev1.ConditionFalse
		}, v1alpha1.ResetPodNotReady},
		"scaled to zero": {func(in *Input) {
			in.Target = deployment(0)
		}, v1alpha1.ResetScaledToZero},
		"a target with another UID": {func(in *Input) {
			d := deployment(3)
			d.UID = otherUID
			in.Target = d
		}, v1alpha1.ResetEvidenceMissing},
		"a target on another revision": {func(in *Input) {
			d := deployment(3)
			d.Annotations[`deployment.kubernetes.io/revision`] = "3"
			in.Target = d
		}, v1alpha1.ResetEvidenceMissing},
		"no target": {func(in *Input) {
			in.Target = (*appsv1.Deployment)(nil)
		}, v1alpha1.ResetEvidenceMissing},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := input(t0, 3, pods(3))
			c.mod(&in)
			if r := sample(t, spec(), v1alpha1.VerificationReportStatus{}, in); r.Reason != c.want {
				t.Fatalf("reason %q, want %q", r.Reason, c.want)
			}
		})
	}
}

// A restart count raised while nobody watched is a restart, never a new baseline: the stored baselines are
// durable, so the first sample after a gap compares against them.
func TestRestartDuringAGapResetsAsRestarted(t *testing.T) {
	st := samples(t, 2)
	ps := pods(3)
	ps[1].Status.ContainerStatuses[0].RestartCount = 1
	r := sample(t, spec(), st, input(st.SampledAt.Add(10*time.Minute), 3, ps))
	if r.Reason != v1alpha1.ResetRestarted || r.Status.TroubleResets != 1 || r.Status.LastTroubleReset == nil ||
		!r.Status.LastTroubleReset.Time.Equal(&r.Status.LastReset.Time) {
		t.Fatalf("reason %q, trouble %d %+v; want Restarted recorded as trouble", r.Reason, r.Status.TroubleResets, r.Status.LastTroubleReset)
	}
	if r.Status.Resets != 1 || ReportWindow(&r.Status).Open() {
		t.Fatalf("resets %d, open %v; want 1 and a closed window", r.Status.Resets, ReportWindow(&r.Status).Open())
	}
	// The raised count is the new baseline once recorded, so the next sample is good again.
	next := sample(t, spec(), r.Status, input(r.Status.SampledAt.Add(PollInterval), 3, ps))
	if next.Reason != "" || !next.Status.WindowStart.After(r.Status.LastReset.Time.Time) {
		t.Fatalf("after the reset: reason %q, window %v", next.Reason, next.Status.WindowStart)
	}
}

// A pod that is not in the baselines after a gap may have replaced one whose history went with it; without a
// gap a new pod starts at a baseline of 0.
func TestNewPodAfterAGap(t *testing.T) {
	st := samples(t, 2)
	ps := pods(4)[1:] // pod 0 deleted, pod 3 is its replacement
	t.Run("after a gap", func(t *testing.T) {
		r := sample(t, spec(), st, input(st.SampledAt.Add(GapAfter+time.Second), 3, ps))
		if r.Reason != v1alpha1.ResetObservationGap || r.Status.TroubleResets != 0 || r.Status.LastTroubleReset != nil {
			t.Fatalf("reason %q, trouble %d; want ObservationGap and no trouble recorded", r.Reason, r.Status.TroubleResets)
		}
	})
	t.Run("without a gap", func(t *testing.T) {
		r := sample(t, spec(), st, input(st.SampledAt.Add(GapAfter), 3, ps))
		if r.Reason != "" {
			t.Fatalf("reason %q, want a good sample", r.Reason)
		}
		if b := baseline(r.Status, podUID(3)); b == nil || b.Restarts != 0 {
			t.Fatalf("new pod's baseline = %+v, want 0", b)
		}
		if baseline(r.Status, podUID(0)) != nil {
			t.Fatal("the deleted pod's baseline was not pruned")
		}
	})
	t.Run("samples without sampledAt", func(t *testing.T) {
		noTime := *st.DeepCopy()
		noTime.SampledAt = nil
		if r := sample(t, spec(), noTime, input(t0.Add(PollInterval), 3, ps)); r.Reason != v1alpha1.ResetObservationGap {
			t.Fatalf("reason %q, want ObservationGap", r.Reason)
		}
	})
	t.Run("without a gap, a new pod that already restarted", func(t *testing.T) {
		restarted := pods(4)[1:]
		restarted[2].Status.ContainerStatuses[1].RestartCount = 1
		if r := sample(t, spec(), st, input(st.SampledAt.Add(PollInterval), 3, restarted)); r.Reason != v1alpha1.ResetRestarted {
			t.Fatalf("reason %q, want Restarted against a baseline of 0", r.Reason)
		}
	})
}

func baseline(st v1alpha1.VerificationReportStatus, uid types.UID) *v1alpha1.PodBaseline {
	for i := range st.Baselines {
		if st.Baselines[i].PodUID == v1alpha1.UID(uid) {
			return &st.Baselines[i]
		}
	}
	return nil
}

// A container that terminated at or after windowStart minus one poll interval resets the window, even with a
// restart count equal to its baseline; an older termination does not.
func TestTerminationInsideTheWindowIsARestart(t *testing.T) {
	st := samples(t, 3)
	for name, c := range map[string]struct {
		finished time.Time
		want     v1alpha1.ResetReason
	}{
		"inside the window":       {st.WindowStart.Add(time.Minute), v1alpha1.ResetRestarted},
		"within the margin":       {st.WindowStart.Add(-TerminationMargin), v1alpha1.ResetRestarted},
		"before the margin":       {st.WindowStart.Add(-TerminationMargin - time.Second), ""},
		"node clock far in front": {st.WindowStart.Add(24 * time.Hour), v1alpha1.ResetRestarted},
	} {
		t.Run(name, func(t *testing.T) {
			ps := pods(3)
			ps[0].Status.ContainerStatuses[1].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
				Reason: reasonError, FinishedAt: metav1.NewTime(c.finished),
			}
			if r := sample(t, spec(), st, input(st.SampledAt.Add(PollInterval), 3, ps)); r.Reason != c.want {
				t.Fatalf("reason %q, want %q", r.Reason, c.want)
			}
		})
	}
}

// An OOM kill of the action's container on a pod of the rollout is recorded once and fails the request at
// once, whatever the window; one of another container is a Restarted reset.
func TestOOMKill(t *testing.T) {
	st := samples(t, 1)
	oom := func(container string) []corev1.Pod {
		ps := pods(3)
		for i := range ps[1].Status.ContainerStatuses {
			s := &ps[1].Status.ContainerStatuses[i]
			if s.Name == container {
				s.RestartCount = 1
				s.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
					Reason: ReasonOOMKilled, ExitCode: 137, FinishedAt: metav1.NewTime(t0.Add(-time.Hour)),
				}
			}
		}
		return ps
	}
	t.Run("the action's container", func(t *testing.T) {
		r := sample(t, spec(), st, input(st.SampledAt.Add(PollInterval), 3, oom(app)))
		if !r.OOMKilled || r.Status.OOMKill == nil || r.Status.OOMKill.Container != app ||
			r.Status.OOMKill.PodUID != v1alpha1.UID(podUID(1)) {
			t.Fatalf("OOM kill not recorded: %+v, %+v", r.OOMKilled, r.Status.OOMKill)
		}
		if !OOMFails(report(r.Status), expected()) {
			t.Fatal("a recorded OOM kill does not fail the request")
		}
		// It is recorded once: a later sample keeps the first record and does not report it again.
		again := sample(t, spec(), r.Status, input(r.Status.SampledAt.Add(PollInterval), 3, oom(app)))
		if again.OOMKilled || !equality.Semantic.DeepEqual(again.Status.OOMKill, r.Status.OOMKill) {
			t.Fatalf("second sample: OOMKilled %v, record %+v", again.OOMKilled, again.Status.OOMKill)
		}
		// An OOM record of a container other than the action's does not fail the request.
		forged := report(*r.Status.DeepCopy())
		forged.Status.OOMKill.Container = sidecar
		if OOMFails(forged, expected()) {
			t.Fatal("an OOM record of another container failed the request")
		}
		// A report bound to another request does not fail this one.
		other := expected()
		other.RequestUID = otherUID
		if OOMFails(report(r.Status), other) {
			t.Fatal("an OOM kill in a mismatched report failed the request")
		}
	})
	t.Run("another container", func(t *testing.T) {
		r := sample(t, spec(), st, input(st.SampledAt.Add(PollInterval), 3, oom(sidecar)))
		if r.OOMKilled || r.Status.OOMKill != nil || r.Reason != v1alpha1.ResetRestarted {
			t.Fatalf("OOMKilled %v, record %+v, reason %q; want a Restarted reset only", r.OOMKilled, r.Status.OOMKill, r.Reason)
		}
	})
}

// A target with more desired replicas than the bound, or a pod list longer than the pages hold, cannot be
// covered: the sample is EvidenceMissing and no baseline is dropped to fit.
func TestMoreThan200PodsIsEvidenceMissing(t *testing.T) {
	st := samples(t, 1)
	cases := map[string]Input{
		"201 desired replicas": input(st.SampledAt.Add(PollInterval), MaxPods+1, pods(MaxPods+1)),
		"a truncated list": func() Input {
			in := input(st.SampledAt.Add(PollInterval), 3, pods(3))
			in.Truncated = true
			return in
		}(),
		"more pods than the pages hold": input(st.SampledAt.Add(PollInterval), 3, pods(PageSize*MaxPages+1)),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			r := sample(t, spec(), st, in)
			if r.Reason != v1alpha1.ResetEvidenceMissing {
				t.Fatalf("reason %q, want EvidenceMissing", r.Reason)
			}
			if len(r.Status.Baselines) != len(st.Baselines) {
				t.Fatalf("baselines %d, want the %d kept", len(r.Status.Baselines), len(st.Baselines))
			}
		})
	}
	t.Run("200 replicas", func(t *testing.T) {
		r := sample(t, spec(), v1alpha1.VerificationReportStatus{}, input(t0, MaxPods, pods(MaxPods)))
		if r.Reason != "" || len(r.Status.Baselines) != MaxPods {
			t.Fatalf("reason %q, baselines %d; want a good sample with 200", r.Reason, len(r.Status.Baselines))
		}
	})
	t.Run("pods of the rollout and kept baselines above the bound", func(t *testing.T) {
		full := sample(t, spec(), v1alpha1.VerificationReportStatus{}, input(t0, MaxPods, pods(MaxPods))).Status
		ps := pods(MaxPods + 1)
		ps[0].DeletionTimestamp = mt(t0) // listed, so its baseline stays, but no longer of the rollout
		r := sample(t, spec(), full, input(t0.Add(PollInterval), MaxPods, ps))
		if r.Reason != v1alpha1.ResetEvidenceMissing || len(r.Status.Baselines) != MaxPods {
			t.Fatalf("reason %q, baselines %d; want EvidenceMissing with the 200 kept", r.Reason, len(r.Status.Baselines))
		}
	})
}

// Pruning the baseline of a deleted pod moves no window and adds no reset.
func TestPrunedBaselineMovesNoWindow(t *testing.T) {
	st := samples(t, 2)
	// A fourth pod (a surge) was sampled once and is gone again.
	st.Baselines = append(st.Baselines, v1alpha1.PodBaseline{PodUID: v1alpha1.UID(podUID(9)), Restarts: 4})
	r := sample(t, spec(), st, input(st.SampledAt.Add(PollInterval), 3, pods(3)))
	if r.Reason != "" || r.Status.Resets != st.Resets || !r.Status.WindowStart.Equal(st.WindowStart) {
		t.Fatalf("reason %q, resets %d, window %v; want a good sample, %d resets, window %v",
			r.Reason, r.Status.Resets, r.Status.WindowStart, st.Resets, st.WindowStart)
	}
	if baseline(r.Status, podUID(9)) != nil || len(r.Status.Baselines) != 3 {
		t.Fatalf("baselines %+v, want the deleted pod's pruned", r.Status.Baselines)
	}
}

// A failed read is retried, never a sample: nothing changes, not even the counter.
func TestFailedReadIsNotASample(t *testing.T) {
	st := samples(t, 1)
	in := input(st.SampledAt.Add(PollInterval), 3, pods(3))
	boom := errors.New("connection refused")
	in.ReplicaSets = func(string) (*appsv1.ReplicaSet, error) { return nil, boom }
	if _, err := Sample(spec(), st, in); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read error", err)
	}
	in.ReplicaSets = nil
	if _, err := Sample(spec(), st, in); err == nil {
		t.Fatal("a Deployment sampled without a ReplicaSet reader gave a sample")
	}
}

// Each ReplicaSet is read once per sample, whatever the number of its pods.
func TestReplicaSetReadOncePerSample(t *testing.T) {
	in := input(t0, 3, pods(3))
	reads := 0
	get := in.ReplicaSets
	in.ReplicaSets = func(name string) (*appsv1.ReplicaSet, error) { reads++; return get(name) }
	sample(t, spec(), v1alpha1.VerificationReportStatus{}, in)
	if reads != 1 {
		t.Fatalf("ReplicaSet reads = %d, want 1", reads)
	}
}

// The verifier's clock going back can only delay success: the window never starts before a reset and a
// reset is never recorded before the window's start.
func TestVerifierClockGoingBack(t *testing.T) {
	st := samples(t, 3)
	ps := pods(3)
	ps[0].Status.Conditions[0].Status = corev1.ConditionFalse
	bad := sample(t, spec(), st, input(t0.Add(-time.Hour), 3, ps))
	if ReportWindow(&bad.Status).Open() {
		t.Fatal("a reset with the clock gone back left the window open")
	}
	good := sample(t, spec(), bad.Status, input(t0.Add(-time.Hour), 3, pods(3)))
	if ReportWindow(&good.Status).Open() {
		t.Fatal("a good sample with the clock gone back reopened the window at or before the reset")
	}
	if good.Status.WindowStart.Before(st.WindowStart) {
		t.Fatal("windowStart moved back")
	}
}

// withInit adds a native sidecar (an init container with restartPolicy Always) and an ordinary init
// container that completed, with their statuses, to the pod.
func withInit(p corev1.Pod, completedAt time.Time) corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}, {Name: "mesh", RestartPolicy: &always}}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "migrate", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Completed", FinishedAt: metav1.NewTime(completedAt),
		}}},
		{Name: "mesh"},
	}
	return p
}

// Native sidecars count like the pod's containers: a restart, a termination inside the window or a missing
// status resets the window. An ordinary init container that completed does not.
func TestNativeSidecarsCount(t *testing.T) {
	podsWithInit := func() []corev1.Pod {
		ps := pods(3)
		for i := range ps {
			// Completed inside the window: an ordinary init container's termination is no restart.
			ps[i] = withInit(ps[i], t0.Add(time.Minute))
		}
		return ps
	}
	var st v1alpha1.VerificationReportStatus
	for i := range 4 {
		r := sample(t, spec(), st, input(t0.Add(time.Duration(i)*PollInterval), 3, podsWithInit()))
		if r.Reason != "" {
			t.Fatalf("quiet sample %d: reason %q, want a good sample", i, r.Reason)
		}
		st = r.Status
	}
	cases := map[string]struct {
		mod  func(*corev1.ContainerStatus)
		want v1alpha1.ResetReason
	}{
		"a sidecar restart at the last sample": {func(s *corev1.ContainerStatus) { s.RestartCount = 1 }, v1alpha1.ResetRestarted},
		"a sidecar termination inside the window": {func(s *corev1.ContainerStatus) {
			s.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: reasonError, FinishedAt: metav1.NewTime(t0.Add(time.Minute))}
		}, v1alpha1.ResetRestarted},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ps := podsWithInit()
			c.mod(&ps[1].Status.InitContainerStatuses[1])
			if r := sample(t, spec(), st, input(st.SampledAt.Add(PollInterval), 3, ps)); r.Reason != c.want {
				t.Fatalf("reason %q, want %q", r.Reason, c.want)
			}
		})
	}
	t.Run("a sidecar without a status", func(t *testing.T) {
		ps := podsWithInit()
		ps[1].Status.InitContainerStatuses = ps[1].Status.InitContainerStatuses[:1]
		if r := sample(t, spec(), st, input(st.SampledAt.Add(PollInterval), 3, ps)); r.Reason != v1alpha1.ResetEvidenceMissing {
			t.Fatalf("reason %q, want EvidenceMissing", r.Reason)
		}
	})
}

// An OOM kill of the action's container on a returned pod of the rollout is affirmative evidence even when
// the bound rejects coverage: the sample records it and fails the request, and no baseline is dropped.
func TestOOMKillBeforeTheBound(t *testing.T) {
	st := sample(t, spec(), v1alpha1.VerificationReportStatus{}, input(t0, MaxPods, pods(MaxPods))).Status
	ps := pods(MaxPods)
	ps[7].Status.ContainerStatuses[0].RestartCount = 1
	ps[7].Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
		Reason: ReasonOOMKilled, ExitCode: 137, FinishedAt: metav1.NewTime(t0),
	}
	in := input(t0.Add(PollInterval), MaxPods, ps)
	in.Truncated = true
	r := sample(t, spec(), st, in)
	if !r.OOMKilled || r.Status.OOMKill == nil || r.Status.OOMKill.PodUID != v1alpha1.UID(podUID(7)) {
		t.Fatalf("reason %q, OOMKilled %v, record %+v; want the OOM kill recorded", r.Reason, r.OOMKilled, r.Status.OOMKill)
	}
	if r.Reason != v1alpha1.ResetRestarted || !OOMFails(report(r.Status), expected()) {
		t.Fatalf("reason %q; want Restarted and the request failed", r.Reason)
	}
	if r.Status.PodsCovered != 0 || !equality.Semantic.DeepEqual(r.Status.Baselines, st.Baselines) {
		t.Fatalf("covered %d, baselines changed; want 0 and the baselines kept", r.Status.PodsCovered)
	}
}

// A container whose current termination has no finish time is no evidence: the sample is incomplete.
func TestTerminationWithoutFinishTimeIsIncomplete(t *testing.T) {
	st := samples(t, 2)
	ps := pods(3)
	ps[2].Status.ContainerStatuses[1].State.Terminated = &corev1.ContainerStateTerminated{Reason: reasonError, ExitCode: 1}
	if r := sample(t, spec(), st, input(st.SampledAt.Add(PollInterval), 3, ps)); r.Reason != v1alpha1.ResetEvidenceMissing {
		t.Fatalf("reason %q, want EvidenceMissing", r.Reason)
	}
}

// TestReplicaSetNamesCoverEverySampleRead: ReplicaSetNames names every ReplicaSet Sample reads, each once, so a
// caller can read them all before it takes the sample time; a terminating pod and a pod of another namespace
// add no name, and a target scaled to zero or of another kind needs none.
func TestReplicaSetNamesCoverEverySampleRead(t *testing.T) {
	// Pods 0 to 2 are of the rollout, 3 of the old ReplicaSet, 4 terminating, 5 in another namespace.
	ps := pods(6)
	ps[3].OwnerReferences = controllerRef("ReplicaSet", oldRSName, oldRSUID)
	ps[4].OwnerReferences = controllerRef("ReplicaSet", "terminating-rs", "c0000000-0000-4000-8000-000000000001")
	ps[4].DeletionTimestamp = mt(time.Now())
	ps[5].Namespace = "elsewhere"
	ps[5].OwnerReferences = controllerRef("ReplicaSet", "elsewhere-rs", "c0000000-0000-4000-8000-000000000002")

	in := input(time.Now(), 3, ps)
	names := ReplicaSetNames(spec(), in)
	if want := []string{rsName, oldRSName}; !slices.Equal(names, want) {
		t.Errorf("ReplicaSetNames = %v, want %v", names, want)
	}
	read := map[string]bool{}
	get := in.ReplicaSets
	in.ReplicaSets = func(name string) (*appsv1.ReplicaSet, error) {
		read[name] = true
		return get(name)
	}
	sample(t, spec(), v1alpha1.VerificationReportStatus{}, in)
	for name := range read {
		if !slices.Contains(names, name) {
			t.Errorf("Sample read ReplicaSet %q, which ReplicaSetNames does not name", name)
		}
	}

	if got := ReplicaSetNames(spec(), input(time.Now(), 0, ps)); got != nil {
		t.Errorf("ReplicaSetNames for a target scaled to zero = %v, want none", got)
	}
	sts := input(time.Now(), 3, ps)
	sts.Target = &appsv1.StatefulSet{ObjectMeta: deployment(3).ObjectMeta}
	if got := ReplicaSetNames(spec(), sts); got != nil {
		t.Errorf("ReplicaSetNames for a StatefulSet = %v, want none", got)
	}
}
