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
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	analyzerpkg "github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/cmdutil"
	"github.com/pkalin/shadewright/internal/controller"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	executor   = "executor"
	analyzer   = "analyzer"
	verifierID = "verifier"
	operatorNS = "shadewright-system"
	otherNS    = "team-a" // watched by the analyzer
	unwatched  = "team-b" // not watched
	group      = "remediation.shadewright.io"
	kindRole   = "Role"
	kindCR     = "ClusterRole"
)

// saGroups are the groups the API server gives a ServiceAccount in the operator namespace.
var saGroups = []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNS, "system:authenticated"}

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
	for _, ns := range []string{operatorNS, otherNS, unwatched} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			return err
		}
	}
	for _, id := range []string{executor, analyzer, verifierID} {
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
	if err := bindWatchRole(ctx, otherNS); err != nil {
		return err
	}
	return bindVerifierWatchRole(ctx, otherNS)
}

// verifierWatchRolePath is the generated ClusterRole of the verifier's reads in watched namespaces.
var verifierWatchRolePath = filepath.Join("..", "..", "config", "rbac", verifierID, "watch", "role.yaml")

// bindVerifierWatchRole creates the verifier's watch ClusterRole and binds it in one namespace with a
// RoleBinding, as config/watch does for each namespace in the verifier's --watch-namespaces.
func bindVerifierWatchRole(ctx context.Context, ns string) error {
	r, err := readClusterRole(verifierWatchRolePath)
	if err != nil {
		return err
	}
	if err := admin.Create(ctx, r); err != nil {
		return err
	}
	return bindIn(ctx, r.Name, verifierID, ns)
}

// bindWatchRole creates the analyzer's watch ClusterRole and the KEDA module's read ClusterRole, and binds both
// in one namespace with a RoleBinding each, as config/watch does for each namespace in --watch-namespaces (the
// chart's default, analyzer.modules.keda.enabled true). The other modules' roles are off by default and not
// bound here.
func bindWatchRole(ctx context.Context, ns string) error {
	for _, path := range []string{watchRolePath, moduleRolePath("keda")} {
		r, err := readClusterRole(path)
		if err != nil {
			return err
		}
		if err := admin.Create(ctx, r); err != nil {
			return err
		}
		if err := bindIn(ctx, r.Name, analyzer, ns); err != nil {
			return err
		}
	}
	return nil
}

// watchRolePath is the generated watch ClusterRole.
var watchRolePath = filepath.Join("..", "..", "config", "rbac", analyzer, "watch", "role.yaml")

// moduleRolePath is the generated ClusterRole of one module's reads (config/rbac/analyzer/modules).
func moduleRolePath(name string) string {
	return filepath.Join("..", "..", "config", "rbac", analyzer, "modules", name, "role.yaml")
}

