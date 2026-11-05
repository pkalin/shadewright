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

package rbac

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	analyzerpkg "github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents"
)

// The subject and reason of the observations fed to the Incident controller.
const (
	subjectAPIVersion = "apps/v1"
	subjectKind       = "Deployment"
	reason            = "BackOff"
)

// TestIncidentControllerRunsWithTheAnalyzerRBAC runs the Incident controller as the analyzer's
// ServiceAccount: it must load, create and update Incidents in the operator namespace and, at the cap, write
// its Warning event there as a core Event, with the leader election Role's grant and no other. A missing
// grant shows up as an Incident or an event that never appears.
func TestIncidentControllerRunsWithTheAnalyzerRBAC(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var existing remediationv1alpha1.IncidentList
	if err := admin.List(ctx, &existing, client.InNamespace(operatorNS)); err != nil {
		t.Fatal(err)
	}

	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{UserName: user(analyzer), Groups: saGroups}
	mgr, err := ctrl.NewManager(c, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  analyzerpkg.CacheOptions(operatorNS, []string{otherNS}),
	})
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: "analyzer-rbac-test"},
	}
	inc, err := incidents.New(incidents.Config{
		Namespace: operatorNS, MaxIncidents: len(existing.Items) + 1, Window: 100 * time.Millisecond,
		Recorder: mgr.GetEventRecorderFor("shadewright-analyzer"), Regarding: pod, //nolint:staticcheck // as cmd/analyzer
	})
	if err != nil {
		t.Fatal(err)
	}
	obs := make(chan collector.Observation, 4)
	if err := inc.SetupWithManager(mgr, obs); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	observation := func(f, i int, total, count int64) collector.Observation {
		now := time.Now().Truncate(time.Second)
		return collector.Observation{
			Fingerprint: fmt.Sprintf("sha256:%064x", 0xfeed00+f),
			Subject: remediationv1alpha1.ObjectRef{
				APIVersion: subjectAPIVersion, Kind: subjectKind, Namespace: otherNS, Name: "rbac-orders",
				UID: "44444444-4444-4444-8444-444444444444",
			},
			Reason: reason, Type: corev1.EventTypeWarning, ReportingController: "kubelet",
			Symptom:    "Back-off restarting failed container worker",
			Occurrence: fmt.Sprintf("event:0a1b2c3d-4e5f-4a6b-8c7d-%012d", i),
			Total:      total, Count: count, Began: now, ObservedAt: now,
			// Opens its Incident at its first sighting, as an OOMKilled termination does.
			Immediate: true,
		}
	}
	first := observation(1, 1, 2, 2)
	name := incidents.Name(first.Fingerprint)
	t.Cleanup(func() {
		_ = admin.Delete(context.Background(), &remediationv1alpha1.Incident{
			ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: name},
		})
	})
	obs <- first
	wait := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the Incident controller's reads or writes do not fit the analyzer's RBAC", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	got := &remediationv1alpha1.Incident{}
	countIs := func(n int64) func() bool {
		return func() bool {
			err := admin.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, got)
			return err == nil && got.Status.Count == n
		}
	}
	wait("no Incident created", countIs(2))
	// A second write goes through the status update.
	obs <- observation(1, 2, 3, 3)
	wait("the Incident status was not updated", countIs(5))

	// A new fingerprint at the cap: refused, with a Warning event about the analyzer's Pod.
	obs <- observation(2, 3, 1, 1)
	wait("no Warning event at the cap", func() bool {
		var events corev1.EventList
		if err := admin.List(ctx, &events, client.InNamespace(operatorNS)); err != nil {
			return false
		}
		for _, e := range events.Items {
			if e.Reason == "IncidentCapReached" && e.InvolvedObject.Name == pod.Name {
				return true
			}
		}
		return false
	})
}
