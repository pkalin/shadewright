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
	res, err := Evaluate(cacheStatefulSet(), applied, applied.Add(DefaultDeadline), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != Stuck || res.Reason != ReasonRolloutStuck {
		t.Fatalf("got %v/%s, want Stuck/%s", res.State, res.Reason, ReasonRolloutStuck)
	}
	if !slices.Equal(res.OldPods, []string{blockedPod, "cache-0"}) {
		t.Errorf("old pods %v, want [cache-1 cache-0]", res.OldPods)
	}
	for _, want := range []string{blockedPod, "delete cache-1 yourself", "does not delete pods", strconv.Quote(newRevision), "10m0s"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message %q lacks %q", res.Message, want)
		}
	}
	c := res.Condition(4, applied)
	if c.Type != ConditionRolledOut || c.Status != metav1.ConditionFalse || c.Reason != ReasonRolloutStuck ||
		c.ObservedGeneration != 4 {
		t.Errorf("condition %+v, want RolledOut=False/RolloutStuck at generation 4", c)
	}
}

func TestStuckOnlyAfterTheDeadline(t *testing.T) {
	res, err := Evaluate(cacheStatefulSet(), applied, applied.Add(DefaultDeadline-time.Second), 0)
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
	if res, _ = Evaluate(cacheStatefulSet(), applied, applied.Add(2*time.Minute), 2*time.Minute); res.State != Stuck {
		t.Errorf("deadline 2m after 2m: got %v, want Stuck", res.State)
	}
	if res, _ = Evaluate(cacheStatefulSet(), applied, applied.Add(MaxDeadline), 5*time.Hour); res.State != Stuck {
		t.Errorf("deadline 5h is clamped to 1h: got %v, want Stuck", res.State)
	}
}

func TestCompletedRolloutReportsNothing(t *testing.T) {
	ss := cacheStatefulSet()
	ss.Status = appsv1.StatefulSetStatus{ObservedGeneration: 3, Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2,
		CurrentReplicas: 2, CurrentRevision: newRevision, UpdateRevision: newRevision}
	for _, obj := range []client.Object{ss, deployment3()} {
		res, err := Evaluate(obj, applied, applied.Add(time.Hour), 0)
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
		}, Stuck, []string{blockedPod, "cache-0"}},
		"partition reached": {func(s *appsv1.StatefulSet) {
			s.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](1)}
			s.Status.UpdatedReplicas = 1
		}, Complete, nil},
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
		res, err := Evaluate(ss, applied, applied.Add(DefaultDeadline), 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.State != tc.state || !slices.Equal(res.OldPods, tc.pods) {
			t.Errorf("%s: got %v %v (%s), want %v %v", name, res.State, res.OldPods, res.Message, tc.state, tc.pods)
		}
	}
	ss := cacheStatefulSet()
	ss.Spec.Replicas = ptr.To[int32](30)
	if res, _ := Evaluate(ss, applied, applied.Add(DefaultDeadline), 0); !strings.Contains(res.Message, "and 20 more") {
		t.Errorf("message %q does not count the pods it does not name", res.Message)
	}
	ss = cacheStatefulSet()
	ss.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
	res, _ := Evaluate(ss, applied, applied.Add(DefaultDeadline), 0)
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
		res, err := Evaluate(d, applied, applied.Add(DefaultDeadline), 0)
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
		res, err := Evaluate(tc.obj, tc.at, applied.Add(time.Hour), 0)
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