// readClusterRole decodes the one ClusterRole of a generated file.
func readClusterRole(path string) (*rbacv1.ClusterRole, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var r rbacv1.ClusterRole
	if err := utilyaml.NewYAMLOrJSONDecoder(f, 4096).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// bindIn binds a ClusterRole to a ServiceAccount of the operator namespace with a RoleBinding in ns, or with a
// ClusterRoleBinding when ns is "".
func bindIn(ctx context.Context, role, sa, ns string) error {
	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindCR, Name: role}
	subjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: operatorNS}}
	if ns == "" {
		return admin.Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role + "-" + sa}, RoleRef: ref, Subjects: subjects,
		})
	}
	return admin.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role + "-" + sa, Namespace: ns}, RoleRef: ref, Subjects: subjects,
	})
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
			case kindCR:
				var r rbacv1.ClusterRole
				if err := decodeInto(u, &r); err != nil {
					return err
				}
				if err := admin.Create(ctx, &r); err != nil {
					return err
				}
				b := &rbacv1.ClusterRoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: r.Name},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindCR, Name: r.Name},
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
		Groups: saGroups,
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
		// Incidents live in the operator namespace only.
		{"analyzer", "create", group, "incidents", "", operatorNS, true},
		{"analyzer", "update", group, "incidents", "status", operatorNS, true},
		{"analyzer", "patch", group, "incidents", "status", operatorNS, false},
		{"analyzer", "get", group, "incidents", "status", operatorNS, false},
		{"analyzer", "update", group, "incidents", "", operatorNS, false},
		{"analyzer", "patch", group, "incidents", "", operatorNS, false},
		{"analyzer", "create", group, "incidents", "", otherNS, false},
		{"analyzer", "patch", group, "incidents", "status", otherNS, false},
		{"analyzer", "create", group, "incidents", "", unwatched, false},
		// The Warning event at the Incident cap is a core Event, written with the leader election Role's grant
		// in the operator namespace; there is no grant on events.k8s.io Events to write.
		{"analyzer", "create", "", "events", "", operatorNS, true},
		{"analyzer", "create", "", "events", "", otherNS, false},
		{"analyzer", "delete", "", "events", "", operatorNS, false},
		{"analyzer", "create", "events.k8s.io", "events", "", operatorNS, false},
		{"analyzer", "patch", "events.k8s.io", "events", "", operatorNS, false},
		{"analyzer", "create", "events.k8s.io", "events", "", otherNS, false},
		// Reads only in the watched namespaces, never cluster-wide.
		{"analyzer", "list", "events.k8s.io", "events", "", otherNS, true},
		{"analyzer", "watch", "", "pods", "", otherNS, true},
		// Workloads: get only, one owner at a time; no list or watch, so no informer on them.
		{"analyzer", "get", "apps", "deployments", "", otherNS, true},
		{"analyzer", "get", "apps", "replicasets", "", otherNS, true},
		{"analyzer", "get", "apps", "statefulsets", "", otherNS, true},
		{"analyzer", "list", "apps", "deployments", "", otherNS, false},
		{"analyzer", "watch", "apps", "replicasets", "", otherNS, false},
		{"analyzer", "list", "apps", "statefulsets", "", otherNS, false},
		{"analyzer", "get", "apps", "daemonsets", "", otherNS, false},
		{"analyzer", "get", "batch", "jobs", "", otherNS, false},
		// Module symptom sources (#380): HPAs are watched, read only, in the watched namespaces; the ScaledObject
		// that controls one is read one at a time by name to verify it, and only while the KEDA module is
		// active (the collector's tests check that).
		{"analyzer", "get", "autoscaling", "horizontalpodautoscalers", "", otherNS, true},
		{"analyzer", "list", "autoscaling", "horizontalpodautoscalers", "", otherNS, true},
		{"analyzer", "watch", "autoscaling", "horizontalpodautoscalers", "", otherNS, true},
		{"analyzer", "update", "autoscaling", "horizontalpodautoscalers", "", otherNS, false},
		{"analyzer", "patch", "autoscaling", "horizontalpodautoscalers", "status", otherNS, false},
		{"analyzer", "delete", "autoscaling", "horizontalpodautoscalers", "", otherNS, false},
		{"analyzer", "list", "autoscaling", "horizontalpodautoscalers", "", unwatched, false},
		{"analyzer", "list", "autoscaling", "horizontalpodautoscalers", "", "", false},
		// The KEDA module's reads (#409, analyzer-watch-keda, on by default): ScaledObjects and ScaledJobs, read
		// only, in the watched namespaces. TriggerAuthentications are never read.
		{"analyzer", "get", "keda.sh", "scaledobjects", "", otherNS, true},
		{"analyzer", "list", "keda.sh", "scaledobjects", "", otherNS, true},
		{"analyzer", "watch", "keda.sh", "scaledobjects", "", otherNS, true},
		{"analyzer", "update", "keda.sh", "scaledobjects", "", otherNS, false},
		{"analyzer", "patch", "keda.sh", "scaledobjects", "", otherNS, false},
		{"analyzer", "get", "keda.sh", "scaledobjects", "", unwatched, false},
		{"analyzer", "list", "keda.sh", "scaledobjects", "", "", false},
		{"analyzer", "get", "keda.sh", "scaledjobs", "", otherNS, false},
		{"analyzer", "list", "keda.sh", "scaledjobs", "", otherNS, true},
		{"analyzer", "watch", "keda.sh", "scaledjobs", "", otherNS, true},
		{"analyzer", "update", "keda.sh", "scaledjobs", "", otherNS, false},
		{"analyzer", "list", "keda.sh", "scaledjobs", "", unwatched, false},
		{"analyzer", "get", "keda.sh", "triggerauthentications", "", otherNS, false},
		// The other modules' reads are off by default (their roles are not bound here).
		{"analyzer", "list", "kustomize.toolkit.fluxcd.io", "kustomizations", "", otherNS, false},
		{"analyzer", "list", "kyverno.io", "policies", "", otherNS, false},
		{"analyzer", "list", "kyverno.io", "clusterpolicies", "", "", false},
		{"analyzer", "list", "wgpolicyk8s.io", "clusterpolicyreports", "", "", false},
		{"analyzer", "list", "events.k8s.io", "events", "", unwatched, false},
		{"analyzer", "get", "", "pods", "", unwatched, false},
		{"analyzer", "get", "apps", "replicasets", "", unwatched, false},
		{"analyzer", "list", "", "pods", "", "", false},
		{"analyzer", "list", "events.k8s.io", "events", "", "", false},
		{"analyzer", "list", "", "pods", "", operatorNS, false},
		{"analyzer", "list", group, "remediationsettings", "", "", true},
		// The analyzer's one write on the settings is an update of their status subresource, the detection
		// report (ADR-0024); no patch, no read of the subresource, nothing on the spec. The daily analysis
		// budget is a ConfigMap of its own (TestAnalysisBudgetIsTheOnlyConfigMap).
		{"analyzer", "get", group, "remediationsettings", "status", "", false},
		{"analyzer", "update", group, "remediationsettings", "status", "", true},
		{"analyzer", "patch", group, "remediationsettings", "status", "", false},
		{"analyzer", "update", group, "remediationpolicies", "", "", false},
		{"executor", "update", group, "remediationsettings", "status", "", false},
		// Platform detection lists nodes cluster-wide; nothing else on them, so no informer and no write.
		{"analyzer", "list", "", "nodes", "", "", true},
		{"analyzer", "get", "", "nodes", "", "", false},
		{"analyzer", "watch", "", "nodes", "", "", false},
		{"analyzer", "update", "", "nodes", "", "", false},
		{"analyzer", "patch", "", "nodes", "status", "", false},
		{"analyzer", "delete", "", "nodes", "", "", false},
		{"executor", "list", "", "nodes", "", "", false},
		{"analyzer", "update", group, "remediationsettings", "", "", false},
		{"analyzer", "patch", group, "remediationsettings", "", "", false},
		{"analyzer", "create", group, "remediationsettings", "", "", false},
		{"analyzer", "delete", group, "remediationsettings", "", "", false},
		{"analyzer", "update", group, "remediationpolicies", "status", "", false},
		{"analyzer", "list", "", "events", "", otherNS, false},
		// Secrets: every verb in every namespace is refused in TestAnalyzerGrantsNothingOnSecrets.
		{"analyzer", "create", group, "remediationpolicies", "", "", false},
		{"analyzer", "update", group, "remediationrequests", "finalizers", operatorNS, false},
		{"analyzer", "delete", group, "incidents", "", operatorNS, false},
		{"analyzer", "get", "", "configmaps", "", operatorNS, false},
		// Executor: writes request status in its namespace only, reads the rest, never creates requests or
		// writes incidents.
		{"executor", "update", group, "remediationrequests", "status", operatorNS, true},
		{"executor", "patch", group, "remediationrequests", "status", operatorNS, true},
		{"executor", "update", group, "remediationrequests", "status", otherNS, false},
		{"executor", "list", group, "remediationrequests", "", otherNS, false},
		{"executor", "create", group, "remediationrequests", "", operatorNS, false},
		{"executor", "update", group, "remediationrequests", "", operatorNS, false},
		// patch only for the finalizer that closes the pull request on deletion; the spec is immutable (CEL).
		{"executor", "patch", group, "remediationrequests", "", operatorNS, true},
		{"executor", "patch", group, "remediationrequests", "", otherNS, false},
		{"executor", "delete", group, "remediationrequests", "", operatorNS, false},
		{"executor", "get", group, "incidents", "status", otherNS, false},
		{"executor", "get", "", "configmaps", "", operatorNS, false},
		{"executor", "create", group, "incidents", "", otherNS, false},
		{"executor", "update", group, "incidents", "status", otherNS, false},
		{"executor", "get", group, "incidents", "", otherNS, false},
		{"executor", "list", group, "incidents", "", otherNS, false},
		// get on the request's Incident only, to check the name the pull request body shows (#264).
		{"executor", "get", group, "incidents", "", operatorNS, true},
		// get on the verifier's reports in the operator namespace, its evidence for Cleared (ADR-0030 section 7,
		// #683): no list or watch, no write, nowhere else, and no grant on pods.
		{"executor", "get", group, "verificationreports", "", operatorNS, true},
		{"executor", "get", group, "verificationreports", "", otherNS, false},
		{"executor", "list", group, "verificationreports", "", operatorNS, false},
		{"executor", "watch", group, "verificationreports", "", operatorNS, false},
		{"executor", "update", group, "verificationreports", "status", operatorNS, false},
		{"executor", "delete", group, "verificationreports", "", operatorNS, false},
		{"executor", "get", "", "pods", "", otherNS, false},
		{"executor", "list", "", "pods", "", otherNS, false},
		{"executor", "list", "", "pods", "", "", false},
		{"executor", "get", "", "pods", "", operatorNS, false},
		// The kill switch's Events about a request (WritesPaused, WritesResumed) are core Events, written with
		// the leader election Role's grant in the operator namespace, where the requests are.
		{"executor", "create", "", "events", "", operatorNS, true},
		{"executor", "create", "", "events", "", otherNS, false},
		{"executor", "delete", "", "events", "", operatorNS, false},
		{"executor", "create", "events.k8s.io", "events", "", operatorNS, false},
		{"executor", "list", group, "incidents", "", operatorNS, false},
		{"executor", "watch", group, "incidents", "", operatorNS, false},
		{"executor", "update", group, "incidents", "", operatorNS, false},
		{"executor", "update", group, "incidents", "status", operatorNS, false},
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

