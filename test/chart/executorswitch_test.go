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
	"bytes"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

const (
	executorOff = "executor.enabled=false"
	// executorName is the name every executor object carries (the ServiceAccount, the Deployment, the Roles).
	executorName = "shadewright-executor"
	metricsAuth  = "ClusterRoleBinding//shadewright-metrics-auth-rolebinding"
	analyzerName = "shadewright-analyzer"
	// The values and the keys they name that the refusal tests use more than once.
	replicas2 = "executor.replicas"
	hintsKey  = "executor.reviewerHints.enabled"
	verifKey  = "executor.verification.enabled"
	id0       = "00000000-0000-0000-0000-000000000000"
)

// withoutExecutor is the value set of an analyzer-only install with the extras that render executor objects:
// the ServiceMonitors and the verifier.
var withoutExecutor = []string{executorOff, "metrics.serviceMonitor.enabled=true", withVerifier}

// allObjects lists the chart objects and the test hooks, which a test must see too.
func allObjects(r rendered) map[string]string {
	out := map[string]string{}
	for k, u := range r.chart {
		b, _ := json.Marshal(u.Object)
		out[k] = string(b)
	}
	for k, u := range r.tests {
		b, _ := json.Marshal(u.Object)
		out[k] = string(b)
	}
	return out
}

// TestExecutorOffRendersNoExecutorObject: executor.enabled=false leaves no object that is named for the
// executor, carries its label or lists its ServiceAccount as a subject, in the chart or in the helm test, and
// keeps every object of the analyzer as the default render has it.
func TestExecutorOffRendersNoExecutorObject(t *testing.T) {
	def := defaultRender(t, "metrics.serviceMonitor.enabled=true", withVerifier)
	off := defaultRender(t, withoutExecutor...)
	offObjects := allObjects(off)
	for k, body := range offObjects {
		named := strings.Contains(k, "executor") || strings.Contains(body, executorName)
		labelled := strings.Contains(body, `"control-plane":"executor"`) ||
			strings.Contains(body, `"app.kubernetes.io/component":"executor"`)
		if named || labelled {
			if strings.HasPrefix(k, "CustomResourceDefinition") {
				continue // the CRDs describe the executor in their field text
			}
			t.Errorf("%s still names the executor: %s", k, body)
		}
	}
	// The default render has the executor's objects, so the check above can fail.
	var gone []string
	for k := range allObjects(def) {
		if strings.Contains(k, "executor") {
			gone = append(gone, k)
		}
	}
	for _, want := range []string{
		executorDeployment, "ServiceAccount/" + operatorNS + "/" + executorName,
		"Service/" + operatorNS + "/shadewright-executor-metrics-service",
		"ServiceMonitor/" + operatorNS + "/shadewright-executor-metrics",
		"NetworkPolicy/" + operatorNS + "/shadewright-allow-executor-egress",
		"Lease/" + operatorNS + "/shadewright-executor-leader-election",
		"ClusterRole//shadewright-executor-role", "Role/" + operatorNS + "/shadewright-executor-role",
		"ClusterRoleBinding//shadewright-executor-rolebinding",
		"RoleBinding/" + operatorNS + "/shadewright-executor-rolebinding",
		"Role/" + operatorNS + "/shadewright-executor-leader-election-role",
		"RoleBinding/" + operatorNS + "/shadewright-executor-leader-election-rolebinding",
	} {
		if !slices.Contains(gone, want) {
			t.Errorf("the default render lacks %s, so the test would prove nothing for it", want)
		}
		if _, ok := offObjects[want]; ok {
			t.Errorf("%s is rendered with executor.enabled=false", want)
		}
	}
	// What the analyzer has does not change: every other object is the one the default render has.
	for k, u := range def.chart {
		if strings.Contains(k, "executor") {
			continue
		}
		got := off.chart[k]
		if got == nil {
			t.Errorf("%s is missing with executor.enabled=false", k)
			continue
		}
		if k != metricsAuth && !equalObjects(u.Object, got.Object) {
			t.Errorf("%s differs with executor.enabled=false", k)
		}
	}
	// The metrics authentication binding keeps the analyzer and the verifier and loses the executor.
	crb := convert[rbacv1.ClusterRoleBinding](t, off.chart[metricsAuth])
	subjects := make([]string, 0, len(crb.Subjects))
	for _, s := range crb.Subjects {
		subjects = append(subjects, s.Name)
	}
	if want := []string{analyzerName, "shadewright-verifier"}; !slices.Equal(subjects, want) {
		t.Errorf("metrics-auth subjects %v, want %v", subjects, want)
	}
	// The helm test asks the components that exist.
	pod := off.tests[testPodKey]
	if pod == nil {
		t.Fatal("the helm test Pod is not rendered")
	}
	body := allObjects(off)[testPodKey]
	if strings.Contains(body, "EXECUTOR_URL") {
		t.Errorf("the helm test still asks the executor: %s", body)
	}
	if !strings.Contains(body, "ANALYZER_URL") || !strings.Contains(body, "VERIFIER_URL") {
		t.Errorf("the helm test lost the analyzer or the verifier: %s", body)
	}
}

