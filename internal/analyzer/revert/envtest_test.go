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

package revert

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

var env struct {
	once    sync.Once
	err     error
	testEnv *envtest.Environment
	scheme  *runtime.Scheme
	admin   client.Client
}

func TestMain(m *testing.M) {
	code := m.Run()
	if env.testEnv != nil {
		_ = env.testEnv.Stop()
	}
	os.Exit(code)
}

func firstEnvtestBinaryDir() string {
	base := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

func startEnv(t *testing.T) {
	t.Helper()
	env.once.Do(func() {
		env.scheme = runtime.NewScheme()
		if env.err = clientgoscheme.AddToScheme(env.scheme); env.err != nil {
			return
		}
		if env.err = remediationv1alpha1.AddToScheme(env.scheme); env.err != nil {
			return
		}
		env.testEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: firstEnvtestBinaryDir(),
		}
		cfg, err := env.testEnv.Start()
		if err != nil {
			env.err = err
			return
		}
		env.admin, env.err = client.New(cfg, client.Options{Scheme: env.scheme})
	})
	if env.err != nil {
		t.Fatalf("starting envtest: %v", env.err)
	}
}

// startProposer runs the proposer, registered with SetupWithManager, in a manager of its own with the analyzer's
// cache options, for the operator namespace namespace, until the test ends.
func startProposer(t *testing.T, namespace string) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.testEnv.Config, ctrl.Options{
		Scheme:                 env.scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Each test runs its own manager with the same controller names.
		Controller: config.Controller{SkipNameValidation: new(true)},
		Cache:      analyzer.CacheOptions(namespace, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &Proposer{Client: mgr.GetClient(), Namespace: namespace}
	if err := p.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("the manager: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// eventually polls cond for up to 20 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// entries returns the status.revertsProposed of the Incident inc, read from the API server.
func entries(t *testing.T, inc *remediationv1alpha1.Incident) map[remediationv1alpha1.UID]remediationv1alpha1.ProposedRevert {
	t.Helper()
	got := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(inc), got); err != nil {
		t.Fatal(err)
	}
	out := map[remediationv1alpha1.UID]remediationv1alpha1.ProposedRevert{}
	for _, e := range got.Status.RevertsProposed {
		out[e.UID] = e
	}
	return out
}

func pendingEntry(name string, uid remediationv1alpha1.UID) remediationv1alpha1.ProposedRevert {
	return remediationv1alpha1.ProposedRevert{RequestRef: remediationv1alpha1.RequestRef{Name: name, UID: uid},
		State: remediationv1alpha1.RevertPending}
}

// addEntries adds es to the status.revertsProposed of inc, read again first.
func addEntries(t *testing.T, inc *remediationv1alpha1.Incident, es ...remediationv1alpha1.ProposedRevert) {
	t.Helper()
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(inc), inc); err != nil {
		t.Fatal(err)
	}
	inc.Status.Phase = remediationv1alpha1.IncidentEscalated
	inc.Status.RevertsProposed = append(inc.Status.RevertsProposed, es...)
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
}

// TestEnvtestPendingSettledFromTheIncident: Pending entries no event about their request will name again are
// settled from the Incident's side, by the controller SetupWithManager registers. At start-up: an entry whose
// request is gone, and one whose request name now belongs to another UID, move to Abandoned (RequestGone); one
// whose request is gone but whose linked revert exists moves to Created; one whose request is there with its
// UID stays Pending. Later, an entry of a gone request added to a running analyzer's Incident is settled too.
func TestEnvtestPendingSettledFromTheIncident(t *testing.T) {
	startEnv(t)
	ctx := t.Context()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "revert-"}}
	if err := env.admin.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	namespace := ns.Name

	inc := fx.Incident(namespace, incName)
	if err := env.admin.Create(ctx, inc); err != nil {
		t.Fatal(err)
	}
	live := fx.MemoryLimitRequest(namespace, "remediation-live")
	live.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)}
	reused := fx.MemoryLimitRequest(namespace, "remediation-reused")
	reused.Spec.IncidentRef = live.Spec.IncidentRef
	for _, rr := range []*remediationv1alpha1.RemediationRequest{live, reused} {
		if err := env.admin.Create(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	const (
		goneUID   = "41414141-4141-4141-8141-414141414141"
		reusedUID = "42424242-4242-4242-8242-424242424242"
		madeUID   = "43434343-4343-4343-8343-434343434343"
		laterUID  = "44444444-4444-4444-8444-444444444444"
	)
	// The revert of remediation-made exists: its create succeeded, its move to Created was lost, and then the
	// request was deleted.
	made := fx.MemoryLimitRequest(namespace, "remediation-made")
	made.UID = madeUID
	made.Spec.IncidentRef = live.Spec.IncidentRef
	spec, ok := Spec(made)
	if !ok {
		t.Fatal("the fixture's action cannot be inverted")
	}
	if err := env.admin.Create(ctx, &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: string(remediationv1alpha1.RevertName(madeUID))}, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	addEntries(t, inc,
		pendingEntry("remediation-gone", goneUID),
		pendingEntry(reused.Name, reusedUID),
		pendingEntry(made.Name, madeUID),
		pendingEntry(live.Name, remediationv1alpha1.UID(live.UID)))

	startProposer(t, namespace)

	abandoned := func(uid remediationv1alpha1.UID) bool {
		e := entries(t, inc)[uid]
		return e.State == remediationv1alpha1.RevertAbandoned && e.Reason == remediationv1alpha1.RevertReasonRequestGone
	}
	eventually(t, "the entries of the gone and the reused request are Abandoned", func() bool {
		return abandoned(goneUID) && abandoned(reusedUID)
	})
	eventually(t, "the entry whose revert exists is Created", func() bool {
		return entries(t, inc)[madeUID].State == remediationv1alpha1.RevertCreated
	})
	if e := entries(t, inc)[remediationv1alpha1.UID(live.UID)]; e.State != remediationv1alpha1.RevertPending {
		t.Errorf("the entry of a request that is there is %+v, want Pending", e)
	}

	addEntries(t, inc, pendingEntry("remediation-later", laterUID))
	eventually(t, "the entry added later is Abandoned", func() bool { return abandoned(laterUID) })
}