// TestPodLogsOnlyInWatchedNamespaces checks the analyzer's one grant on logs (#371): get on pods/log through the
// watch role, so only in the namespaces a RoleBinding names, never in the operator namespace, an unwatched
// namespace or cluster-wide, and no other verb. The executor has none.
func TestPodLogsOnlyInWatchedNamespaces(t *testing.T) {
	cases := []struct {
		id, verb, ns string
		want         bool
	}{
		{analyzer, "get", otherNS, true},
		{analyzer, "get", unwatched, false},
		{analyzer, "get", operatorNS, false},
		{analyzer, "get", "", false},
		{analyzer, "list", otherNS, false},
		{analyzer, "watch", otherNS, false},
		{analyzer, "create", otherNS, false},
		{executor, "get", otherNS, false},
		{executor, "get", "", false},
	}
	for _, c := range cases {
		if got := can(t, c.id, c.verb, "", "pods", "log", c.ns); got != c.want {
			t.Errorf("%s %s pods/log in %q: allowed=%v, want %v", c.id, c.verb, c.ns, got, c.want)
		}
	}
	// The grant is in the watch role only: no role of the analyzer bound in the operator namespace or
	// cluster-wide names pods or any of their subresources.
	for _, file := range []string{"role.yaml", "leader_election.yaml"} {
		for _, doc := range decodeAll(t, filepath.Join("..", "..", "config", "rbac", analyzer, file)) {
			var r rbacv1.ClusterRole // a Role has the same rules
			if err := decodeInto(doc, &r); err != nil {
				t.Fatal(err)
			}
			for _, rule := range r.Rules {
				for _, res := range rule.Resources {
					if strings.HasPrefix(res, "pods") || res == "*" {
						t.Errorf("%s: %s names %s", file, r.Name, res)
					}
				}
			}
		}
	}
}

