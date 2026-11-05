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
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	executorDeployment = "Deployment/" + operatorNS + "/shadewright-executor"
	analyzerDeployment = "Deployment/" + operatorNS + "/shadewright-analyzer"
	identityLabel      = "azure.workload.identity/use"
	componentLabel     = "app.kubernetes.io/component"
	mirrorSecret       = "mirror"
)

// chartVersions reads version and appVersion from Chart.yaml.
func chartVersions(t *testing.T) (version, appVersion string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "dist", "chart", "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v := regexp.MustCompile(`(?m)^version: ([^\s#]+)`).FindSubmatch(b)
	a := regexp.MustCompile(`(?m)^appVersion: "?([^"\s#]+)"?`).FindSubmatch(b)
	if v == nil || a == nil {
		t.Fatal("version or appVersion not found in Chart.yaml")
	}
	return string(v[1]), string(a[1])
}

// ciValues is the values file of chart-testing: the default values plus image.allowTag, since a source
// checkout has no digests.
func ciValues(t *testing.T) []string {
	return []string{"-f", filepath.Join(repoRoot(t), "dist", "chart", "ci", "default-values.yaml")}
}

// defaultRender renders the chart as chart-testing installs it: the default values with ci/default-values.yaml
// (image.allowTag) and a clusterId, since helm template asks no cluster for the kube-system UID.
func defaultRender(t *testing.T, set ...string) rendered {
	t.Helper()
	return renderArgs(t, helmArgsWith(t, ciValues(t), append([]string{"clusterId=chart-test"}, set...)...))
}

func container(t *testing.T, r rendered, k string) corev1.Container {
	t.Helper()
	u := r.chart[k]
	if u == nil {
		t.Fatalf("%s not rendered", k)
	}
	d := convert[appsv1.Deployment](t, u)
	if len(d.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("%s: want one container", k)
	}
	return d.Spec.Template.Spec.Containers[0]
}

// TestDefaultValuesWatchNothingAndHaveNoIdentity checks the install with no values (invariant 6 and the issue's
// default): the analyzer watches no namespace and has no RoleBinding outside the operator namespace, neither
// component has an Azure workload identity, so the executor cannot reach Key Vault to sign for the GitHub App,
// and with image.allowTag (chart-testing's only value) the images run by the appVersion tag.
func TestDefaultValuesWatchNothingAndHaveNoIdentity(t *testing.T) {
	_, appVersion := chartVersions(t)
	r := defaultRender(t)

	args := container(t, r, analyzerDeployment).Args
	if !slices.Contains(args, "--watch-namespaces=") {
		t.Errorf("analyzer args %v: want an explicit empty --watch-namespaces=", args)
	}
	if !slices.Contains(args, "--cluster-id=chart-test") {
		t.Errorf("analyzer args %v: want the explicit clusterId", args)
	}
	for k, u := range r.chart {
		if u.GetKind() == kindRoleBinding && u.GetNamespace() != operatorNS {
			t.Errorf("%s: a RoleBinding outside the operator namespace with no watched namespace", k)
		}
	}
	for _, comp := range components {
		dk := "Deployment/" + operatorNS + "/shadewright-" + comp
		c := container(t, r, dk)
		if want := "ghcr.io/pkalin/shadewright-" + comp + ":" + appVersion; c.Image != want {
			t.Errorf("%s: image %q, want %q (the appVersion tag without a digest)", dk, c.Image, want)
		}
		d := convert[appsv1.Deployment](t, r.chart[dk])
		if _, ok := d.Spec.Template.Labels[identityLabel]; ok {
			t.Errorf("%s: Pods carry %s with no identity set", dk, identityLabel)
		}
		sa := r.chart["ServiceAccount/"+operatorNS+"/shadewright-"+comp]
		if sa == nil {
			t.Fatalf("ServiceAccount of %s not rendered", comp)
		}
		for a := range sa.GetAnnotations() {
			if strings.HasPrefix(a, "azure.workload.identity/") {
				t.Errorf("ServiceAccount of %s carries %s with no identity set", comp, a)
			}
		}
	}
	if _, ok := r.chart["RemediationSettings//default"]; ok {
		t.Error("the settings object is rendered by default")
	}
}

