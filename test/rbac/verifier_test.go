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
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/cmdutil"
)

const (
	verifierRoleFile = "role.yaml"
	kindRoleBinding  = "RoleBinding"
)

// TestVerifierRulesAreExactlySection7 reads every role in config/rbac/verifier and checks that together they
// grant exactly the rules of ADR-0030 section 7: list on pods and get on replicasets, deployments and
// statefulsets (the watch ClusterRole, bound per namespace); get, list and watch on remediationrequests, get and
// create on verificationreports and update on verificationreports/status (a Role in the operator namespace);
// its own Lease and core Events. No ClusterRoleBinding, no wildcard, nothing else.
//
//nolint:goconst // literals keep the table readable
func TestVerifierRulesAreExactlySection7(t *testing.T) {
	type key struct{ kind, group, resource, verb, name string }
	want := map[key]bool{
		{kindCR, "", "pods", "list", ""}:                                             true,
		{kindCR, "apps", "replicasets", "get", ""}:                                   true,
		{kindCR, "apps", "deployments", "get", ""}:                                   true,
		{kindCR, "apps", "statefulsets", "get", ""}:                                  true,
		{kindRole, group, "remediationrequests", "get", ""}:                          true,
		{kindRole, group, "remediationrequests", "list", ""}:                         true,
		{kindRole, group, "remediationrequests", "watch", ""}:                        true,
		{kindRole, group, "verificationreports", "get", ""}:                          true,
		{kindRole, group, "verificationreports", "create", ""}:                       true,
		{kindRole, group, "verificationreports/status", "update", ""}:                true,
		{kindRole, "coordination.k8s.io", "leases", "get", cmdutil.VerifierLease}:    true,
		{kindRole, "coordination.k8s.io", "leases", "update", cmdutil.VerifierLease}: true,
		{kindRole, "coordination.k8s.io", "leases", "patch", cmdutil.VerifierLease}:  true,
		{kindRole, "", "events", "create", ""}:                                       true,
		{kindRole, "", "events", "patch", ""}:                                        true,
	}
	got := map[key]bool{}
	root := filepath.Join("..", "..", "config", "rbac", verifierID)
	for _, file := range []string{verifierRoleFile, "leader_election.yaml", "role_binding.yaml", "service_account.yaml",
		filepath.Join("watch", verifierRoleFile)} {
		path := filepath.Join(root, file)
		for _, u := range decodeAll(t, path) {
			if u["kind"] == "ClusterRoleBinding" {
				t.Errorf("%s: a ClusterRoleBinding; the verifier is bound per namespace only", path)
			}
			rules, aggregated, ok := policyRules(t, path, u)
			if !ok {
				continue
			}
			if aggregated {
				t.Errorf("%s: an aggregated ClusterRole", path)
			}
			kind := u["kind"].(string)
			for _, r := range rules {
				names := r.ResourceNames
				if len(names) == 0 {
					names = []string{""}
				}
				for _, g := range r.APIGroups {
					for _, res := range r.Resources {
						for _, v := range r.Verbs {
							for _, n := range names {
								if g == "*" || res == "*" || v == "*" || strings.HasSuffix(res, "/*") {
									t.Errorf("%s: wildcard in %v", path, r)
								}
								got[key{kind, g, res, v, n}] = true
							}
						}
					}
				}
			}
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("the verifier is granted %+v, which section 7 does not name", k)
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("the verifier lacks %+v of section 7", k)
		}
	}
}