// TestWatchRoleIsReadOnly checks that every rule of the analyzer's watch role, which the module symptom sources
// widened (#380), only reads: get, list or watch, on named groups and resources, never secrets.
func TestWatchRoleIsReadOnly(t *testing.T) {
	read := []string{"get", "list", "watch"}
	paths := make([]string, 0, 5)
	paths = append(paths, watchRolePath)
	for _, m := range []string{"keda", "flux", "kyverno", "kyverno-cluster"} {
		paths = append(paths, moduleRolePath(m))
	}
	for _, path := range paths {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) { checkReadOnlyRole(t, path, read) })
	}
}

// checkReadOnlyRole checks that the one role of a generated file grants only the read verbs on named resources.
func checkReadOnlyRole(t *testing.T, path string, read []string) {
	t.Helper()
	roles := 0
	for _, u := range decodeAll(t, path) {
		rules, _, ok := policyRules(t, path, u)
		if !ok {
			continue
		}
		roles++
		for _, rule := range rules {
			for _, v := range rule.Verbs {
				if !slices.Contains(read, v) {
					t.Errorf("watch role rule %v grants %q", rule, v)
				}
			}
			if slices.Contains(rule.APIGroups, "*") || slices.Contains(rule.Resources, "*") ||
				slices.Contains(rule.Resources, "secrets") || len(rule.ResourceNames) > 0 || len(rule.NonResourceURLs) > 0 {
				t.Errorf("watch role rule %v is not a plain read of named resources", rule)
			}
		}
	}
	if roles != 1 {
		t.Fatalf("read %d roles from %s, want 1", roles, path)
	}
}

