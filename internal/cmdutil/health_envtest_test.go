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

package cmdutil_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/cmdutil"
	"github.com/pkalin/shadewright/internal/controller"
	"github.com/pkalin/shadewright/internal/verifier"
)

// The operator namespace of the tests, and the namespace the analyzer watches.
const (
	operatorNS = "default"
	watchedNS  = "kube-public"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(remediationv1alpha1.AddToScheme(scheme))
}

// binary is how one of the three binaries builds its manager: its Lease, its cache and its required kinds.
type binary struct {
	name  string
	lease string
	cache cache.Options
	kinds []client.Object
}

func binaries() []binary {
	watched := []string{watchedNS}
	return []binary{
		{"executor", cmdutil.ExecutorLease, controller.CacheOptions(operatorNS), controller.ReadinessKinds()},
		{"analyzer", cmdutil.AnalyzerLease, analyzer.CacheOptions(operatorNS, watched), analyzer.ReadinessKinds(watched)},
		{"verifier", cmdutil.VerifierLease, verifier.CacheOptions(operatorNS), verifier.ReadinessKinds()},
	}
}

// envtestBinaryDir is the directory make setup-envtest fills, for a run without KUBEBUILDER_ASSETS.
func envtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
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

// startEnv starts an API server with the project's CRDs and none of an add-on (no Flux, KEDA or Kyverno).
func startEnv(t *testing.T) (*envtest.Environment, *rest.Config) {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestBinaryDir(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	return env, cfg
}

// holdLeases creates each binary's Lease, held for an hour by another replica, so a manager never leads.
func holdLeases(t *testing.T, admin client.Client) {
	t.Helper()
	now := metav1.NewMicroTime(time.Now())
	for _, b := range binaries() {
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: b.lease},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       new("another-replica"),
				LeaseDurationSeconds: new(int32(3600)),
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		if err := admin.Create(t.Context(), lease); err != nil {
			t.Fatal(err)
		}
	}
}

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// probe is the health endpoint of a running manager.
type probe struct {
	addr     string
	elected  <-chan struct{}
	ranAsLdr <-chan struct{}
}

