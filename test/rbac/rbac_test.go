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

// Package rbac checks the permission boundary between the analyzer and the executor (design 2.2, invariant 6)
// against a real API server, using the roles generated from the kubebuilder RBAC markers.
package rbac

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/cmdutil"
	"github.com/pkalin/shadewright/internal/controller"
	"github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	executor   = "executor"
	analyzer   = "analyzer"
	operatorNS = "shadewright-system"
	otherNS    = "team-a"
	group      = "remediation.shadewright.io"
	kindRole   = "Role"
)

var (
	scheme = runtime.NewScheme()
	cfg    *rest.Config
	admin  client.Client
)

func TestMain(m *testing.M) {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvtestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	var err error
	if cfg, err = env.Start(); err != nil {
		panic(err)
	}
	if admin, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	if err := setup(context.Background()); err != nil {
		_ = env.Stop()
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func firstEnvtestBinaryDir() string {
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

// setup creates the namespaces and ServiceAccounts, applies the generated roles (with the namespace that
// kustomize would set) and binds each role to its ServiceAccount, as config/rbac does.
func setup(ctx context.Context) error {
	for _, ns := range []string{operatorNS, otherNS} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			return err
		}
	}
	for _, id := range []string{executor, analyzer} {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: operatorNS}}
		if err := admin.Create(ctx, sa); err != nil {
			return err
		}
		if err := applyRoles(ctx, id, "role.yaml"); err != nil {
			return err
		}
		if err := applyRoles(ctx, id, "leader_election.yaml"); err != nil {
			return err
		}
	}
	return nil
}

// applyRoles creates the roles in one file of config/rbac/<id>. Roles from role.yaml are bound to the
// ServiceAccount here, as role_binding.yaml does; RoleBindings in the file are applied as written.
func applyRoles(ctx context.Context, id, file string) error {
	{
		f, err := os.Open(filepath.Join("..", "..", "config", "rbac", id, file))
		if err != nil {
			return err
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		subject := []rbacv1.Subject{{Kind: "ServiceAccount", Name: id, Namespace: operatorNS}}
		for {
			var u map[string]any
			if err := dec.Decode(&u); err != nil {
				break
			}
			switch u["kind"] {
			case "ClusterRole":
				var r rbacv1.ClusterRole
				if err := decodeInto(u, &r); err != nil {
					return err
				}
				if err := admin.Create(ctx, &r); err != nil {
					return err
				}
				b := &rbacv1.ClusterRoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: r.Name},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: r.Name},
					Subjects:   subject,
				}
				if err := admin.Create(ctx, b); err != nil {
					return err
				}
			case "RoleBinding":
				var b rbacv1.RoleBinding
				if err := decodeInto(u, &b); err != nil {
					return err
				}
				b.Namespace = operatorNS
				for i := range b.Subjects {
					b.Subjects[i].Namespace = operatorNS
				}
				if err := admin.Create(ctx, &b); err != nil {
					return err
				}
			case kindRole:
				var r rbacv1.Role
				if err := decodeInto(u, &r); err != nil {
					return err
				}
				r.Namespace = operatorNS
				if err := admin.Create(ctx, &r); err != nil {
					return err
				}
				if file != "role.yaml" {
					continue
				}
				b := &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: r.Name, Namespace: operatorNS},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindRole, Name: r.Name},
					Subjects:   subject,
				}
				if err := admin.Create(ctx, b); err != nil {
					return err
				}
			}
		}
		_ = f.Close()
	}
	return nil
}

func decodeInto(u map[string]any, out runtime.Object) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u, out)
}

func user(id string) string { return "system:serviceaccount:" + operatorNS + ":" + id }

// can asks the API server's authorizer whether the ServiceAccount may perform the action.
func can(t *testing.T, id, verb, apiGroup, resource, subresource, namespace string) bool {
	t.Helper()
	return canNamed(t, id, verb, apiGroup, resource, subresource, namespace, "")
}

// canNamed is can for one named object.
func canNamed(t *testing.T, id, verb, apiGroup, resource, subresource, namespace, name string) bool {
	t.Helper()
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   user(id),
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNS, "system:authenticated"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb: verb, Group: apiGroup, Resource: resource, Subresource: subresource, Namespace: namespace,
			Name: name,
		},
	}}
	if err := admin.Create(context.Background(), sar); err != nil {
		t.Fatalf("SubjectAccessReview: %v", err)
	}
	return sar.Status.Allowed
}