// TestAnalyzerGrantsNothingOnSecrets checks that the analyzer holds no permission at all on Secrets (design
// 10.3, invariant 6). Kubernetes RBAC has no metadata-only verb: get, list or watch on secrets, also through
// the metadata API, returns the data, so the missing Secret check (internal/analyzer/refcheck) infers its
// answer from pod status and Events instead. The API server is asked for every verb in every namespace,
// cluster-wide included, and every Role and ClusterRole in config/rbac/analyzer is read, so a rule that names
// secrets, or any resource or group by wildcard, fails the test even before it is bound.
func TestAnalyzerGrantsNothingOnSecrets(t *testing.T) {
	verbs := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	for _, ns := range []string{operatorNS, otherNS, unwatched, ""} {
		for _, verb := range verbs {
			if can(t, analyzer, verb, "", "secrets", "", ns) {
				t.Errorf("analyzer may %s secrets in %q", verb, ns)
			}
		}
	}

	var files []string
	root := filepath.Join("..", "..", "config", "rbac", analyzer)
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".yaml") {
			files = append(files, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	roles := 0
	for _, path := range files {
		for _, u := range decodeAll(t, path) {
			rules, aggregated, ok := policyRules(t, path, u)
			if !ok {
				continue
			}
			if aggregated {
				t.Errorf("%s: a ClusterRole aggregates other roles, whose rules this test cannot see", path)
			}
			roles++
			for _, rule := range rules {
				core := slices.Contains(rule.APIGroups, "") || slices.Contains(rule.APIGroups, "*")
				if core && (slices.Contains(rule.Resources, "secrets") || slices.Contains(rule.Resources, "*")) {
					t.Errorf("%s: a rule grants %v on %v in groups %q", path, rule.Verbs, rule.Resources, rule.APIGroups)
				}
			}
		}
	}
	// role.yaml (a Role and a ClusterRole), leader_election.yaml and watch/role.yaml: a test that read no role
	// would pass for nothing.
	if roles < 4 {
		t.Fatalf("read %d roles in %s, want at least 4", roles, root)
	}
}

// TestLeasesAreSeparate checks that each identity can renew only its own leader election Lease.
func TestLeasesAreSeparate(t *testing.T) {
	leases := map[string]string{executor: cmdutil.ExecutorLease, analyzer: cmdutil.AnalyzerLease,
		verifierID: cmdutil.VerifierLease}
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
	for id, want := range map[string]string{executor: cmdutil.ExecutorLease, analyzer: cmdutil.AnalyzerLease,
		verifierID: cmdutil.VerifierLease} {
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
			if u["kind"] == "Lease" {
				leaseName = "shadewright-" + u["metadata"].(map[string]any)["name"].(string)
				continue
			}
			rules, _, _ := policyRules(t, id+"/leader_election.yaml", u)
			for _, rule := range rules {
				resourceNames = append(resourceNames, rule.ResourceNames...)
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

// TestAnalysisBudgetIsTheOnlyConfigMap checks that the analyzer may get and update its analysis budget
// ConfigMap in the operator namespace and nothing else on ConfigMaps: no other name, no other namespace, no
// list, watch, create, patch or delete, so it can neither read other ConfigMaps nor recreate a deleted
// budget. The executor has no grant on it.
func TestAnalysisBudgetIsTheOnlyConfigMap(t *testing.T) {
	budget, other := incidents.BudgetName, "other"
	cases := []struct {
		id, verb, ns, name string
		want               bool
	}{
		{analyzer, "get", operatorNS, budget, true},
		{analyzer, "update", operatorNS, budget, true},
		{analyzer, "patch", operatorNS, budget, false},
		{analyzer, "delete", operatorNS, budget, false},
		{analyzer, "create", operatorNS, "", false},
		{analyzer, "create", operatorNS, budget, false},
		{analyzer, "list", operatorNS, "", false},
		{analyzer, "watch", operatorNS, "", false},
		{analyzer, "deletecollection", operatorNS, "", false},
		{analyzer, "get", operatorNS, "", false},
		{analyzer, "get", operatorNS, "kube-root-ca.crt", false},
		{analyzer, "update", operatorNS, other, false},
		{analyzer, "get", otherNS, budget, false},
		{analyzer, "update", otherNS, budget, false},
		{analyzer, "update", unwatched, budget, false},
		{executor, "get", operatorNS, budget, false},
		{executor, "update", operatorNS, budget, false},
	}
	for _, c := range cases {
		if got := canNamed(t, c.id, c.verb, "", "configmaps", "", c.ns, c.name); got != c.want {
			t.Errorf("%s %s configmap %q in %q: allowed=%v, want %v", c.id, c.verb, c.name, c.ns, got, c.want)
		}
	}
}

// TestAnalysisBudgetRealCalls makes the analyzer's own calls on ConfigMaps as its ServiceAccount: an update
// of the budget with the resourceVersion read succeeds, the same update of another ConfigMap and the
// creation of the budget after it was deleted are refused.
func TestAnalysisBudgetRealCalls(t *testing.T) {
	ctx := t.Context()
	asAnalyzer := impersonate(t, analyzer)
	const other = "other"
	for _, name := range []string{incidents.BudgetName, other} {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: name}}
		if err := admin.Create(ctx, cm); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Delete(context.Background(), cm) })
	}
	budget := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: operatorNS, Name: incidents.BudgetName}
	if err := asAnalyzer.Get(ctx, key, budget); err != nil {
		t.Fatalf("analyzer cannot read its budget: %v", err)
	}
	budget.Data = map[string]string{incidents.BudgetDayKey: "2026-11-03", incidents.BudgetUsedKey: "1"}
	if err := asAnalyzer.Update(ctx, budget); err != nil {
		t.Fatalf("analyzer cannot update its budget: %v", err)
	}
	otherCM := &corev1.ConfigMap{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: other}, otherCM); err != nil {
		t.Fatal(err)
	}
	otherCM.Data = map[string]string{"used": "0"}
	if err := asAnalyzer.Update(ctx, otherCM); !apierrors.IsForbidden(err) {
		t.Errorf("analyzer updating another ConfigMap = %v, want forbidden", err)
	}
	if err := asAnalyzer.Get(ctx, client.ObjectKeyFromObject(otherCM), &corev1.ConfigMap{}); !apierrors.IsForbidden(err) {
		t.Errorf("analyzer reading another ConfigMap = %v, want forbidden", err)
	}
	if err := admin.Delete(ctx, budget); err != nil {
		t.Fatal(err)
	}
	recreated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: incidents.BudgetName}}
	if err := asAnalyzer.Create(ctx, recreated); !apierrors.IsForbidden(err) {
		t.Errorf("analyzer recreating its budget = %v, want forbidden", err)
	}
}

