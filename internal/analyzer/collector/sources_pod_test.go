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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/cloudflared"
)

const (
	containerTunnel = "cloudflared"
	reasonTunnel    = cloudflared.ReasonTunnelDown
)

// cloudflaredSelection returns the modules active with the cloudflared module compiled in and detected (it is
// detected by the core group version), enabled or not.
func cloudflaredSelection(t *testing.T, enabled bool) modules.Selection {
	t.Helper()
	r, err := modules.New(nil, []modules.Integration{cloudflared.Module()})
	if err != nil {
		t.Fatal(err)
	}
	var e modules.Enabled
	if enabled {
		e.Integrations = []string{cloudflared.Name}
	}
	return r.Select(e, r.Detect(nil, []string{cloudflared.CoreVersion}))
}

// tunnelWorkload is the web Deployment, its ReplicaSet and a running pod of it whose one container runs
// Cloudflare's cloudflared image and is not ready.
func tunnelWorkload() (*appsv1.Deployment, *appsv1.ReplicaSet, *corev1.Pod) {
	d, rs, pod := workload()
	pod.Spec.Containers = []corev1.Container{{Name: containerTunnel, Image: "cloudflare/cloudflared:2026.10.7"}}
	setTunnelReady(pod, false)
	return d, rs, pod
}

// setTunnelReady sets the pod's status: running, its cloudflared container ready or not.
func setTunnelReady(pod *corev1.Pod, ready bool) {
	cond := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}
	if !ready {
		cond = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}
	}
	pod.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{cond},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: containerTunnel, Ready: ready, RestartCount: 2,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
}

// podSourceHarness is the harness with a source reconciler for Pods, sharing its client and owner resolver.
type podSourceHarness struct {
	*harness
	r   *sourceReconciler
	sel modules.Selection
}

func newPodSourceHarness(t *testing.T, sel modules.Selection, objs ...corev1.Pod) *podSourceHarness {
	t.Helper()
	d, rs, _ := tunnelWorkload()
	h := &podSourceHarness{sel: sel}
	h.harness = newHarness(t, Config{Modules: func() modules.Selection { return h.sel }}, d, rs)
	for i := range objs {
		h.create(&objs[i])
	}
	h.r = &sourceReconciler{c: h.c, reader: h.cl, api: h.cl, gvk: sourcePod}
	return h
}

func (h *podSourceHarness) reconcile() time.Duration {
	h.t.Helper()
	res, err := h.r.Reconcile(h.t.Context(), reqFor(webPod))
	if err != nil {
		h.t.Fatalf("pod source reconcile: %v", err)
	}
	return res.RequeueAfter
}

// TestTunnelDownIsAnIncidentAboutTheDeployment is the cloudflared criterion of #409: a cloudflared container
// that is not ready gives, once the state has lasted SettleAfter, one observation about the Deployment the pod
// belongs to, with the fingerprint on the Deployment's UID and the module's fixed summary.
func TestTunnelDownIsAnIncidentAboutTheDeployment(t *testing.T) {
	_, _, pod := tunnelWorkload()
	h := newPodSourceHarness(t, cloudflaredSelection(t, true), *pod)

	// First seen: nothing yet, and a look again once the state has lasted SettleAfter.
	if wait := h.reconcile(); wait != SettleAfter {
		t.Errorf("requeue after %v, want %v", wait, SettleAfter)
	}
	h.none()
	h.clock.add(time.Minute)
	if wait := h.reconcile(); wait != time.Minute {
		t.Errorf("requeue after %v, want the minute left", wait)
	}
	h.none()

	h.clock.add(time.Minute)
	h.reconcile()
	o := h.one()
	want := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindDeployment, Namespace: testNS, Name: web,
		UID: remediationv1alpha1.UID(deployUID)}
	if o.Subject != want {
		t.Errorf("subject %+v, want %+v", o.Subject, want)
	}
	if o.Reason != reasonTunnel || o.ReportingController != reportingKubelet || o.Source != testNS || !o.Immediate {
		t.Errorf("reason %q, controller %q, source %q, immediate %v", o.Reason, o.ReportingController, o.Source,
			o.Immediate)
	}
	if !strings.HasPrefix(o.Occurrence, occurrenceCondition) || o.Total != 1 {
		t.Errorf("occurrence %q total %d", o.Occurrence, o.Total)
	}
	for _, part := range []string{"cloudflared container is not ready", "spec.containers.name=cloudflared",
		"status.conditions.Ready.reason=ContainersNotReady"} {
		if !strings.Contains(o.Symptom, part) {
			t.Errorf("symptom %q does not hold %q", o.Symptom, part)
		}
	}
	summary := modules.EvaluateSource(cloudflared.Module().Hooks().Symptoms[0], mustContent(t, h.r, pod))[0].Summary
	fp := fingerprint.Fingerprint(fingerprint.Observation{ClusterID: testCluster, Namespace: testNS,
		OwnerUID: string(deployUID), Reason: reasonTunnel, Type: corev1.EventTypeWarning,
		ReportingController: reportingKubelet, Note: summary})
	if o.Fingerprint != fp {
		t.Errorf("fingerprint %s, want %s on the Deployment", o.Fingerprint, fp)
	}

	// The state lasts: counted once.
	h.clock.add(time.Minute)
	h.reconcile()
	h.none()
}