// TestRBACEqualsKustomizeForEveryToggle renders the chart with each toggle of values.yaml and compares its RBAC
// with config/: equal, except for the objects a toggle is documented to leave out. No toggle may add anything.
func TestRBACEqualsKustomizeForEveryToggle(t *testing.T) {
	metricsAuth := []string{
		"ClusterRole//shadewright-metrics-auth-role",
		"ClusterRoleBinding//shadewright-metrics-auth-rolebinding",
		"ClusterRole//shadewright-metrics-reader",
	}
	cases := []struct {
		name    string
		helpers bool
		set     []string
		// defaults renders with the default values (no watched namespace): compare with config/default alone.
		defaults bool
		// without lists the kustomize objects the chart leaves out with this toggle.
		without []string
	}{
		{name: "defaults", defaults: true},
		{name: "defaults with helpers", defaults: true, helpers: true},
		{name: "test values"},
		{name: "metrics off", set: []string{"metrics.enabled=false"}, without: metricsAuth},
		{name: "metrics insecure", set: []string{"metrics.secure=false"}, without: metricsAuth},
		{name: "service monitor", set: []string{"metrics.serviceMonitor.enabled=true"}},
		{name: "network policy off", set: []string{"networkPolicy.enabled=false"}},
		{name: "crds off", set: []string{"crd.enabled=false"}},
		{name: "crds not kept", set: []string{"crd.keep=false"}},
		{name: "settings", set: []string{"settings.create=true", "settings.spec.paused=true"}},
		{name: "both identities", set: []string{"analyzer.identity.azure.clientId=11111111-1111-1111-1111-111111111111"}},
		{name: "max incidents", set: []string{"analyzer.maxIncidents=10"}},
		{name: "scheduling", set: []string{
			"executor.priorityClassName=high", "analyzer.nodeSelector.pool=system",
			"imagePullSecrets[0].name=" + mirrorSecret, "executor.imagePullSecrets[0].name=other",
		}},
		{name: "overrides", set: []string{"nameOverride=shadewright", "fullnameOverride=shadewright"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r rendered
			var want map[string]*unstructured.Unstructured
			if tc.defaults {
				r = defaultRender(t, append([]string{fmt.Sprintf("rbac.helpers.enabled=%t", tc.helpers)}, tc.set...)...)
				want = rbacOnly(r.kustomizeDefault, !tc.helpers)
			} else {
				r = render(t, tc.helpers, tc.set...)
				want = rbacOnly(r.kustomize, !tc.helpers)
			}
			if len(want) < 15 {
				t.Fatalf("only %d RBAC objects from kustomize: the comparison would prove nothing", len(want))
			}
			for _, k := range tc.without {
				if _, ok := want[k]; !ok {
					t.Fatalf("%s is not in config/: the case is out of date", k)
				}
				delete(want, k)
			}
			for _, p := range compareRBAC(t, want, rbacOnly(r.chart, false)) {
				t.Error(p)
			}
		})
	}
}