// The table spells every verb and resource out so each row reads as one rule.
//
//nolint:goconst // literals keep the table readable
func TestPermissionBoundary(t *testing.T) {
	cases := []struct {
		id, verb, group, resource, sub, ns string
		want                               bool
	}{
		// Analyzer: writes incidents, creates requests in the operator namespace, never writes request status.
		{"analyzer", "create", group, "remediationrequests", "", operatorNS, true},
		{"analyzer", "create", group, "remediationrequests", "", otherNS, false},
		{"analyzer", "update", group, "remediationrequests", "status", operatorNS, false},
		{"analyzer", "patch", group, "remediationrequests", "status", operatorNS, false},
		{"analyzer", "update", group, "remediationrequests", "", operatorNS, false},
		{"analyzer", "delete", group, "remediationrequests", "", operatorNS, false},
		{"analyzer", "create", group, "incidents", "", otherNS, true},
		{"analyzer", "patch", group, "incidents", "status", otherNS, true},
		{"analyzer", "list", "events.k8s.io", "events", "", otherNS, true},
		{"analyzer", "list", "", "events", "", otherNS, false},
		{"analyzer", "get", "", "secrets", "", otherNS, false},
		{"analyzer", "list", "", "secrets", "", operatorNS, false},
		{"analyzer", "create", group, "remediationpolicies", "", "", false},
		{"analyzer", "update", group, "remediationrequests", "finalizers", operatorNS, false},
		{"analyzer", "delete", group, "incidents", "", otherNS, false},
		{"analyzer", "get", "", "configmaps", "", operatorNS, false},
		// Executor: writes request status in its namespace only, reads the rest, never creates requests or
		// writes incidents.
		{"executor", "update", group, "remediationrequests", "status", operatorNS, true},
		{"executor", "patch", group, "remediationrequests", "status", operatorNS, true},
		{"executor", "update", group, "remediationrequests", "status", otherNS, false},
		{"executor", "list", group, "remediationrequests", "", otherNS, false},
		{"executor", "create", group, "remediationrequests", "", operatorNS, false},
		{"executor", "update", group, "remediationrequests", "", operatorNS, false},
		{"executor", "patch", group, "remediationrequests", "", operatorNS, false},
		{"executor", "delete", group, "remediationrequests", "", operatorNS, false},
		{"executor", "get", group, "incidents", "status", otherNS, false},
		{"executor", "get", "", "configmaps", "", operatorNS, false},
		{"executor", "create", group, "incidents", "", otherNS, false},
		{"executor", "update", group, "incidents", "status", otherNS, false},
		{"executor", "get", group, "incidents", "", otherNS, false},
		{"executor", "list", group, "incidents", "", otherNS, false},
		{"executor", "watch", group, "remediationpolicies", "", "", false},
		{"executor", "list", group, "remediationsettings", "", "", false},
		{"executor", "get", group, "remediationpolicies", "", "", true},
		{"executor", "update", group, "remediationpolicies", "", "", false},
		{"executor", "get", "", "secrets", "", operatorNS, false},
		{"executor", "list", "", "secrets", "", otherNS, false},
		// Executor source mapping: get on the target kinds and the Flux objects, nothing more.
		{"executor", "get", "apps", "deployments", "", otherNS, true},
		{"executor", "get", "apps", "statefulsets", "", otherNS, true},
		{"executor", "get", "keda.sh", "scaledobjects", "", otherNS, true},
		{"executor", "get", "kustomize.toolkit.fluxcd.io", "kustomizations", "", otherNS, true},
		{"executor", "get", "source.toolkit.fluxcd.io", "gitrepositories", "", otherNS, true},
		{"executor", "list", "apps", "deployments", "", otherNS, false},
		{"executor", "watch", "apps", "statefulsets", "", otherNS, false},
		{"executor", "list", "keda.sh", "scaledobjects", "", otherNS, false},
		{"executor", "list", "kustomize.toolkit.fluxcd.io", "kustomizations", "", otherNS, false},
		{"executor", "watch", "source.toolkit.fluxcd.io", "gitrepositories", "", otherNS, false},
		{"executor", "update", "apps", "deployments", "", otherNS, false},
		{"executor", "patch", "apps", "deployments", "", otherNS, false},
		{"executor", "update", "apps", "deployments", "scale", otherNS, false},
		{"executor", "update", "keda.sh", "scaledobjects", "", otherNS, false},
		{"executor", "patch", "kustomize.toolkit.fluxcd.io", "kustomizations", "", otherNS, false},
		{"executor", "update", "source.toolkit.fluxcd.io", "gitrepositories", "", otherNS, false},
		{"executor", "get", "", "configmaps", "", otherNS, false},
		{"executor", "get", "helm.toolkit.fluxcd.io", "helmreleases", "", otherNS, false},
		{"executor", "get", "", "secrets", "", otherNS, false},
		// The analyzer gets none of it through the executor's role.
		{"analyzer", "get", "kustomize.toolkit.fluxcd.io", "kustomizations", "", otherNS, false},
		{"analyzer", "get", "source.toolkit.fluxcd.io", "gitrepositories", "", otherNS, false},
	}
	for _, c := range cases {
		if got := can(t, c.id, c.verb, c.group, c.resource, c.sub, c.ns); got != c.want {
			t.Errorf("%s %s %s/%s %q in %q: allowed=%v, want %v", c.id, c.verb, c.group, c.resource, c.sub, c.ns, got, c.want)
		}
	}
}