// TestTunnelDownThatEndsSoonOpensNothing checks the start-up limit of design 2.5.6: a cloudflared replica that is
// not ready for less than SettleAfter, as in a rollout, gives no observation, and a later state starts over.
func TestTunnelDownThatEndsSoonOpensNothing(t *testing.T) {
	_, _, pod := tunnelWorkload()
	h := newPodSourceHarness(t, cloudflaredSelection(t, true), *pod)
	h.reconcile()
	h.clock.add(30 * time.Second)
	setTunnelReady(pod, true)
	h.update(pod)
	if wait := h.reconcile(); wait != 0 {
		t.Errorf("requeue after %v with no state left", wait)
	}
	h.clock.add(2 * SettleAfter)
	h.reconcile()
	h.none()

	// Not ready again: a new state, which waits the whole SettleAfter from now.
	setTunnelReady(pod, false)
	h.update(pod)
	if wait := h.reconcile(); wait != SettleAfter {
		t.Errorf("requeue after %v, want %v", wait, SettleAfter)
	}
	h.none()
}

// TestCloudflaredDisabledReadsNothing is the second criterion of #409 for the Pod source: with the cloudflared
// module compiled in and detected but not enabled, or with no module at all, a pod with a cloudflared container
// that is not ready gives nothing, the predicate lets none of its events through and no owner is read.
func TestCloudflaredDisabledReadsNothing(t *testing.T) {
	for name, sel := range map[string]modules.Selection{
		"detected, not enabled": cloudflaredSelection(t, false),
		"no module at all":      {},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, pod := tunnelWorkload()
			h := newPodSourceHarness(t, sel, *pod)
			if h.r.predicate().Create(event.CreateEvent{Object: pod}) {
				t.Error("the predicate let the pod through")
			}
			h.reconcile()
			h.clock.add(SettleAfter)
			if wait := h.reconcile(); wait != 0 {
				t.Errorf("requeue after %v", wait)
			}
			h.none()
			if n := h.gets.Load(); n != 0 {
				t.Errorf("%d owner reads", n)
			}
			if n := h.c.conditions.len(); n != 0 {
				t.Errorf("%d objects tracked", n)
			}
		})
	}
}

// TestHPAStaysImmediate keeps the HPA source as #380 built it: its states are emitted at their first reconcile.
func TestHPAStaysImmediate(t *testing.T) {
	h := newSourceHarness(t, kedaActive(t), Config{})
	h.cache.put(ceilingHPA(t0))
	h.api.put(scaledObject())
	res, err := h.reconcile()
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("result %+v, error %v", res, err)
	}
	h.one()
}

func mustContent(t *testing.T, r *sourceReconciler, pod *corev1.Pod) map[string]any {
	t.Helper()
	m, ok := r.content(pod)
	if !ok {
		t.Fatal("the pod has no content")
	}
	return m
}

// countingPods is an integration with a Pod source whose Symptoms counts its calls and whose PodFilter rules out
// every pod.
type countingPods struct{ calls *atomic.Int32 }

func (countingPods) Name() string { return "counting" }
func (countingPods) Detection() modules.IntegrationDetection {
	return modules.IntegrationDetection{GroupVersions: []string{"v1"}}
}
func (m countingPods) Hooks() modules.Hooks {
	return modules.Hooks{Symptoms: []modules.SymptomSource{{
		Versions: []string{"v1"}, Kind: "Pod", Resource: "pods", ReportingController: reportingKubelet,
		Symptoms: func(map[string]any) []modules.Symptom {
			m.calls.Add(1)
			return []modules.Symptom{{Reason: "Down", Summary: "down"}}
		},
		PodFilter: func(map[string]string, []string) bool { return false },
	}}}
}

// TestPodFilterSparesTheConversion (#409 review): a pod the active source's PodFilter rules out passes no
// create or update through the predicate, and the source's Symptoms is never called on it, so the pod is not
// converted to a map. Without a filter, the source sees it.
func TestPodFilterSparesTheConversion(t *testing.T) {
	calls := &atomic.Int32{}
	r, err := modules.New(nil, []modules.Integration{countingPods{calls: calls}})
	if err != nil {
		t.Fatal(err)
	}
	sel := r.Select(modules.Enabled{Integrations: []string{"counting"}}, r.Detect(nil, []string{"v1"}))
	_, _, pod := tunnelWorkload()
	h := newPodSourceHarness(t, sel, *pod)
	p := h.r.predicate()
	if p.Create(event.CreateEvent{Object: pod}) || p.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: pod}) {
		t.Error("the predicate let a pod the filter rules out through")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("Symptoms called %d times on a pod the filter rules out, want none", n)
	}
}