// TestBudgetNameMatchesTheBinary guards against drift between the budget ConfigMap shipped with the analyzer,
// the Role that may update it, and the name compiled into the binary. The kustomize name prefix
// "shadewright-" is applied to the ConfigMap name; the Role carries the full name, which no object of the
// build has before the prefix, so kustomize leaves it as written.
func TestBudgetNameMatchesTheBinary(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "config", "manager", "analysis_budget.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	err = utilyaml.NewYAMLOrJSONDecoder(f, 4096).Decode(&cm)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := "shadewright-" + cm.Name; got != incidents.BudgetName {
		t.Errorf("manifest ConfigMap is %q after the prefix, binary uses %q", got, incidents.BudgetName)
	}
	if len(cm.Data) > 0 || len(cm.BinaryData) > 0 {
		t.Errorf("the shipped budget holds data %v, so a GitOps apply would reset the count", cm.Data)
	}
	var role rbacv1.Role
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: operatorNS, Name: "analyzer-role"}, &role); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, rule := range role.Rules {
		if slices.Contains(rule.Resources, "configmaps") {
			names = append(names, rule.ResourceNames...)
		}
	}
	if len(names) != 1 || names[0] != incidents.BudgetName {
		t.Errorf("the analyzer Role may update ConfigMaps %v, binary uses %q", names, incidents.BudgetName)
	}
}

// TestSettingsStatusRealCalls makes the analyzer's detection write as its ServiceAccount: an update of the
// settings status succeeds, an update of the spec and a patch of the status are refused, and nodes can be
// listed but not read one by one.
func TestSettingsStatusRealCalls(t *testing.T) {
	ctx := t.Context()
	asAnalyzer := impersonate(t, analyzer)
	s := &remediationv1alpha1.RemediationSettings{ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName}}
	if err := admin.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), s) })

	var got remediationv1alpha1.RemediationSettings
	if err := asAnalyzer.Get(ctx, client.ObjectKeyFromObject(s), &got); err != nil {
		t.Fatalf("analyzer cannot read the settings: %v", err)
	}
	got.Status.Platform = &remediationv1alpha1.PlatformStatus{Active: remediationv1alpha1.GenericPlatform}
	if err := asAnalyzer.Status().Update(ctx, &got); err != nil {
		t.Fatalf("analyzer cannot update the settings status: %v", err)
	}
	paused := true
	got.Spec.Paused = &paused
	if err := asAnalyzer.Update(ctx, &got); !apierrors.IsForbidden(err) {
		t.Errorf("analyzer updating the settings spec = %v, want forbidden", err)
	}
	patch := client.RawPatch(types.MergePatchType, []byte(`{"status":{"observedGeneration":7}}`))
	if err := asAnalyzer.Status().Patch(ctx, &got, patch); !apierrors.IsForbidden(err) {
		t.Errorf("analyzer patching the settings status = %v, want forbidden", err)
	}
	if err := asAnalyzer.List(ctx, &corev1.NodeList{}); err != nil {
		t.Errorf("analyzer cannot list nodes: %v", err)
	}
	if err := asAnalyzer.Get(ctx, client.ObjectKey{Name: "any"}, &corev1.Node{}); !apierrors.IsForbidden(err) {
		t.Errorf("analyzer getting a node = %v, want forbidden", err)
	}
}

