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

package chart

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// verifierSA is the ServiceAccount name the chart gives the verifier in the test release.
const verifierSA = release + "-" + verifierComp

// verifierWatchBindings returns the namespaces of the verifier-watch RoleBindings, sorted.
func verifierWatchBindings(r rendered) []string {
	var ns []string
	for _, u := range r.chart {
		if u.GetKind() == kindRoleBinding && u.GetName() == verifierSA+"-watch" {
			ns = append(ns, u.GetNamespace())
		}
	}
	slices.Sort(ns)
	return ns
}

func verifierWatchFlag(t *testing.T, r rendered) string {
	t.Helper()
	var found []string
	for _, a := range container(t, r, verifierDeployment).Args {
		if v, ok := strings.CutPrefix(a, "--watch-namespaces="); ok {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one --watch-namespaces flag, got %v", found)
	}
	return found[0]
}

// TestVerifierRulesAreExactlyTheDecidedSet (ADR-0030 section 7): the rendered rules equal the grants, nothing
// more, in the three Roles and the one ClusterRole.
func TestVerifierRulesAreExactlyTheDecidedSet(t *testing.T) {
	r := render(t, false)
	rep := "remediation.shadewright.io"
	for k, want := range map[string][]string{
		"ClusterRole/" + "/" + verifierSA + "-watch-role": {
			"|pods|list|*all*",
			"apps|deployments|get|*all*", "apps|replicasets|get|*all*", "apps|statefulsets|get|*all*",
		},
		"Role/" + operatorNS + "/" + verifierSA + "-role": {
			rep + "|remediationrequests|get|*all*", rep + "|remediationrequests|list|*all*",
			rep + "|remediationrequests|watch|*all*",
			rep + "|verificationreports|create|*all*", rep + "|verificationreports|get|*all*",
			rep + "|verificationreports/status|update|*all*",
		},
		"Role/" + operatorNS + "/" + verifierSA + "-leader-election-role": {
			"coordination.k8s.io|leases|get|shadewright-verifier-leader-election",
			"coordination.k8s.io|leases|patch|shadewright-verifier-leader-election",
			"coordination.k8s.io|leases|update|shadewright-verifier-leader-election",
			"|events|create|*all*", "|events|patch|*all*",
		},
	} {
		u := r.chart[k]
		if u == nil {
			t.Fatalf("%s not rendered", k)
		}
		want = slices.Clone(want)
		slices.Sort(want)
		if missing, extra := diffLines(want, describe(t, u)); len(missing)+len(extra) != 0 {
			t.Errorf("%s: missing %v, extra %v", k, missing, extra)
		}
	}
	// The Lease the leader election Role names exists, with that exact name.
	if r.chart["Lease/"+operatorNS+"/shadewright-verifier-leader-election"] == nil {
		t.Error("the verifier Lease is not rendered")
	}
}

// TestVerifierWatchBindingsEqualTheFlag: one RoleBinding per namespace of the list the flag carries; null falls
// back to analyzer.watchNamespaces; a list of its own, [] included, is used as it is.
func TestVerifierWatchBindingsEqualTheFlag(t *testing.T) {
	// helm --set cannot write an empty list, so those cases come from a values file.
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(empty, []byte("verifier:\n  watchNamespaces: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const analyzerAB = "analyzer.watchNamespaces={a,b}"
	for name, tc := range map[string]struct {
		files []string
		set   []string
		want  []string
	}{
		"null follows the analyzer":  {nil, []string{analyzerAB}, []string{"a", "b"}},
		"null and an empty analyzer": {nil, []string{"analyzer.watchNamespaces=null"}, nil},
		"own list wins":              {nil, []string{analyzerAB, "verifier.watchNamespaces={c}"}, []string{"c"}},
		"empty list is used as is":   {[]string{"-f", empty}, []string{analyzerAB}, nil},
		"own list, analyzer empty": {nil, []string{"analyzer.watchNamespaces=null", "verifier.watchNamespaces={c,d}"},
			[]string{"c", "d"}},
	} {
		t.Run(name, func(t *testing.T) {
			files := append([]string{"-f", testValues(t)}, tc.files...)
			args := helmArgsWith(t, files, append([]string{"rbac.helpers.enabled=false"}, tc.set...)...)
			r := renderArgs(t, args)
			got := verifierWatchBindings(r)
			if !slices.Equal(got, tc.want) {
				t.Errorf("RoleBindings in %v, want %v", got, tc.want)
			}
			if flag := verifierWatchFlag(t, r); flag != strings.Join(tc.want, ",") {
				t.Errorf("--watch-namespaces=%q, want %q", flag, strings.Join(tc.want, ","))
			}
			for _, ns := range got {
				b := convert[rbacv1.RoleBinding](t, r.chart["RoleBinding/"+ns+"/"+verifierSA+"-watch"])
				if b.RoleRef.Kind != kindClusterRole || b.RoleRef.Name != verifierSA+"-watch-role" ||
					len(b.Subjects) != 1 || b.Subjects[0].Name != verifierSA || b.Subjects[0].Namespace != operatorNS {
					t.Errorf("binding in %s: %v %v", ns, b.RoleRef, b.Subjects)
				}
			}
		})
	}
}

// TestVerifierWatchListIsValidated: the same rules as analyzer.watchNamespaces.
func TestVerifierWatchListIsValidated(t *testing.T) {
	many := make([]string, 257)
	for i := range many {
		many[i] = fmt.Sprintf("ns%d", i)
	}
	for name, set := range map[string]string{
		"wildcard":   "verifier.watchNamespaces={*}",
		"duplicates": "verifier.watchNamespaces={a,a}",
		"257 names":  "verifier.watchNamespaces={" + strings.Join(many, ",") + "}",
		"not a name": "verifier.watchNamespaces={Not_A_Name}",
		"a string":   "verifier.watchNamespaces=demo",
	} {
		t.Run(name, func(t *testing.T) { helmFails(t, set) })
	}
}

// TestNoClusterRoleBindingNamesTheVerifierExceptMetricsAuth: the pod and workload reads are bound per
// namespace only, never cluster-wide (ADR-0030 section 7).
func TestNoClusterRoleBindingNamesTheVerifierExceptMetricsAuth(t *testing.T) {
	r := render(t, false)
	for k, u := range r.chart {
		if u.GetKind() != kindClusterRoleBinding {
			continue
		}
		b := convert[rbacv1.ClusterRoleBinding](t, u)
		for _, s := range b.Subjects {
			if s.Name == verifierSA && u.GetName() != release+"-metrics-auth-rolebinding" {
				t.Errorf("%s binds the verifier ServiceAccount cluster-wide", k)
			}
		}
	}
	mb := convert[rbacv1.ClusterRoleBinding](t, r.chart["ClusterRoleBinding//"+release+"-metrics-auth-rolebinding"])
	if !slices.ContainsFunc(mb.Subjects, func(s rbacv1.Subject) bool { return s.Name == verifierSA }) {
		t.Error("the verifier is no subject of the metrics-auth ClusterRoleBinding")
	}
	// With insecure metrics there is no metrics-auth binding at all.
	r = render(t, false, "metrics.secure=false")
	if u := r.chart["ClusterRoleBinding//"+release+"-metrics-auth-rolebinding"]; u != nil {
		t.Error("metrics-auth binding rendered with metrics.secure=false")
	}
}

// TestVerifierHasNoCloudIdentity: no workload identity label or annotation, and the values that would add one
// do not exist.
func TestVerifierHasNoCloudIdentity(t *testing.T) {
	r := render(t, false)
	d := convert[appsv1.Deployment](t, r.chart[verifierDeployment])
	for k := range d.Spec.Template.Labels {
		if strings.HasPrefix(k, "azure.workload.identity/") {
			t.Errorf("verifier Pod label %s", k)
		}
	}
	if sa := r.chart["ServiceAccount/"+operatorNS+"/"+verifierSA]; sa == nil || len(sa.GetAnnotations()) != 0 {
		t.Error("verifier ServiceAccount must exist and carry no annotation")
	}
	if c := container(t, r, verifierDeployment); c.Command[0] != "/verifier" {
		t.Errorf("command %v, want /verifier", c.Command)
	}
	for _, set := range []string{
		"verifier.identity.azure.clientId=x", "verifier.serviceAccount.annotations.a=b",
		`verifier.podLabels.azure\.workload\.identity/use=true`,
	} {
		helmFails(t, set)
	}
}

// TestVerifierImageFollowsItsOwnValues: repository and digest of its own, and a missing digest fails closed.
func TestVerifierImageFollowsItsOwnValues(t *testing.T) {
	r := render(t, false)
	if got, want := container(t, r, verifierDeployment).Image,
		"ghcr.io/pkalin/shadewright-verifier@sha256:"+strings.Repeat("3", 64); got != want {
		t.Errorf("image %s, want %s", got, want)
	}
	out, err := runErr(t, helmArgsWith(t, nil, "clusterId=chart-test", "image.allowTag=false",
		"executor.image.digest=sha256:"+strings.Repeat("1", 64),
		"analyzer.image.digest=sha256:"+strings.Repeat("2", 64)))
	if err == nil || !strings.Contains(out, "verifier.image.digest is empty") {
		t.Errorf("a missing verifier digest did not fail on it:\n%s", out)
	}
}

// TestVerifierMetricsAndMonitor: a metrics Service and a ServiceMonitor like the other components', and none
// without metrics.
func TestVerifierMetricsAndMonitor(t *testing.T) {
	r := render(t, false, "metrics.serviceMonitor.enabled=true")
	for _, k := range []string{
		"Service/" + operatorNS + "/" + verifierSA + "-metrics-service",
		"ServiceMonitor/" + operatorNS + "/" + verifierSA + "-metrics",
	} {
		if r.chart[k] == nil {
			t.Errorf("%s not rendered", k)
		}
	}
	r = render(t, false, "metrics.enabled=false")
	if r.chart["Service/"+operatorNS+"/"+verifierSA+"-metrics-service"] != nil {
		t.Error("metrics Service rendered with metrics.enabled=false")
	}
	for _, a := range container(t, r, verifierDeployment).Args {
		if strings.HasPrefix(a, "--metrics") {
			t.Errorf("flag %s with metrics.enabled=false", a)
		}
	}
}

// TestVerifierHelmTestChecksItsMetricsToo: the helm test Pod asks the verifier's Service and the test
// NetworkPolicies let it through.
func TestVerifierHelmTestChecksItsMetricsToo(t *testing.T) {
	r := render(t, false)
	pod := r.tests["Pod/"+operatorNS+"/"+release+"-test-metrics"]
	if pod == nil {
		t.Fatal("helm test Pod not rendered")
	}
	b, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	if !strings.Contains(fmt.Sprint(b), "VERIFIER_URL") {
		t.Error("the helm test Pod does not ask the verifier's metrics Service")
	}
	for _, k := range []string{testEgressKey, "NetworkPolicy/" + operatorNS + "/" + release + "-test-ingress"} {
		if u := r.tests[k]; u == nil || !strings.Contains(asJSON(t, u.Object), verifierComp) {
			t.Errorf("%s does not name the verifier", k)
		}
	}
}

// TestVerifierExtraArgsRefuseReservedFlags names the two the verifier chart sets from values.
func TestVerifierExtraArgsRefuseReservedFlags(t *testing.T) {
	for _, f := range []string{"namespace", "watch-namespaces", "leader-elect"} {
		if out := helmFails(t, "verifier.extraArgs={--"+f+"=x}"); !strings.Contains(out, "--"+f) {
			t.Errorf("--%s: error does not name it:\n%s", f, out)
		}
	}
	run(t, repoRoot(t), tool(t, "HELM", "helm"), helmArgs(t, false, "verifier.extraArgs={--zap-log-level=debug}")...)
}