// TestVerifierBoundary asks the API server what the verifier's ServiceAccount may do with the roles bound as
// config/rbac/verifier and config/watch bind them.
//
//nolint:goconst // literals keep the table readable
func TestVerifierBoundary(t *testing.T) {
	cases := []struct {
		verb, group, resource, sub, ns string
		want                           bool
	}{
		// Pods: list only, in the watched namespace only; never get or watch, never cluster-wide.
		{"list", "", "pods", "", otherNS, true},
		{"get", "", "pods", "", otherNS, false},
		{"watch", "", "pods", "", otherNS, false},
		{"get", "", "pods", "log", otherNS, false},
		{"list", "", "pods", "", unwatched, false},
		{"list", "", "pods", "", operatorNS, false},
		{"list", "", "pods", "", "", false},
		{"delete", "", "pods", "", otherNS, false},
		{"create", "", "pods", "eviction", otherNS, false},
		// Workloads: get only, in the watched namespace only.
		{"get", "apps", "replicasets", "", otherNS, true},
		{"get", "apps", "deployments", "", otherNS, true},
		{"get", "apps", "statefulsets", "", otherNS, true},
		{"list", "apps", "replicasets", "", otherNS, false},
		{"watch", "apps", "deployments", "", otherNS, false},
		{"update", "apps", "deployments", "", otherNS, false},
		{"patch", "apps", "statefulsets", "", otherNS, false},
		{"update", "apps", "deployments", "scale", otherNS, false},
		{"get", "apps", "deployments", "", unwatched, false},
		{"get", "apps", "daemonsets", "", otherNS, false},
		// Requests: read only, in the operator namespace; never their status.
		{"get", group, "remediationrequests", "", operatorNS, true},
		{"list", group, "remediationrequests", "", operatorNS, true},
		{"watch", group, "remediationrequests", "", operatorNS, true},
		{"list", group, "remediationrequests", "", otherNS, false},
		{"create", group, "remediationrequests", "", operatorNS, false},
		{"update", group, "remediationrequests", "", operatorNS, false},
		{"patch", group, "remediationrequests", "", operatorNS, false},
		{"update", group, "remediationrequests", "status", operatorNS, false},
		{"patch", group, "remediationrequests", "status", operatorNS, false},
		// Reports: get and create, update of status only, in the operator namespace.
		{"get", group, "verificationreports", "", operatorNS, true},
		{"create", group, "verificationreports", "", operatorNS, true},
		{"update", group, "verificationreports", "status", operatorNS, true},
		{"patch", group, "verificationreports", "status", operatorNS, false},
		{"update", group, "verificationreports", "", operatorNS, false},
		{"patch", group, "verificationreports", "", operatorNS, false},
		{"delete", group, "verificationreports", "", operatorNS, false},
		{"list", group, "verificationreports", "", operatorNS, false},
		{"watch", group, "verificationreports", "", operatorNS, false},
		{"create", group, "verificationreports", "", otherNS, false},
		// Nothing else of Shadewright, no Secrets, no ConfigMaps.
		{"get", group, "incidents", "", operatorNS, false},
		{"get", group, "remediationpolicies", "", "", false},
		{"get", group, "remediationsettings", "", "", false},
		{"get", "", "secrets", "", operatorNS, false},
		{"list", "", "secrets", "", otherNS, false},
		{"get", "", "configmaps", "", operatorNS, false},
		{"list", "", "nodes", "", "", false},
		{"get", "keda.sh", "scaledobjects", "", otherNS, false},
		// Core Events in the operator namespace only, as the other identities.
		{"create", "", "events", "", operatorNS, true},
		{"create", "", "events", "", otherNS, false},
		// The other identities get nothing of the verifier's.
		{"create", group, "verificationreports", "", operatorNS, false},
	}
	for i, c := range cases {
		id := verifierID
		if i == len(cases)-1 {
			id = executor
		}
		if got := can(t, id, c.verb, c.group, c.resource, c.sub, c.ns); got != c.want {
			t.Errorf("%s %s %s/%s %q in %q: allowed=%v, want %v", id, c.verb, c.group, c.resource, c.sub, c.ns, got, c.want)
		}
	}
	for _, id := range []string{executor, analyzer} {
		for _, verb := range []string{"create", "update"} {
			sub := ""
			if verb == "update" {
				sub = "status"
			}
			if can(t, id, verb, group, "verificationreports", sub, operatorNS) {
				t.Errorf("%s may %s verificationreports %s", id, verb, sub)
			}
		}
	}
}

