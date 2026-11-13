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

package rollout

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// oldestPod is the pod with the lowest ordinal of the StatefulSet in these tests.
const oldestPod = "cache-0"

const (
	newRevision = "cache-7b9c"
	blockedPod  = "cache-1"
)

var applied = time.Date(2026, 11, 8, 10, 36, 0, 0, time.UTC)

// cacheStatefulSet is the lab's statefulset-oom scenario after the fix was applied: the template moved to a new
// revision, and cache-1 stays on the old one in CrashLoopBackOff (forced rollback), so nothing is updated.
func cacheStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "demo", Generation: 3},
		Spec: appsv1.StatefulSetSpec{
			Replicas:       ptr.To[int32](2),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 3, Replicas: 2, ReadyReplicas: 1, CurrentReplicas: 2, UpdatedReplicas: 0,
			CurrentRevision: "cache-6d4f", UpdateRevision: newRevision,
		},
	}
}

func deployment3() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Generation: 5},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](3)},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 5, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3,
			AvailableReplicas: 3},
	}
}

func TestStatefulSetStuckPastDeadlineNamesThePod(t *testing.T) {
	res, err := Evaluate(cacheStatefulSet(), "", applied, applied.Add(DefaultDeadline), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != Stuck || res.Reason != ReasonRolloutStuck {
		t.Fatalf("got %v/%s, want Stuck/%s", res.State, res.Reason, ReasonRolloutStuck)
	}
	if !slices.Equal(res.OldPods, []string{blockedPod, oldestPod}) {
		t.Errorf("old pods %v, want [cache-1 cache-0]", res.OldPods)
	}
	for _, want := range []string{blockedPod, "1 of 2 pods are not Ready", "never a Ready one", "does not delete pods",
		strconv.Quote(newRevision), "10m0s"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message %q lacks %q", res.Message, want)
		}
	}
	// Which of the two pods is not Ready cannot be told from the StatefulSet status, so none is named for deletion.
	if strings.Contains(res.Message, "delete cache-1") || strings.Contains(res.Message, "delete cache-0") {
		t.Errorf("message %q names a pod to delete that may be Ready", res.Message)
	}
	c := res.Condition(4, applied)
	if c.Type != ConditionRolledOut || c.Status != metav1.ConditionFalse || c.Reason != ReasonRolloutStuck ||
		c.ObservedGeneration != 4 {
		t.Errorf("condition %+v, want RolledOut=False/RolloutStuck at generation 4", c)
	}
}