// TestClusterIDNeedsAValueWithoutACluster checks that helm template (no cluster, so lookup finds no kube-system
// namespace) fails with a message that names clusterId, and that the default comes from the kube-system UID.
func TestClusterIDNeedsAValueWithoutACluster(t *testing.T) {
	out, err := runErr(t, helmArgsWith(t, ciValues(t)))
	if err == nil {
		t.Fatalf("helm template without clusterId succeeded:\n%s", out)
	}
	if !strings.Contains(out, "clusterId is not set") || !strings.Contains(out, "set clusterId") {
		t.Errorf("error does not say how to set clusterId:\n%s", out)
	}
	src, rerr := os.ReadFile(filepath.Join(repoRoot(t), "dist", "chart", "templates", "_helpers.tpl"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(src), `lookup "v1" "Namespace" "" "kube-system"`) {
		t.Error("the clusterId default does not read the kube-system namespace with lookup")
	}
	// An explicit value wins.
	args := container(t, defaultRender(t), analyzerDeployment).Args
	if !slices.Contains(args, "--cluster-id=chart-test") {
		t.Errorf("args %v: the explicit clusterId is not passed", args)
	}
}

// runErr runs helm with args and returns its combined output and error, without failing the test.
func runErr(t *testing.T, args []string) (string, error) {
	t.Helper()
	out, err := execCombined(tool(t, "HELM", "helm"), repoRoot(t), args...)
	return string(out), err
}

// execCombined runs name with args in dir and returns its combined output.
func execCombined(name, dir string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// TestImageWithoutDigestFailsClosed checks that a source checkout without digests does not render unless
// image.allowTag opts in to the appVersion tag, and that ci/default-values.yaml holds that one key only.
func TestImageWithoutDigestFailsClosed(t *testing.T) {
	out, err := runErr(t, helmArgsWith(t, nil, "clusterId=chart-test"))
	if err == nil {
		t.Fatalf("helm template without digests or image.allowTag succeeded:\n%s", out)
	}
	if !strings.Contains(out, "image.digest is empty") || !strings.Contains(out, "image.allowTag=true") {
		t.Errorf("error does not say how to fix it:\n%s", out)
	}
	out, err = runErr(t, helmArgsWith(t, nil, "clusterId=chart-test", "image.allowTag=false",
		"executor.image.digest=sha256:"+strings.Repeat("1", 64)))
	if err == nil || !strings.Contains(out, "analyzer.image.digest is empty") {
		t.Errorf("one digest missing did not fail on that component:\n%s", out)
	}
	b, rerr := os.ReadFile(filepath.Join(repoRoot(t), "dist", "chart", "ci", "default-values.yaml"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var keys []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			keys = append(keys, strings.TrimSpace(l))
		}
	}
	if !slices.Equal(keys, []string{"image:", "allowTag: true"}) {
		t.Errorf("ci/default-values.yaml sets %v, want image.allowTag only (ct install must test the defaults)", keys)
	}
}

func TestImagesByDigestOrAppVersionTag(t *testing.T) {
	r := render(t, false)
	for _, comp := range components {
		c := container(t, r, "Deployment/"+operatorNS+"/shadewright-"+comp)
		if !regexp.MustCompile(`^ghcr\.io/pkalin/shadewright-` + comp + `@sha256:[a-f0-9]{64}$`).MatchString(c.Image) {
			t.Errorf("%s: image %q, want repository@digest from the values", comp, c.Image)
		}
	}
	for _, bad := range []string{"latest", "0.1.0", "sha256:abc"} {
		if out := helmFails(t, "executor.image.digest="+bad); !strings.Contains(out, "digest") {
			t.Errorf("digest %q: error does not name the digest:\n%s", bad, out)
		}
	}
}

// TestStandardValuesReachOnlyTheirComponent sets each standard Pod value on one component and checks that it
// reaches that Pod spec and not the other one, and that the global pull secrets are merged without duplicates.
func TestStandardValuesReachOnlyTheirComponent(t *testing.T) {
	const term = "executor.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0]"
	r := render(t, false,
		"imagePullSecrets[0].name="+mirrorSecret,
		"executor.imagePullSecrets[0].name="+mirrorSecret,
		"executor.imagePullSecrets[1].name=executor-only",
		"executor.priorityClassName=system-cluster-critical",
		"executor.nodeSelector.pool=system",
		"executor.podAnnotations.team=platform",
		"executor.tolerations[0].key=dedicated",
		"executor.tolerations[0].operator=Exists",
		"executor.tolerations[0].effect=NoSchedule",
		"executor.topologySpreadConstraints[0].maxSkew=1",
		"executor.topologySpreadConstraints[0].topologyKey=kubernetes.io/hostname",
		"executor.topologySpreadConstraints[0].whenUnsatisfiable=ScheduleAnyway",
		term+".matchExpressions[0].key=pool",
		term+".matchExpressions[0].operator=Exists",
	)
	ex := convert[appsv1.Deployment](t, r.chart[executorDeployment])
	an := convert[appsv1.Deployment](t, r.chart[analyzerDeployment])
	es, as := ex.Spec.Template.Spec, an.Spec.Template.Spec
	secrets := make([]string, 0, len(es.ImagePullSecrets))
	for _, s := range es.ImagePullSecrets {
		secrets = append(secrets, s.Name)
	}
	if !slices.Equal(secrets, []string{mirrorSecret, "executor-only"}) {
		t.Errorf("executor pull secrets %v, want mirror and executor-only once each", secrets)
	}
	if len(as.ImagePullSecrets) != 1 || as.ImagePullSecrets[0].Name != mirrorSecret {
		t.Errorf("analyzer pull secrets %v, want the global mirror only", as.ImagePullSecrets)
	}
	if es.PriorityClassName != "system-cluster-critical" || as.PriorityClassName != "" {
		t.Errorf("priorityClassName executor %q analyzer %q", es.PriorityClassName, as.PriorityClassName)
	}
	if es.NodeSelector["pool"] != "system" || len(as.NodeSelector) != 0 {
		t.Errorf("nodeSelector executor %v analyzer %v", es.NodeSelector, as.NodeSelector)
	}
	if ex.Spec.Template.Annotations["team"] != "platform" || an.Spec.Template.Annotations["team"] != "" {
		t.Errorf("podAnnotations executor %v analyzer %v", ex.Spec.Template.Annotations, an.Spec.Template.Annotations)
	}
	if len(es.Tolerations) != 1 || es.Tolerations[0].Key != "dedicated" || len(as.Tolerations) != 0 {
		t.Errorf("tolerations executor %v analyzer %v", es.Tolerations, as.Tolerations)
	}
	ets, ats := es.TopologySpreadConstraints, as.TopologySpreadConstraints
	if len(ets) != 1 || ets[0].TopologyKey != "kubernetes.io/hostname" || len(ats) != 0 {
		t.Errorf("topologySpreadConstraints executor %v analyzer %v", ets, ats)
	}
	if es.Affinity == nil || es.Affinity.NodeAffinity == nil || as.Affinity != nil {
		t.Errorf("affinity executor %v analyzer %v", es.Affinity, as.Affinity)
	}
}

// TestSchemaBoundsTheStandardValues checks that the schema refuses standard values outside their bounds.
func TestSchemaBoundsTheStandardValues(t *testing.T) {
	cases := map[string]refusalCase{
		"unknown toleration field":      {"executor.tolerations[0].keys=a", unknownField},
		"bad toleration operator":       {"executor.tolerations[0].operator=Maybe", "operator"},
		"unknown affinity kind":         {"analyzer.affinity.nodeAntiAffinity.a=b", unknownField},
		"spread without a policy":       {"executor.topologySpreadConstraints[0].maxSkew=1", "whenUnsatisfiable"},
		"bad priority class":            {"executor.priorityClassName=Not_A_Name", "priorityClassName"},
		"bad pull secret name":          {"imagePullSecrets[0].name=Bad_Name", "name"},
		"unknown pull secret field":     {"analyzer.imagePullSecrets[0].secret=a", unknownField},
		"all namespaces":                {"analyzer.watchNamespaces={*}", "watchNamespaces"},
		"service monitor bad interval":  {"metrics.serviceMonitor.interval=often", "interval"},
		"service monitor without port":  {"metrics.serviceMonitor.enabled=true,metrics.enabled=false", "metrics.enabled"},
		"test image by tag":             {"tests.image.digest=8.23.0", "digest"},
		"cluster ID with a bad charset": {"clusterId=my cluster", "clusterId"},
		"executor memory limit removed": {"executor.resources.limits.memory=null", "memory"},
		"analyzer memory limit removed": {"analyzer.resources.limits.memory=null", "memory"},
		"empty memory limit":            {"executor.resources.limits.memory=", "memory"},
		"zero memory limit":             {"analyzer.resources.limits.memory=0", "memory"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if out := helmFails(t, c.set); !strings.Contains(out, c.want) {
				t.Errorf("error does not mention %q:\n%s", c.want, out)
			}
		})
	}
}

type refusalCase struct {
	set  string
	want string
}

// TestEveryObjectHasTheStandardLabels renders every optional object, the helm test hooks included, and checks
// the app.kubernetes.io labels and helm.sh/chart on each one and on each Pod template.
func TestEveryObjectHasTheStandardLabels(t *testing.T) {
	version, appVersion := chartVersions(t)
	r := render(t, true, append(slices.Clone(allToggles), "metrics.serviceMonitor.enabled=true")...)
	want := map[string]string{
		"app.kubernetes.io/name":       chartName,
		"app.kubernetes.io/instance":   release,
		"app.kubernetes.io/version":    appVersion,
		"app.kubernetes.io/part-of":    chartName,
		"app.kubernetes.io/managed-by": "Helm",
		"helm.sh/chart":                "shadewright-" + version,
	}
	check := func(what string, got map[string]string) {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: label %s = %q, want %q", what, k, got[k], v)
			}
		}
	}
	kinds := map[string]bool{}
	for _, m := range []map[string]*unstructured.Unstructured{r.chart, r.tests} {
		for k, u := range m {
			kinds[u.GetKind()] = true
			check(k, u.GetLabels())
			if u.GetKind() == "Deployment" {
				check(k+" Pod template", convert[appsv1.Deployment](t, u).Spec.Template.Labels)
			}
		}
	}
	for _, kind := range []string{
		"CustomResourceDefinition", "Deployment", "ServiceAccount", kindRole, kindClusterRole, kindRoleBinding,
		kindClusterRoleBinding, "NetworkPolicy", "Service", "ConfigMap", "Lease", "RemediationSettings",
		"ServiceMonitor", "Pod",
	} {
		if !kinds[kind] {
			t.Errorf("no %s rendered: the label check does not cover it", kind)
		}
	}
}

