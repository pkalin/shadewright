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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// probe is the probe each component container must carry: an HTTP GET on the health port, no initial delay.
func probe(path string, period, timeout, failures int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path, Port: intstr.FromInt32(8081),
		}},
		PeriodSeconds: period, TimeoutSeconds: timeout, FailureThreshold: failures,
	}
}

// The three components report started and ready from /readyz, which passes once their caches have synced; the
// startup probe gives them five minutes, and liveness and readiness start after it with no initial delay
// (#762). The chart and kustomize render the same probes.
func TestComponentProbes(t *testing.T) {
	want := map[string]*corev1.Probe{
		"startup":   probe("/readyz", 2, 2, 150),
		"liveness":  probe("/healthz", 20, 2, 3),
		"readiness": probe("/readyz", 10, 2, 3),
	}
	r := render(t, false)
	for _, comp := range []string{executorComp, analyzerComp, verifierComp} {
		k := "Deployment/" + operatorNS + "/" + release + "-" + comp
		for source, objs := range map[string]map[string]*unstructured.Unstructured{
			"chart": r.chart, "kustomize": r.kustomize,
		} {
			u := objs[k]
			if u == nil {
				t.Fatalf("%s: no %s", source, k)
			}
			d := convert[appsv1.Deployment](t, u)
			if len(d.Spec.Template.Spec.Containers) != 1 {
				t.Fatalf("%s %s: want one container", source, k)
			}
			c := d.Spec.Template.Spec.Containers[0]
			got := map[string]*corev1.Probe{
				"startup": c.StartupProbe, "liveness": c.LivenessProbe, "readiness": c.ReadinessProbe,
			}
			for kind, w := range want {
				g := got[kind]
				if g == nil {
					t.Errorf("%s %s: no %s probe", source, k, kind)
					continue
				}
				if g.InitialDelaySeconds != 0 {
					t.Errorf("%s %s: %s probe has initialDelaySeconds %d", source, k, kind, g.InitialDelaySeconds)
				}
				if asJSON(t, g) != asJSON(t, w) {
					t.Errorf("%s %s: %s probe %s, want %s", source, k, kind, asJSON(t, g), asJSON(t, w))
				}
			}
		}
	}
}