// start builds b's manager as its main does (cmdutil.Options with leader election, b's cache, its required
// kinds), with a leader-only runnable that reports whether it ran, runs beforeStart if set, and starts the
// manager until the test ends.
func start(t *testing.T, cfg *rest.Config, b binary, beforeStart func()) probe {
	t.Helper()
	addr := freeAddr(t)
	opts := cmdutil.Options{MetricsAddr: "0", ProbeAddr: addr, EnableLeaderElection: true}
	mgrOpts := opts.ManagerOptions(logr.Discard(), scheme, b.lease)
	mgrOpts.LeaderElectionNamespace = operatorNS
	mgrOpts.Cache = b.cache
	mgr, err := ctrl.NewManager(cfg, mgrOpts)
	if err != nil {
		t.Fatal(err)
	}
	ranAsLeader := make(chan struct{})
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		close(ranAsLeader)
		<-ctx.Done()
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := cmdutil.AddHealthChecks(mgr, b.kinds...); err != nil {
		t.Fatal(err)
	}
	if beforeStart != nil {
		beforeStart()
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-stopped; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	return probe{addr: addr, elected: mgr.Elected(), ranAsLdr: ranAsLeader}
}

func (p probe) get(path string) (int, string) {
	resp, err := http.Get("http://" + p.addr + path) //nolint:noctx // a test against a local listener
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// waitReady waits until /readyz passes.
func (p probe) waitReady(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if code, _ := p.get("/readyz"); code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			code, body := p.get("/readyz?verbose")
			t.Fatalf("/readyz did not pass: %d %s", code, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// staysNotReady checks that, once the probe server answers, /readyz fails for d while /healthz passes.
func (p probe) staysNotReady(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, _ := p.get("/healthz/" + cmdutil.LivenessCheck); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe server never answered /healthz")
		}
		time.Sleep(50 * time.Millisecond)
	}
	until := time.Now().Add(d)
	for time.Now().Before(until) {
		if code, body := p.get("/readyz/" + cmdutil.CacheSyncCheck); code != http.StatusInternalServerError {
			t.Fatalf("/readyz/%s = %d %s, want %d", cmdutil.CacheSyncCheck, code, body, http.StatusInternalServerError)
		}
		if code, body := p.get("/healthz/" + cmdutil.LivenessCheck); code != http.StatusOK {
			t.Fatalf("/healthz/%s = %d %s: liveness must not depend on the API server", cmdutil.LivenessCheck, code, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (p probe) assertStandby(t *testing.T) {
	t.Helper()
	select {
	case <-p.elected:
		t.Fatal("the manager became leader: the test proves nothing about a standby")
	case <-p.ranAsLdr:
		t.Fatal("a leader-only runnable ran: the test proves nothing about a standby")
	default:
	}
}

// TestReadyOnAStandbyWithoutOptionalKinds builds each binary's manager against an API server without any add-on
// CRD (no Flux, KEDA or Kyverno) and with every Lease held by another replica: each turns Ready once its
// required informers have synced, while it stays a standby.
func TestReadyOnAStandbyWithoutOptionalKinds(t *testing.T) {
	_, cfg := startEnv(t)
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	holdLeases(t, admin)
	for _, b := range binaries() {
		t.Run(b.name, func(t *testing.T) {
			p := start(t, cfg, b, nil)
			p.waitReady(t, 30*time.Second)
			if code, body := p.get("/readyz/" + cmdutil.CacheSyncCheck); code != http.StatusOK {
				t.Errorf("/readyz/%s = %d %s", cmdutil.CacheSyncCheck, code, body)
			}
			p.assertStandby(t)
		})
	}
}

// TestNotReadyWhileARequiredKindIsForbidden runs the analyzer's manager as a user that may list and watch every
// required kind but Incidents: it stays not Ready, and turns Ready once the grant is added, without a restart.
func TestNotReadyWhileARequiredKindIsForbidden(t *testing.T) {
	env, cfg := startEnv(t)
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	holdLeases(t, admin)
	const userName = "analyzer-without-incidents"
	user, err := env.AddUser(envtest.User{Name: userName}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	read := []string{"get", "list", "watch"}
	grant(t, admin, "readiness-base", userName, []rbacv1.PolicyRule{
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get"}},
		{APIGroups: []string{remediationv1alpha1.SchemeGroupVersion.Group},
			Resources: []string{"remediationrequests", "remediationsettings"}, Verbs: read},
		{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: read},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: read},
	})
	var b binary
	for _, c := range binaries() {
		if c.name == "analyzer" {
			b = c
		}
	}
	p := start(t, user.Config(), b, nil)
	p.staysNotReady(t, 5*time.Second)

	grant(t, admin, "readiness-incidents", userName, []rbacv1.PolicyRule{
		{APIGroups: []string{remediationv1alpha1.SchemeGroupVersion.Group}, Resources: []string{"incidents"}, Verbs: read},
	})
	p.waitReady(t, 60*time.Second)
	p.assertStandby(t)
}

// grant binds a new ClusterRole with rules to the user.
func grant(t *testing.T, admin client.Client, name, user string, rules []rbacv1.PolicyRule) {
	t.Helper()
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
	}
	for _, o := range []client.Object{role, binding} {
		if err := admin.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNotReadyWithoutAnAPIServer builds each binary's manager, which needs the API server to map its cached
// kinds, then stops the API server and starts the manager: the probe server answers, liveness passes and
// readiness fails. (With no API server at all, ctrl.NewManager fails and the binary exits before it serves a
// probe.)
func TestNotReadyWithoutAnAPIServer(t *testing.T) {
	for _, b := range binaries() {
		t.Run(b.name, func(t *testing.T) {
			env, cfg := startEnv(t)
			cfg.Timeout = time.Second
			stopAPIServer := func() {
				if err := env.Stop(); err != nil {
					t.Fatal(err)
				}
			}
			start(t, cfg, b, stopAPIServer).staysNotReady(t, 3*time.Second)
		})
	}
}

// TestReadinessKindsAreNotOptional checks that no binary waits for a kind outside the project's API group and
// the built-in groups every cluster serves: an add-on kind (Flux, KEDA, Kyverno) may not be installed or granted.
func TestReadinessKindsAreNotOptional(t *testing.T) {
	allowed := map[string]bool{"": true, "events.k8s.io": true, remediationv1alpha1.SchemeGroupVersion.Group: true}
	for _, b := range binaries() {
		if len(b.kinds) == 0 {
			t.Errorf("%s: no required kind", b.name)
		}
		for _, obj := range b.kinds {
			gvk, err := apiutil.GVKForObject(obj, scheme)
			if err != nil {
				t.Fatal(err)
			}
			if !allowed[gvk.Group] {
				t.Errorf("%s waits for %s, a kind that may not be installed or granted", b.name, gvk)
			}
		}
	}
	if got := analyzer.ReadinessKinds(nil); hasGroupKind(t, got, schema.GroupKind{Kind: "Pod"}) {
		t.Error("the analyzer with no watched namespace waits for Pods, which it never watches then")
	}
}

func hasGroupKind(t *testing.T, objs []client.Object, gk schema.GroupKind) bool {
	t.Helper()
	for _, obj := range objs {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			t.Fatal(err)
		}
		if gvk.GroupKind() == gk {
			return true
		}
	}
	return false
}