// TestHelmTestHookNeedsNoPermissions checks templates/tests: one Pod without a ServiceAccount token that asks
// both metrics Services, and NetworkPolicies that let only that Pod reach only the metrics port, deleted by
// Helm after the test whatever its result. No RBAC object is a test hook.
func TestHelmTestHookNeedsNoPermissions(t *testing.T) {
	r := render(t, false)
	pod := testPod(t, r)
	checkTestPod(t, pod)
	pol := r.tests[testPodKey].GetAnnotations()["helm.sh/hook-delete-policy"]
	if !strings.Contains(pol, "hook-succeeded") {
		t.Errorf("test Pod delete policy %q", pol)
	}
	nps := 0
	for k, h := range r.tests {
		switch h.GetKind() {
		case "Pod":
		case "NetworkPolicy":
			nps++
			checkTestNetworkPolicy(t, k, h)
		default:
			t.Errorf("%s: a test hook of kind %s; the test needs no permissions", k, h.GetKind())
		}
	}
	if nps != 2 {
		t.Errorf("%d test NetworkPolicies, want 2 (egress of the test Pod, ingress to the metrics port)", nps)
	}
}

// TestHelmTestHookFollowsTheMetricsValues checks which test hooks each metrics and network policy value renders,
// and the answer the test Pod expects.
func TestHelmTestHookFollowsTheMetricsValues(t *testing.T) {
	r := render(t, false, "imagePullSecrets[0].name="+mirrorSecret, "tests.imagePullSecrets[0].name="+mirrorSecret,
		"tests.imagePullSecrets[1].name=tests-only")
	pod := testPod(t, r)
	secrets := make([]string, 0, len(pod.Spec.ImagePullSecrets))
	for _, s := range pod.Spec.ImagePullSecrets {
		secrets = append(secrets, s.Name)
	}
	if !slices.Equal(secrets, []string{mirrorSecret, "tests-only"}) {
		t.Errorf("test Pod pull secrets %v, want mirror and tests-only once each", secrets)
	}
	if r := render(t, false, "networkPolicy.enabled=false"); len(r.tests) != 1 {
		t.Errorf("networkPolicy.enabled=false renders test hooks %v, want the Pod only", keys(r.tests))
	}
	if r := render(t, false, "metrics.enabled=false"); len(r.tests) != 0 {
		t.Errorf("metrics.enabled=false renders test hooks %v, want none", keys(r.tests))
	}
	if got := envOf(testPod(t, render(t, false, "metrics.secure=false")))["WANT"]; got != "200" {
		t.Errorf("WANT = %q with metrics.secure=false, want 200", got)
	}
}