// impersonate returns a client acting as the ServiceAccount.
func impersonate(t *testing.T, id string) client.Client {
	t.Helper()
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{
		UserName: user(id),
		Groups:   saGroups,
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

// TestAnalyzerCacheWorksWithItsOwnRBAC starts the analyzer's cache as the analyzer itself: it must sync with
// the RBAC it has (so it never asks for more), see Pods only in the watched namespace and Incidents only in the
// operator namespace.
func TestAnalyzerCacheWorksWithItsOwnRBAC(t *testing.T) {
	ctx := t.Context()
	const web = "web"
	for _, ns := range []string{otherNS, unwatched} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: web, Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: web, Image: "example.invalid/web:1"}}},
		}
		if err := admin.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{
		UserName: user(analyzer),
		Groups:   saGroups,
	}
	opts := analyzerpkg.CacheOptions(operatorNS, []string{otherNS})
	opts.Scheme = scheme
	ca, err := cache.New(c, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Informers for each kind the analyzer reads, so a missing grant shows up as a sync that never completes.
	kinds := []client.Object{&corev1.Pod{}, &autoscalingv2.HorizontalPodAutoscaler{}, &remediationv1alpha1.Incident{},
		&remediationv1alpha1.RemediationRequest{}}
	for _, obj := range kinds {
		syncCtx, done := context.WithTimeout(ctx, 30*time.Second)
		_, err := ca.GetInformer(syncCtx, obj, cache.BlockUntilSynced(false))
		done()
		if err != nil {
			t.Fatalf("informer for %T: %v", obj, err)
		}
	}
	go func() { _ = ca.Start(ctx) }()
	syncCtx, done := context.WithTimeout(ctx, 30*time.Second)
	defer done()
	if !ca.WaitForCacheSync(syncCtx) {
		t.Fatal("the analyzer's cache did not sync with the analyzer's own RBAC")
	}
	var pods corev1.PodList
	if err := ca.List(ctx, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Namespace != otherNS {
		t.Errorf("analyzer cache sees %d pods, want the one in %s", len(pods.Items), otherNS)
	}
	var p corev1.Pod
	if err := ca.Get(ctx, client.ObjectKey{Namespace: unwatched, Name: web}, &p); err == nil {
		t.Error("analyzer cache returned a pod from an unwatched namespace")
	}
}

// TestWatchBindingsMatchTheFlag guards against drift between the analyzer's --watch-namespaces
// (config/manager/analyzer.yaml) and the RoleBindings in config/watch: one per namespace, each binding the
// prefixed watch role to the prefixed analyzer ServiceAccount in the namespace config/default installs to.
func TestWatchBindingsMatchTheFlag(t *testing.T) {
	root := filepath.Join("..", "..", "config")

	var flagValue string
	for _, doc := range decodeAll(t, filepath.Join(root, "manager", "analyzer.yaml")) {
		var d appsv1.Deployment
		if err := decodeInto(doc, &d); err != nil {
			t.Fatal(err)
		}
		for _, ct := range d.Spec.Template.Spec.Containers {
			for _, a := range ct.Args {
				if v, ok := strings.CutPrefix(a, "--watch-namespaces="); ok {
					flagValue = v
				}
			}
		}
	}
	want, err := analyzerpkg.ParseNamespaces(flagValue)
	if err != nil {
		t.Fatalf("config/manager/analyzer.yaml: %v", err)
	}

	var kz struct {
		Resources []string `json:"resources"`
	}
	kd := decodeAll(t, filepath.Join(root, "watch", "kustomization.yaml"))
	if len(kd) != 1 {
		t.Fatal("config/watch/kustomization.yaml: want one document")
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(kd[0], &kz); err != nil {
		t.Fatal(err)
	}
	// The watch role and the KEDA module's reads, which are on by default, are bound in each namespace.
	watchRoles := []string{"shadewright-analyzer-watch-role", "shadewright-analyzer-watch-keda"}
	got := map[string][]string{}
	for _, res := range kz.Resources {
		for _, doc := range decodeAll(t, filepath.Join(root, "watch", res)) {
			var b rbacv1.RoleBinding
			if err := decodeInto(doc, &b); err != nil {
				t.Fatal(err)
			}
			if b.Kind != "RoleBinding" {
				t.Errorf("%s: %s, want only RoleBindings", res, b.Kind)
				continue
			}
			if b.RoleRef.Name == "shadewright-verifier-watch-role" {
				continue // the verifier's bindings: TestVerifierWatchBindingsMatchTheFlag
			}
			if b.RoleRef.Kind != kindCR || !slices.Contains(watchRoles, b.RoleRef.Name) {
				t.Errorf("%s: roleRef %s/%s, want a ClusterRole of %v", res, b.RoleRef.Kind, b.RoleRef.Name, watchRoles)
			}
			if len(b.Subjects) != 1 || b.Subjects[0].Name != "shadewright-analyzer" || b.Subjects[0].Namespace != operatorNS {
				t.Errorf("%s: subjects %v, want the ServiceAccount shadewright-analyzer in %s", res, b.Subjects, operatorNS)
			}
			got[b.RoleRef.Name] = append(got[b.RoleRef.Name], b.Namespace)
		}
	}
	for _, role := range watchRoles {
		slices.Sort(got[role])
		if !slices.Equal(got[role], want) {
			t.Errorf("config/watch binds %s in %v, --watch-namespaces is %v", role, got[role], want)
		}
	}
}

// policyRules returns the rules of a decoded Role or ClusterRole, whether it is a ClusterRole with an
// aggregation rule, and false for any other kind. path is only for the failure message.
func policyRules(t *testing.T, path string, u map[string]any) (rules []rbacv1.PolicyRule, aggregated, ok bool) {
	t.Helper()
	switch u["kind"] {
	case kindRole:
		var r rbacv1.Role
		if err := decodeInto(u, &r); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return r.Rules, false, true
	case kindCR:
		var r rbacv1.ClusterRole
		if err := decodeInto(u, &r); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return r.Rules, r.AggregationRule != nil, true
	}
	return nil, false, false
}

func decodeAll(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	var out []map[string]any
	for {
		var u map[string]any
		if err := dec.Decode(&u); err != nil {
			break
		}
		if u != nil {
			out = append(out, u)
		}
	}
	return out
}

// TestModuleRolesGrantOnlyWhenBound checks each module's read role with a ServiceAccount of its own (#409): with
// the role not bound (its chart value off) every read of the module's kinds is refused; once bound (on) the
// reads are allowed, in the watched namespace only, and writes stay refused. The Kyverno cluster-wide role is
// bound with a ClusterRoleBinding, the namespaced ones with a RoleBinding, as the chart and config/modules do.
func TestModuleRolesGrantOnlyWhenBound(t *testing.T) {
	ctx := context.Background()
	// get is granted only where an object is read by name: the ScaledObject a KEDA HPA names as its owner. The
	// sources' informers need list and watch. get_delivery_status reads one Kustomization or HelmRelease with
	// list too (analyzer.ListGetter, #562), so the Flux role keeps no get.
	type kind struct {
		group, resource string
		get             bool
	}
	for _, tc := range []struct {
		dir     string
		cluster bool
		kinds   []kind
	}{
		{"keda", false, []kind{{"keda.sh", "scaledobjects", true}, {"keda.sh", "scaledjobs", false}}},
		{"flux", false, []kind{
			{"kustomize.toolkit.fluxcd.io", "kustomizations", false},
			{"helm.toolkit.fluxcd.io", "helmreleases", false},
			{"source.toolkit.fluxcd.io", "gitrepositories", false},
		}},
		{"kyverno", false, []kind{{"kyverno.io", "policies", false}, {"wgpolicyk8s.io", "policyreports", false}}},
		{"kyverno-cluster", true, []kind{{"kyverno.io", "clusterpolicies", false},
			{"wgpolicyk8s.io", "clusterpolicyreports", false}}},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			sa := "module-" + tc.dir
			account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: operatorNS}}
			if err := admin.Create(ctx, account); err != nil {
				t.Fatalf("create ServiceAccount: %v", err)
			}
			ns := otherNS
			if tc.cluster {
				ns = ""
			}
			for _, k := range tc.kinds {
				for _, verb := range []string{"get", "list", "watch"} {
					if can(t, sa, verb, k.group, k.resource, "", ns) {
						t.Errorf("unbound: %s %s/%s allowed, want refused", verb, k.group, k.resource)
					}
				}
			}
			r, err := readClusterRole(moduleRolePath(tc.dir))
			if err != nil {
				t.Fatalf("read role: %v", err)
			}
			r.Name = "test-" + r.Name // the role of the default setup may exist already
			if err := admin.Create(ctx, r); err != nil {
				t.Fatalf("create role: %v", err)
			}
			if err := bindIn(ctx, r.Name, sa, ns); err != nil {
				t.Fatalf("bind role: %v", err)
			}
			for _, k := range tc.kinds {
				for _, verb := range []string{"list", "watch"} {
					if !eventuallyCan(t, sa, verb, k.group, k.resource, ns) {
						t.Errorf("bound: %s %s/%s refused, want allowed", verb, k.group, k.resource)
					}
				}
				if got := can(t, sa, "get", k.group, k.resource, "", ns); got != k.get {
					t.Errorf("bound: get %s/%s allowed %v, want %v", k.group, k.resource, got, k.get)
				}
				for _, verb := range []string{"create", "update", "patch", "delete"} {
					if can(t, sa, verb, k.group, k.resource, "", ns) {
						t.Errorf("bound: %s %s/%s allowed, want refused", verb, k.group, k.resource)
					}
				}
				if !tc.cluster {
					if can(t, sa, "list", k.group, k.resource, "", unwatched) || can(t, sa, "list", k.group, k.resource, "", "") {
						t.Errorf("bound: list %s/%s allowed outside %s", k.group, k.resource, otherNS)
					}
				}
			}
		})
	}
}