// TestLeasesAreSeparate checks that each identity can renew only its own leader election Lease.
func TestLeasesAreSeparate(t *testing.T) {
	leases := map[string]string{executor: cmdutil.ExecutorLease, analyzer: cmdutil.AnalyzerLease}
	for id, own := range leases {
		for other, lease := range leases {
			want := other == id
			for _, verb := range []string{"get", "update", "patch"} {
				if got := canNamed(t, id, verb, "coordination.k8s.io", "leases", "", operatorNS, lease); got != want {
					t.Errorf("%s %s lease %s: allowed=%v, want %v", id, verb, lease, got, want)
				}
			}
		}
		if can(t, id, "create", "coordination.k8s.io", "leases", "", operatorNS) {
			t.Errorf("%s can create leases, so it could squat %s's lease", id, own)
		}
		if canNamed(t, id, "delete", "coordination.k8s.io", "leases", "", operatorNS, own) {
			t.Errorf("%s can delete leases", id)
		}
	}
}

// TestLeaseNamesMatchTheBinaries guards against drift between the Lease created by the manifests, the
// Role that may renew it, and the LeaderElectionID compiled into each binary. The kustomize name prefix
// "shadewright-" is applied to the Lease name but not to resourceNames.
func TestLeaseNamesMatchTheBinaries(t *testing.T) {
	for id, want := range map[string]string{executor: cmdutil.ExecutorLease, analyzer: cmdutil.AnalyzerLease} {
		f, err := os.Open(filepath.Join("..", "..", "config", "rbac", id, "leader_election.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		var leaseName string
		var resourceNames []string
		for {
			var u map[string]any
			if err := dec.Decode(&u); err != nil {
				break
			}
			switch u["kind"] {
			case "Lease":
				leaseName = "shadewright-" + u["metadata"].(map[string]any)["name"].(string)
			case kindRole:
				var r rbacv1.Role
				if err := decodeInto(u, &r); err != nil {
					t.Fatal(err)
				}
				for _, rule := range r.Rules {
					resourceNames = append(resourceNames, rule.ResourceNames...)
				}
			}
		}
		_ = f.Close()
		if leaseName != want {
			t.Errorf("%s: manifest Lease is %q after the prefix, binary uses %q", id, leaseName, want)
		}
		if len(resourceNames) != 1 || resourceNames[0] != want {
			t.Errorf("%s: Role may renew %v, binary uses %q", id, resourceNames, want)
		}
	}
}

// impersonate returns a client acting as the ServiceAccount.
func impersonate(t *testing.T, id string) client.Client {
	t.Helper()
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{
		UserName: user(id),
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNS, "system:authenticated"},
	}
	cl, err := client.New(c, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func newRequest(ns, name string) *remediationv1alpha1.RemediationRequest {
	return testfixtures.RemediationRequest(ns, name)
}

func TestRealCallsMatchTheBoundary(t *testing.T) {
	ctx := context.Background()
	asAnalyzer, asExecutor := impersonate(t, analyzer), impersonate(t, executor)

	req := newRequest(operatorNS, "from-analyzer")
	if err := asAnalyzer.Create(ctx, req); err != nil {
		t.Fatalf("analyzer cannot create a request: %v", err)
	}
	if err := asExecutor.Create(ctx, newRequest(operatorNS, "from-executor")); err == nil {
		t.Error("executor created a request")
	}

	req.Status.Phase = remediationv1alpha1.PhasePolicyChecked
	if err := asAnalyzer.Status().Update(ctx, req); err == nil {
		t.Error("analyzer wrote a request's status")
	}
	var fresh remediationv1alpha1.RemediationRequest
	if err := asExecutor.Get(ctx, client.ObjectKeyFromObject(req), &fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Status.Phase = remediationv1alpha1.PhasePolicyChecked
	if err := asExecutor.Status().Update(ctx, &fresh); err != nil {
		t.Errorf("executor cannot write the status: %v", err)
	}
}

func TestExecutorCacheSeesOnlyTheOperatorNamespace(t *testing.T) {
	ctx := t.Context()
	seen, unseen := newRequest(operatorNS, "seen"), newRequest(otherNS, "unseen")
	for _, r := range []*remediationv1alpha1.RemediationRequest{seen, unseen} {
		if err := admin.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	opts := controller.CacheOptions(operatorNS)
	opts.Scheme = scheme
	c, err := cache.New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.Start(ctx) }()
	syncCtx, done := context.WithTimeout(ctx, 30*time.Second)
	defer done()
	if !c.WaitForCacheSync(syncCtx) {
		t.Fatal("cache did not sync")
	}
	var list remediationv1alpha1.RemediationRequestList
	if err := c.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	for _, r := range list.Items {
		if r.Namespace != operatorNS {
			t.Errorf("executor cache sees %s/%s", r.Namespace, r.Name)
		}
	}
	var got remediationv1alpha1.RemediationRequest
	if err := c.Get(ctx, client.ObjectKey{Namespace: otherNS, Name: "unseen"}, &got); err == nil {
		t.Error("executor cache returned a request from another namespace")
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: "seen"}, &got); err != nil {
		t.Errorf("executor cache misses a request in its own namespace: %v", err)
	}
}