func TestStuckOnlyAfterTheDeadline(t *testing.T) {
	res, err := Evaluate(cacheStatefulSet(), "", applied, applied.Add(DefaultDeadline-time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != Progressing || res.Reason != ReasonRolloutProgressing || len(res.OldPods) != 0 {
		t.Errorf("got %+v, want Progressing without pods", res)
	}
	if c := res.Condition(1, applied); c.Status != metav1.ConditionUnknown {
		t.Errorf("condition status %s, want Unknown", c.Status)
	}
	// A configured deadline is used, within bounds.
	if res, _ = Evaluate(cacheStatefulSet(), "", applied, applied.Add(2*time.Minute), 2*time.Minute); res.State != Stuck {
		t.Errorf("deadline 2m after 2m: got %v, want Stuck", res.State)
	}
	if res, _ = Evaluate(cacheStatefulSet(), "", applied, applied.Add(MaxDeadline), 5*time.Hour); res.State != Stuck {
		t.Errorf("deadline 5h is clamped to 1h: got %v, want Stuck", res.State)
	}
}

func TestCompletedRolloutReportsNothing(t *testing.T) {
	ss := cacheStatefulSet()
	ss.Status = appsv1.StatefulSetStatus{ObservedGeneration: 3, Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2,
		CurrentReplicas: 2, CurrentRevision: newRevision, UpdateRevision: newRevision}
	for _, obj := range []client.Object{ss, deployment3()} {
		res, err := Evaluate(obj, "", applied, applied.Add(time.Hour), 0)
		if err != nil {
			t.Fatalf("%T: %v", obj, err)
		}
		if res.State != Complete || res.Reason != ReasonRolloutComplete || len(res.OldPods) != 0 {
			t.Errorf("%T: got %+v, want Complete", obj, res)
		}
		if c := res.Condition(1, applied); c.Status != metav1.ConditionTrue {
			t.Errorf("%T: condition %s, want True", obj, c.Status)
		}
	}
}

func TestStatefulSetCases(t *testing.T) {
	cases := map[string]struct {
		edit  func(*appsv1.StatefulSet)
		state State
		pods  []string
	}{
		"revisions equal but a pod not ready": {func(s *appsv1.StatefulSet) {
			s.Status.CurrentRevision, s.Status.UpdatedReplicas = s.Status.UpdateRevision, 2
		}, Stuck, nil},
		"generation not observed": {func(s *appsv1.StatefulSet) {
			s.Generation = 4
			s.Status = appsv1.StatefulSetStatus{ObservedGeneration: 3, ReadyReplicas: 2, UpdatedReplicas: 2}
		}, Stuck, nil},
		"one of three updated": {func(s *appsv1.StatefulSet) {
			s.Spec.Replicas = ptr.To[int32](3)
			s.Status.UpdatedReplicas = 1
		}, Stuck, []string{blockedPod, oldestPod}},
		"partition reached": {func(s *appsv1.StatefulSet) {
			s.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](1)}
			s.Status.UpdatedReplicas, s.Status.ReadyReplicas = 1, 2
		}, Complete, []string{oldestPod}},
		"partition reached, a pod not ready": {func(s *appsv1.StatefulSet) {
			s.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](1)}
			s.Status.UpdatedReplicas = 1
		}, Stuck, nil},
		"partition not reached": {func(s *appsv1.StatefulSet) {
			s.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](1)}
		}, Stuck, []string{blockedPod}},
		"on delete": {func(s *appsv1.StatefulSet) {
			s.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
		}, Stuck, nil},
		"status beyond spec is bounded": {func(s *appsv1.StatefulSet) {
			s.Status.UpdatedReplicas = 99
		}, Stuck, nil},
		"many pods are capped": {func(s *appsv1.StatefulSet) {
			s.Spec.Replicas = ptr.To[int32](30)
		}, Stuck, []string{"cache-29", "cache-28", "cache-27", "cache-26", "cache-25", "cache-24", "cache-23",
			"cache-22", "cache-21", "cache-20"}},
	}
	for name, tc := range cases {
		ss := cacheStatefulSet()
		tc.edit(ss)
		res, err := Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.State != tc.state || !slices.Equal(res.OldPods, tc.pods) {
			t.Errorf("%s: got %v %v (%s), want %v %v", name, res.State, res.OldPods, res.Message, tc.state, tc.pods)
		}
	}
	ss := cacheStatefulSet()
	ss.Spec.Replicas = ptr.To[int32](30)
	if res, _ := Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0); !strings.Contains(res.Message, "and 20 more") {
		t.Errorf("message %q does not count the pods it does not name", res.Message)
	}
	ss = cacheStatefulSet()
	ss.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
	res, _ := Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0)
	if !strings.Contains(res.Message, "OnDelete") || !strings.Contains(res.Message, "does not delete pods") {
		t.Errorf("OnDelete message %q lacks the manual step", res.Message)
	}
}

func TestDeploymentCases(t *testing.T) {
	cases := map[string]struct {
		edit func(*appsv1.Deployment)
		want string
	}{
		"not observed":    {func(d *appsv1.Deployment) { d.Status.ObservedGeneration = 4 }, "not observed"},
		"not updated":     {func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 1 }, "1 of 3 pods run the new"},
		"old pods remain": {func(d *appsv1.Deployment) { d.Status.Replicas = 4 }, "1 pods still run the old"},
		"not available":   {func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 2 }, "2 of 3 updated pods"},
		"replicas unset": {func(d *appsv1.Deployment) {
			d.Spec.Replicas, d.Status.UpdatedReplicas = nil, 0
		}, "0 of 1 pods"},
	}
	for name, tc := range cases {
		d := deployment3()
		tc.edit(d)
		res, err := Evaluate(d, "", applied, applied.Add(DefaultDeadline), 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.State != Stuck || !strings.Contains(res.Message, tc.want) || len(res.OldPods) != 0 {
			t.Errorf("%s: got %v %q, want Stuck with %q", name, res.State, res.Message, tc.want)
		}
	}
}

// Input that cannot be judged is an error, never Complete: the caller must not report a rollout as done.
func TestNotEvaluableFailsClosed(t *testing.T) {
	cases := map[string]struct {
		obj client.Object
		at  time.Time
	}{
		"no applied time":  {cacheStatefulSet(), time.Time{}},
		"nil statefulset":  {(*appsv1.StatefulSet)(nil), applied},
		"nil deployment":   {(*appsv1.Deployment)(nil), applied},
		"unsupported kind": {&corev1.Pod{}, applied},
		"nil interface":    {nil, applied},
	}
	for name, tc := range cases {
		res, err := Evaluate(tc.obj, "", tc.at, applied.Add(time.Hour), 0)
		if !errors.Is(err, ErrNotEvaluable) || res.State == Complete {
			t.Errorf("%s: got %+v, %v, want ErrNotEvaluable", name, res, err)
		}
	}
}