func equalObjects(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// TestExecutorDefaultIsOn: the value is true when unset, and an explicit true renders exactly what the default
// renders (the release before this value rendered the same objects).
func TestExecutorDefaultIsOn(t *testing.T) {
	def := allObjects(defaultRender(t, withVerifier, "metrics.serviceMonitor.enabled=true"))
	on := allObjects(defaultRender(t, withVerifier, "metrics.serviceMonitor.enabled=true", "executor.enabled=true"))
	if len(def) == 0 || len(def) != len(on) {
		t.Fatalf("%d objects by default, %d with executor.enabled=true", len(def), len(on))
	}
	for k, v := range def {
		if on[k] != v {
			t.Errorf("%s differs between the default and executor.enabled=true", k)
		}
	}
	r := defaultRender(t)
	if r.chart[executorDeployment] == nil {
		t.Error("the default render has no executor Deployment")
	}
	// A null value drops the key, and the schema refuses anything but a boolean.
	if out := helmFailsCI(t, "executor.enabled=yes"); !strings.Contains(out, "enabled") {
		t.Errorf("error does not mention enabled:\n%s", out)
	}
}

// helmFailsCI renders the chart as chart-testing installs it (no executor identity) and returns helm's error
// output; it fails the test if the render succeeds.
func helmFailsCI(t *testing.T, set ...string) string {
	t.Helper()
	args := helmArgsWith(t, ciValues(t), append([]string{testCluster}, set...)...)
	cmd := exec.CommandContext(t.Context(), tool(t, "HELM", "helm"), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("helm template succeeded with %v, want an error", set)
	}
	return stderr.String()
}

// TestExecutorOffRefusesExecutorOnlyValues: each value that only configures an executor is refused while the
// executor is off, with a message that names it; the same value is accepted with the executor on.
func TestExecutorOffRefusesExecutorOnlyValues(t *testing.T) {
	cases := map[string]string{
		replicas2 + "=2": replicas2,
		"executor.identity.azure.clientId=" + id0:    "executor.identity.azure.clientId",
		"executor.serviceAccount.annotations.a=b":    "executor.serviceAccount.annotations",
		"executor.goMemLimit=100MiB":                 "executor.goMemLimit",
		hintsKey + "=true":                           hintsKey,
		verifKey + "=true":                           verifKey,
		"executor.verification.nodeGrace=2m":         "executor.verification.nodeGrace",
		"executor.extraArgs={--zap-log-level=debug}": "executor.extraArgs",
		"executor.metricsTLS.secretName=tls":         "executor.metricsTLS.secretName",
	}
	for set, want := range cases {
		t.Run(set, func(t *testing.T) {
			out := helmFailsCI(t, executorOff, set)
			if !strings.Contains(out, want) || !strings.Contains(out, "executor.enabled") {
				t.Errorf("error does not name %q and executor.enabled:\n%s", want, out)
			}
			// With the executor on the same value renders.
			defaultRender(t, set)
		})
	}
	t.Run("several values are all named", func(t *testing.T) {
		out := helmFailsCI(t, executorOff, replicas2+"=2", verifKey+"=true")
		if !strings.Contains(out, replicas2) || !strings.Contains(out, verifKey) {
			t.Errorf("error does not name both values:\n%s", out)
		}
	})
	// The values the chart leaves alone have no effect and are not refused: the published chart pins the
	// executor's digest, and the rest only places or sizes a Pod that is not there.
	t.Run("inert values are accepted", func(t *testing.T) {
		r := defaultRender(t, executorOff, "executor.image.pullPolicy=Always", "executor.resources.limits.cpu=1",
			"executor.podLabels.team=a", "executor.podAnnotations.a=b", "executor.nodeSelector.a=b",
			"executor.priorityClassName=high", "executor.image.repository=mirror.example/executor",
			"executor.image.digest=sha256:"+strings.Repeat("4", 64))
		if r.chart[executorDeployment] != nil {
			t.Error("the executor Deployment is rendered")
		}
	})
}

// TestExecutorOffNeedsNoExecutorImage: a source checkout has no executor digest, and without the executor the
// chart does not ask for one.
func TestExecutorOffNeedsNoExecutorImage(t *testing.T) {
	args := helmArgsWith(t, nil, testCluster, executorOff,
		"analyzer.image.digest=sha256:"+strings.Repeat("2", 64))
	r := renderArgs(t, args)
	if r.chart[analyzerDeployment] == nil || r.chart[executorDeployment] != nil {
		t.Errorf("analyzer %v, executor %v", r.chart[analyzerDeployment] != nil, r.chart[executorDeployment] != nil)
	}
}

// TestServiceMonitorTLSNeedsOnlyTheComponentsThatRun: with the executor off, the ServiceMonitor certificate
// check does not ask for the executor's certificate.
func TestServiceMonitorTLSNeedsOnlyTheComponentsThatRun(t *testing.T) {
	tls := []string{smTLS, "metrics.serviceMonitor.tls.serverName=m.example",
		"metrics.serviceMonitor.tls.ca.secret.name=ca", "analyzer.metricsTLS.secretName=a", executorOff}
	if out := helmFailsCI(t, tls[:4]...); !strings.Contains(out, "executor.metricsTLS.secretName") {
		t.Errorf("with the executor on, the executor's certificate is not asked for:\n%s", out)
	}
	r := defaultRender(t, tls...)
	var monitors []string
	for k := range r.chart {
		if strings.HasPrefix(k, "ServiceMonitor/") {
			monitors = append(monitors, k)
		}
	}
	if want := []string{"ServiceMonitor/" + operatorNS + "/shadewright-analyzer-metrics"}; !slices.Equal(monitors, want) {
		t.Errorf("ServiceMonitors %v, want %v", monitors, want)
	}
}