const testPodKey = "Pod/" + operatorNS + "/shadewright-test-metrics"

func testPod(t *testing.T, r rendered) *corev1.Pod {
	t.Helper()
	u := r.tests[testPodKey]
	if u == nil {
		t.Fatalf("%s not rendered; tests: %v", testPodKey, keys(r.tests))
	}
	return convert[corev1.Pod](t, u)
}

func envOf(pod *corev1.Pod) map[string]string {
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

func checkTestPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("the test Pod mounts a ServiceAccount token")
	}
	if pod.Spec.ServiceAccountName != "" {
		t.Errorf("the test Pod runs as %q, want the namespace default with no token", pod.Spec.ServiceAccountName)
	}
	psc := pod.Spec.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("the test Pod may run as root")
	}
	c := pod.Spec.Containers[0]
	if !regexp.MustCompile(`@sha256:[a-f0-9]{64}$`).MatchString(c.Image) {
		t.Errorf("test image %q is not pinned by digest", c.Image)
	}
	sc := c.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, "ALL") {
		t.Errorf("test container is not hardened: %+v", sc)
	}
	env := envOf(pod)
	if env["WANT"] != "401" {
		t.Errorf("WANT = %q, want 401: a secure endpoint must refuse a scrape without a token", env["WANT"])
	}
	for _, name := range []string{"EXECUTOR_URL", "ANALYZER_URL"} {
		if !strings.HasPrefix(env[name], "https://shadewright-") || !strings.HasSuffix(env[name], ".svc:8443/metrics") {
			t.Errorf("%s = %q, want the https metrics Service", name, env[name])
		}
	}
}