func TestClampDeadline(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0: DefaultDeadline, -time.Second: DefaultDeadline, time.Second: MinDeadline,
		5 * time.Minute: 5 * time.Minute, 2 * time.Hour: MaxDeadline,
	} {
		if got := ClampDeadline(in); got != want {
			t.Errorf("ClampDeadline(%s) = %s, want %s", in, got, want)
		}
	}
}

// Review of #532: a partitioned StatefulSet is complete only when its pods are ready.
func TestPartitionNeedsReadyPods(t *testing.T) {
	ss := cacheStatefulSet()
	ss.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](1)}
	ss.Status.UpdatedReplicas, ss.Status.ReadyReplicas = 1, 0
	if res, _ := Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0); res.State == Complete {
		t.Fatalf("partition with no ready pod is %v, want not Complete", res.State)
	}
}

// An OnDelete StatefulSet never promotes its current revision, so it is judged by updated and ready pods.
func TestOnDeleteCompletedByHand(t *testing.T) {
	ss := cacheStatefulSet()
	ss.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
	ss.Status.UpdatedReplicas, ss.Status.ReadyReplicas = 2, 2
	if res, _ := Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0); res.State != Complete {
		t.Fatalf("OnDelete with every pod updated and ready is %v (%s), want Complete", res.State, res.Message)
	}
	ss.Status.ReadyReplicas = 1
	if res, _ := Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0); res.State != Stuck {
		t.Fatalf("OnDelete with a pod not ready is %v, want Stuck", res.State)
	}
}

// A newer template revision than the one recorded ends the check as superseded, never Stuck.
func TestNewerRevisionSupersedes(t *testing.T) {
	ss := cacheStatefulSet()
	res, err := Evaluate(ss, "cache-6aaa", applied, applied.Add(DefaultDeadline), 0)
	if err != nil || res.State != Superseded || res.Reason != ReasonRolloutSuperseded {
		t.Fatalf("got %+v, %v, want Superseded", res, err)
	}
	if c := res.Condition(1, applied); c.Status != metav1.ConditionUnknown {
		t.Errorf("condition %s, want Unknown", c.Status)
	}
	if res, _ := Evaluate(ss, newRevision, applied, applied.Add(DefaultDeadline), 0); res.State != Stuck {
		t.Errorf("the recorded revision is judged: got %v, want Stuck", res.State)
	}
	d := deployment3()
	d.Annotations = map[string]string{DeploymentRevisionAnnotation: "4"}
	d.Status.UpdatedReplicas = 1
	if res, _ := Evaluate(d, "3", applied, applied.Add(DefaultDeadline), 0); res.State != Superseded {
		t.Errorf("Deployment revision 4 over recorded 3: got %v, want Superseded", res.State)
	}
}

func TestRevision(t *testing.T) {
	ss := cacheStatefulSet()
	if got, ok := Revision(ss, 3); !ok || got != newRevision {
		t.Errorf("Revision = %q, %v, want %q", got, ok, newRevision)
	}
	if _, ok := Revision(ss, 4); ok {
		t.Error("a revision before the controller observed the generation")
	}
	d := deployment3()
	if _, ok := Revision(d, 5); ok {
		t.Error("a Deployment without the revision annotation has none")
	}
	d.Annotations = map[string]string{DeploymentRevisionAnnotation: "7"}
	if got, ok := Revision(d, 5); !ok || got != "7" {
		t.Errorf("Revision = %q, %v, want 7", got, ok)
	}
}

// The pod to delete is named only when it is certainly not Ready and on the old revision, with ordinals.start
// honoured; a failing new template is said, and a Ready pod is never offered for deletion.
func TestBlockingPod(t *testing.T) {
	ss := cacheStatefulSet()
	ss.Spec.Ordinals = &appsv1.StatefulSetOrdinals{Start: 5}
	ss.Status.ReadyReplicas = 0
	res, _ := Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0)
	if !strings.Contains(res.Message, "delete cache-5 yourself") || !slices.Equal(res.OldPods, []string{"cache-6", "cache-5"}) {
		t.Errorf("got %v %q, want cache-5 to delete and old pods [cache-6 cache-5]", res.OldPods, res.Message)
	}
	ss = cacheStatefulSet()
	ss.Status.UpdatedReplicas, ss.Status.ReadyReplicas = 1, 1 // cache-1 on the new revision crashes, cache-0 is Ready
	res, _ = Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0)
	if strings.Contains(res.Message, "delete cache-0") || !strings.Contains(res.Message, "new template") {
		t.Errorf("message %q offers a Ready pod or misses the failing new template", res.Message)
	}
}