// TestVerifierRealCalls makes the verifier's own writes as its ServiceAccount: it creates a report and updates
// its status; it cannot write the status of a request.
func TestVerifierRealCalls(t *testing.T) {
	ctx := t.Context()
	asVerifier := impersonate(t, verifierID)
	req := newRequest(operatorNS, "verified")
	if err := admin.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(ctx, req) })

	report := &remediationv1alpha1.VerificationReport{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: "vr-" + string(req.UID)},
		Spec: remediationv1alpha1.VerificationReportSpec{RequestUID: remediationv1alpha1.UID(req.UID),
			MergeCommitSHA: strings.Repeat("a", 40), TargetUID: "33333333-3333-4333-8333-333333333333", TemplateRevision: "1"},
	}
	if err := asVerifier.Create(ctx, report); err != nil {
		t.Fatalf("verifier cannot create a report: %v", err)
	}
	t.Cleanup(func() { _ = admin.Delete(ctx, report) })
	got := &remediationv1alpha1.VerificationReport{}
	if err := asVerifier.Get(ctx, client.ObjectKeyFromObject(report), got); err != nil {
		t.Fatalf("verifier cannot read its report: %v", err)
	}
	got.Status.Sample = 1
	if err := asVerifier.Status().Update(ctx, got); err != nil {
		t.Errorf("verifier cannot update the report status: %v", err)
	}
	if err := asVerifier.Delete(ctx, got); !apierrors.IsForbidden(err) {
		t.Errorf("verifier deleting a report = %v, want forbidden", err)
	}

	var fresh remediationv1alpha1.RemediationRequest
	if err := asVerifier.Get(ctx, client.ObjectKeyFromObject(req), &fresh); err != nil {
		t.Fatalf("verifier cannot read the request: %v", err)
	}
	fresh.Status.Phase = remediationv1alpha1.PhasePolicyChecked
	if err := asVerifier.Status().Update(ctx, &fresh); !apierrors.IsForbidden(err) {
		t.Errorf("verifier writing a request's status = %v, want forbidden", err)
	}
}

// TestVerifierWatchBindingsMatchTheFlag guards against drift between the verifier's --watch-namespaces
// (config/manager/verifier.yaml) and its RoleBindings in config/watch: one per namespace, each a RoleBinding of
// the prefixed verifier-watch-role to the prefixed verifier ServiceAccount, never a ClusterRoleBinding.
func TestVerifierWatchBindingsMatchTheFlag(t *testing.T) {
	root := filepath.Join("..", "..", "config")
	flagValue, found := "", false
	for _, doc := range decodeAll(t, filepath.Join(root, "manager", "verifier.yaml")) {
		var d appsv1.Deployment
		if err := decodeInto(doc, &d); err != nil {
			t.Fatal(err)
		}
		for _, ct := range d.Spec.Template.Spec.Containers {
			for _, a := range ct.Args {
				if v, ok := strings.CutPrefix(a, "--watch-namespaces="); ok {
					flagValue, found = v, true
				}
			}
		}
	}
	if !found {
		t.Fatal("config/manager/verifier.yaml passes no --watch-namespaces")
	}
	want, err := cmdutil.ParseNamespaces(flagValue)
	if err != nil {
		t.Fatalf("config/manager/verifier.yaml: %v", err)
	}
	var got []string
	for _, res := range kustomizeResources(t, filepath.Join(root, "watch", "kustomization.yaml")) {
		for _, doc := range decodeAll(t, filepath.Join(root, "watch", res)) {
			var b rbacv1.RoleBinding
			if err := decodeInto(doc, &b); err != nil {
				t.Fatal(err)
			}
			if b.RoleRef.Name != "shadewright-verifier-watch-role" {
				if slices.ContainsFunc(b.Subjects, func(s rbacv1.Subject) bool { return s.Name == "shadewright-verifier" }) {
					t.Errorf("%s: binds %s to the verifier", res, b.RoleRef.Name)
				}
				continue
			}
			if b.Kind != kindRoleBinding || b.RoleRef.Kind != kindCR {
				t.Errorf("%s: %s of %s %s, want a RoleBinding of the ClusterRole", res, b.Kind, b.RoleRef.Kind, b.RoleRef.Name)
			}
			if b.Name != "shadewright-verifier-watch" {
				t.Errorf("%s: name %q, want shadewright-verifier-watch", res, b.Name)
			}
			if len(b.Subjects) != 1 || b.Subjects[0].Name != "shadewright-verifier" || b.Subjects[0].Namespace != operatorNS {
				t.Errorf("%s: subjects %v, want the ServiceAccount shadewright-verifier in %s", res, b.Subjects, operatorNS)
			}
			got = append(got, b.Namespace)
		}
	}
	slices.Sort(got)
	if len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("config/watch binds verifier-watch-role in %v, the verifier's --watch-namespaces is %v", got, want)
	}
}

func kustomizeResources(t *testing.T, path string) []string {
	t.Helper()
	docs := decodeAll(t, path)
	if len(docs) != 1 {
		t.Fatalf("%s: want one document", path)
	}
	var out []string
	if rs, ok := docs[0]["resources"].([]any); ok {
		for _, r := range rs {
			out = append(out, r.(string))
		}
	}
	return out
}