func checkTestNetworkPolicy(t *testing.T, k string, h *unstructured.Unstructured) {
	t.Helper()
	if pol := h.GetAnnotations()["helm.sh/hook-delete-policy"]; !strings.Contains(pol, "hook-succeeded") ||
		!strings.Contains(pol, "hook-failed") {
		t.Errorf("%s: delete policy %q, want it deleted after the test whatever its result", k, pol)
	}
	np := convert[networkingv1.NetworkPolicy](t, h)
	for _, rule := range np.Spec.Egress {
		dns := false
		for _, p := range rule.Ports {
			dns = dns || (p.Port != nil && p.Port.IntValue() == 53)
		}
		if dns && len(rule.To) != 0 {
			t.Errorf("%s: DNS egress limited to %+v; NodeLocal DNSCache and other CoreDNS labels need any destination",
				k, rule.To)
		}
	}
	for _, rule := range np.Spec.Ingress {
		for _, p := range rule.Ports {
			if p.Port == nil || p.Port.IntValue() != 8443 {
				t.Errorf("%s: ingress port %v, want the metrics port only", k, p.Port)
			}
		}
		for _, from := range rule.From {
			if from.PodSelector == nil || from.PodSelector.MatchLabels[componentLabel] != "test" ||
				from.NamespaceSelector != nil {
				t.Errorf("%s: ingress from %+v, want the test Pod of this namespace only", k, from)
			}
		}
	}
}

func keys(m map[string]*unstructured.Unstructured) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestServiceMonitorSelectsEachMetricsService checks the optional ServiceMonitors: off by default, one per
// component when on, each selecting exactly its component's metrics Service and port, and extra labels that
// cannot replace the chart's own.
func TestServiceMonitorSelectsEachMetricsService(t *testing.T) {
	for k, u := range render(t, false).chart {
		if u.GetKind() == "ServiceMonitor" {
			t.Errorf("%s rendered with metrics.serviceMonitor.enabled false", k)
		}
	}
	r := render(t, false, "metrics.serviceMonitor.enabled=true", "metrics.serviceMonitor.interval=30s",
		"metrics.serviceMonitor.labels.release=prometheus", `metrics.serviceMonitor.labels.app\.kubernetes\.io/name=other`)
	services := map[string]*corev1.Service{}
	for _, u := range r.chart {
		if u.GetKind() == "Service" {
			services[u.GetName()] = convert[corev1.Service](t, u)
		}
	}
	for _, comp := range components {
		sm := r.chart["ServiceMonitor/"+operatorNS+"/shadewright-"+comp+"-metrics"]
		if sm == nil {
			t.Fatalf("no ServiceMonitor for %s", comp)
		}
		if sm.GetLabels()["release"] != "prometheus" || sm.GetLabels()["app.kubernetes.io/name"] != chartName {
			t.Errorf("%s ServiceMonitor labels %v: want release=prometheus and the chart's own name", comp, sm.GetLabels())
		}
		sel, _, _ := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
		var matched []string
		for name, svc := range services {
			ok := len(sel) > 0
			for k, v := range sel {
				if svc.Labels[k] != v {
					ok = false
				}
			}
			if ok {
				matched = append(matched, name)
			}
		}
		if want := "shadewright-" + comp + "-metrics-service"; len(matched) != 1 || matched[0] != want {
			t.Errorf("%s ServiceMonitor selects %v, want only %s", comp, matched, want)
		}
		eps, _, _ := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
		if len(eps) != 1 {
			t.Fatalf("%s ServiceMonitor endpoints %v", comp, eps)
		}
		ep := eps[0].(map[string]any)
		svc := services["shadewright-"+comp+"-metrics-service"]
		if ep["port"] != svc.Spec.Ports[0].Name || ep["scheme"] != "https" || ep["interval"] != "30s" {
			t.Errorf("%s ServiceMonitor endpoint %v, want port %s over https every 30s", comp, ep, svc.Spec.Ports[0].Name)
		}
	}
}