// TemplateHash changes with the pod template only: a scale keeps it, a new image changes it, and a kind without
// a template has none.
func TestTemplateHash(t *testing.T) {
	d := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](2),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1"}}}}}}
	h := TemplateHash(d)
	if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		t.Fatalf("TemplateHash = %q, want 64 lower-case hex digits", h)
	}
	scaled := d.DeepCopy()
	scaled.Spec.Replicas = ptr.To[int32](5)
	if got := TemplateHash(scaled); got != h {
		t.Errorf("a scale changed the hash: %s != %s", got, h)
	}
	changed := d.DeepCopy()
	changed.Spec.Template.Spec.Containers[0].Image = "app:2"
	if got := TemplateHash(changed); got == h {
		t.Error("a new image kept the hash")
	}
	ss := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Template: d.Spec.Template}}
	if got := TemplateHash(ss); got != h {
		t.Errorf("the same template in a StatefulSet hashed %s, want %s", got, h)
	}
	if got := TemplateHash(&corev1.ConfigMap{}); got != "" {
		t.Errorf("TemplateHash(ConfigMap) = %q, want none", got)
	}
	if got := ObservedGeneration(&appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{ObservedGeneration: 4}}); got != 4 {
		t.Errorf("ObservedGeneration = %d, want 4", got)
	}
}

// ADR-0030 section 3: a rolling update of three replicas with partition 2 and one updated pod is complete as
// the strategy asks, with reason RolloutPartitioned (RolledOut True) and a message naming the two pods left on
// the old revision, not "every pod runs the current template".
func TestPartitionedStatefulSetNamesTheOldPods(t *testing.T) {
	ss := cacheStatefulSet()
	ss.Spec.Replicas = ptr.To[int32](3)
	ss.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2)}
	ss.Status.Replicas, ss.Status.UpdatedReplicas, ss.Status.ReadyReplicas = 3, 1, 3
	res, err := Evaluate(ss, "", applied, applied.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != Complete || res.Reason != ReasonRolloutPartitioned ||
		!slices.Equal(res.OldPods, []string{"cache-1", oldestPod}) {
		t.Fatalf("got %+v, want Complete, RolloutPartitioned, old pods cache-1 and cache-0", res)
	}
	if !strings.Contains(res.Message, "cache-1, cache-0") || strings.Contains(res.Message, "Every pod") {
		t.Errorf("message %q does not name the pods on the old revision", res.Message)
	}
	if c := res.Condition(1, applied); c.Status != metav1.ConditionTrue || c.Reason != ReasonRolloutPartitioned {
		t.Errorf("condition %s/%s, want True/RolloutPartitioned", c.Status, c.Reason)
	}

	// Once the partition is lowered and every pod is updated, the rollout is plainly complete.
	ss.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](0)
	ss.Status.UpdatedReplicas, ss.Status.CurrentRevision = 3, ss.Status.UpdateRevision
	if res, _ := Evaluate(ss, "", applied, applied.Add(time.Minute), 0); res.Reason != ReasonRolloutComplete {
		t.Errorf("after the partition was lowered: %+v, want RolloutComplete", res)
	}
}

// TestStatefulSetMessageCapsRevisionNames: the revision names a StatefulSet reports go into the RolledOut message
// and its Event only cut to maxRevisionInMessage bytes, whatever their length.
func TestStatefulSetMessageCapsRevisionNames(t *testing.T) {
	long := strings.Repeat("r", 4000)
	ss := cacheStatefulSet()
	ss.Status.CurrentRevision = "old-" + long
	ss.Status.UpdateRevision = "new-" + long
	res, err := Evaluate(ss, "", applied, applied.Add(DefaultDeadline), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != Stuck {
		t.Fatalf("got %v, want Stuck", res.State)
	}
	if strings.Contains(res.Message, long[:200]) {
		t.Errorf("message carries more than %d bytes of a revision name (%d bytes)", 200,
			len(res.Message))
	}
	if !strings.Contains(res.Message, "old-rrr") || !strings.Contains(res.Message, "...") {
		t.Error("message does not name the cut current revision")
	}
	if len(res.Message) > 2000 {
		t.Errorf("message is %d bytes", len(res.Message))
	}
}