// eventuallyCan waits a moment for a new binding to reach the authorizer's informers.
func eventuallyCan(t *testing.T, id, verb, apiGroup, resource, namespace string) bool {
	t.Helper()
	for range 50 {
		if can(t, id, verb, apiGroup, resource, "", namespace) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestPolicyReaderGetsOnlyTheNamedPolicies checks the analyzer's read of the policies its requests name (#493):
// unbound (no --action-policies) no read of any policy; bound with the names the chart and
// config/analysis-policies set, get on exactly those, and no list, watch, write or other policy.
func TestPolicyReaderGetsOnlyTheNamedPolicies(t *testing.T) {
	ctx := context.Background()
	const group, resource, named = "remediation.shadewright.io", "remediationpolicies", "demo-memory"
	for _, verb := range []string{"get", "list", "watch"} {
		if can(t, analyzer, verb, group, resource, "", "") {
			t.Errorf("default analyzer: %s %s allowed, want refused", verb, resource)
		}
	}
	sa := "policy-reader"
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: operatorNS}}
	if err := admin.Create(ctx, account); err != nil {
		t.Fatalf("create ServiceAccount: %v", err)
	}
	r, err := readClusterRole(filepath.Join("..", "..", "config", "rbac", analyzer, "policies", "role.yaml"))
	if err != nil {
		t.Fatalf("read role: %v", err)
	}
	if len(r.Rules) != 1 || len(r.Rules[0].ResourceNames) != 0 {
		t.Fatalf("generated rules %+v: want one rule for the patch to restrict", r.Rules)
	}
	// The patch of config/analysis-policies (the chart renders the same rule, test/chart).
	r.Rules[0].ResourceNames = []string{named}
	r.Name = "test-" + r.Name
	if err := admin.Create(ctx, r); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := bindIn(ctx, r.Name, sa, ""); err != nil {
		t.Fatalf("bind role: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !canNamed(t, sa, "get", group, resource, "", "", named) {
		if time.Now().After(deadline) {
			t.Fatalf("bound: get %s %s refused, want allowed", resource, named)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if canNamed(t, sa, "get", group, resource, "", "", "other-policy") {
		t.Error("bound: get of a policy not named allowed")
	}
	for _, verb := range []string{"list", "watch", "create", "update", "patch", "delete"} {
		if can(t, sa, verb, group, resource, "", "") || canNamed(t, sa, verb, group, resource, "", "", named) {
			t.Errorf("bound: %s %s allowed, want refused", verb, resource)
		}
	}
}
