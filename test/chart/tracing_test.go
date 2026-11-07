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
	"strings"
	"testing"
)

const (
	collectorPort     = 4317
	collectorEndpoint = "http://otel-collector.observability.svc:4317"
)

// otelEnv returns the OTEL_* variables of a component's container.
func otelEnv(t *testing.T, r rendered, k string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, e := range container(t, r, k).Env {
		if strings.HasPrefix(e.Name, "OTEL_") {
			env[e.Name] = e.Value
		}
	}
	return env
}

// TestTracingIsOffByDefault: no OTEL_* variable in either Deployment and no collector rule, so the analyzer
// exports nothing.
func TestTracingIsOffByDefault(t *testing.T) {
	r := defaultRender(t)
	for _, k := range []string{analyzerDeployment, executorDeployment} {
		if env := otelEnv(t, r, k); len(env) != 0 {
			t.Errorf("%s has OTEL_* variables by default: %v", k, env)
		}
	}
	for _, rule := range nonDNSRules(t, analyzerEgressKey, r) {
		if len(rule.Ports) == 1 && rule.Ports[0].Port.IntValue() == collectorPort {
			t.Errorf("collector rule rendered by default: %s", asJSON(t, rule))
		}
	}
}

// TestTracingEndpointSetsTheAnalyzersExporter: the endpoint turns on the OTLP exporter of the analyzer only.
func TestTracingEndpointSetsTheAnalyzersExporter(t *testing.T) {
	r := defaultRender(t, "analyzer.tracing.endpoint="+collectorEndpoint)
	env := otelEnv(t, r, analyzerDeployment)
	if env["OTEL_TRACES_EXPORTER"] != "otlp" || env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] != collectorEndpoint {
		t.Errorf("analyzer OTEL_* variables %v, want the otlp exporter and the endpoint", env)
	}
	if env := otelEnv(t, r, executorDeployment); len(env) != 0 {
		t.Errorf("executor got OTEL_* variables: %v", env)
	}
}

// TestTraceCollectorEgressIsForTheAnalyzerOnly: the optional rule reaches only the named collector Pods on one
// port, and never appears in the executor's policy.
func TestTraceCollectorEgressIsForTheAnalyzerOnly(t *testing.T) {
	on := defaultRender(t, "networkPolicy.analyzer.traceCollector.enabled=true",
		"networkPolicy.analyzer.traceCollector.namespace=observability")
	var found []string
	for _, rule := range nonDNSRules(t, analyzerEgressKey, on) {
		if len(rule.Ports) == 1 && rule.Ports[0].Port.IntValue() == collectorPort {
			found = append(found, asJSON(t, rule))
		}
	}
	want := `{"ports":[{"protocol":"TCP","port":4317}],"to":[{"podSelector":{"matchLabels":` +
		`{"app.kubernetes.io/name":"opentelemetry-collector"}},` +
		`"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"observability"}}}]}`
	if len(found) != 1 || found[0] != want {
		t.Errorf("collector rule\n got:  %v\n want: %s", found, want)
	}
	for _, rule := range nonDNSRules(t, executorEgressKey, on) {
		if len(rule.Ports) == 1 && rule.Ports[0].Port.IntValue() == collectorPort {
			t.Errorf("executor got the collector rule: %s", asJSON(t, rule))
		}
	}
}

func TestTracingValuesAreChecked(t *testing.T) {
	out := helmFails(t, "networkPolicy.analyzer.traceCollector.enabled=true")
	if !strings.Contains(out, "traceCollector.namespace") {
		t.Errorf("error does not name the namespace value:\n%s", out)
	}
	for name, set := range map[string]string{
		"port zero":           "networkPolicy.analyzer.traceCollector.port=0",
		"bad namespace":       "networkPolicy.analyzer.traceCollector.namespace=Not_A_Name",
		"unknown key":         "networkPolicy.analyzer.traceCollector.host=x",
		"endpoint no scheme":  "analyzer.tracing.endpoint=otel:4317",
		"endpoint with user":  "analyzer.tracing.endpoint=http://u@otel:4317",
		"endpoint with path":  "analyzer.tracing.endpoint=http://otel:4317/v1/traces",
		"unknown tracing key": "analyzer.tracing.sampler=always_on",
	} {
		t.Run(name, func(t *testing.T) { helmFails(t, set) })
	}
}
