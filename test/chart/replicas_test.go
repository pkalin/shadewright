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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
)

const (
	kindDeployment = "Deployment"
	kindPDB        = "PodDisruptionBudget"
)

const hostnameKey = "kubernetes.io/hostname"

// chartObjects renders only the chart (no kustomize) with the test values plus extra --set arguments and
// returns its Deployments and PodDisruptionBudgets by name.
func chartObjects(t *testing.T, extraSet ...string) (
	map[string]*appsv1.Deployment, map[string]*policyv1.PodDisruptionBudget,
) {
	t.Helper()
	helm := tool(t, "HELM", "helm")
	deps := map[string]*appsv1.Deployment{}
	pdbs := map[string]*policyv1.PodDisruptionBudget{}
	for _, u := range objects(t, run(t, repoRoot(t), helm, helmArgs(t, false, extraSet...)...)) {
		switch u.GetKind() {
		case kindDeployment:
			deps[u.GetName()] = convert[appsv1.Deployment](t, u)
		case kindPDB:
			pdbs[u.GetName()] = convert[policyv1.PodDisruptionBudget](t, u)
		}
	}
	return deps, pdbs
}

// One replica (the default) renders no PodDisruptionBudget and no anti-affinity; more than one renders both,
// for the raised component only (issue 568).
func TestStandbyReplicas(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []string
		want map[string]int32 // component -> replicas
	}{
		{"default", nil, map[string]int32{executorComp: 1, analyzerComp: 1}},
		{"explicit one", []string{"executor.replicas=1", "analyzer.replicas=1"},
			map[string]int32{executorComp: 1, analyzerComp: 1}},
		{"executor standby", []string{"executor.replicas=2"}, map[string]int32{executorComp: 2, analyzerComp: 1}},
		{"analyzer standby", []string{"analyzer.replicas=2"}, map[string]int32{executorComp: 1, analyzerComp: 2}},
		{"both", []string{"executor.replicas=2", "analyzer.replicas=3"}, map[string]int32{executorComp: 2, analyzerComp: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, pdbs := chartObjects(t, tc.set...)
			raised := 0
			for _, comp := range components {
				name := release + "-" + comp
				d := deps[name]
				if d == nil {
					t.Fatalf("no Deployment %s", name)
				}
				n := tc.want[comp]
				if d.Spec.Replicas == nil || *d.Spec.Replicas != n {
					t.Errorf("%s: replicas %v, want %d", comp, d.Spec.Replicas, n)
				}
				aff := d.Spec.Template.Spec.Affinity
				pdb := pdbs[name]
				if n == 1 {
					if pdb != nil {
						t.Errorf("%s: PodDisruptionBudget rendered with one replica", comp)
					}
					if aff != nil {
						t.Errorf("%s: affinity rendered with one replica: %s", comp, asJSON(t, aff))
					}
					continue
				}
				raised++
				if pdb == nil {
					t.Fatalf("%s: no PodDisruptionBudget with %d replicas", comp, n)
				}
				if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.String() != "1" || pdb.Spec.MaxUnavailable != nil {
					t.Errorf("%s: PodDisruptionBudget %s, want minAvailable 1", comp, asJSON(t, pdb.Spec))
				}
				if got, want := asJSON(t, pdb.Spec.Selector), asJSON(t, d.Spec.Selector); got != want {
					t.Errorf("%s: PodDisruptionBudget selector %s, want the Deployment's %s", comp, got, want)
				}
				if aff == nil || aff.PodAntiAffinity == nil {
					t.Fatalf("%s: no pod anti-affinity with %d replicas", comp, n)
				}
				pa := aff.PodAntiAffinity
				req, pref := pa.RequiredDuringSchedulingIgnoredDuringExecution, pa.PreferredDuringSchedulingIgnoredDuringExecution
				if len(req) != 0 || len(pref) != 1 {
					t.Fatalf("%s: anti-affinity must be one preferred term: %s", comp, asJSON(t, pa))
				}
				term := pa.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm
				if term.TopologyKey != hostnameKey {
					t.Errorf("%s: topology key %q, want %s", comp, term.TopologyKey, hostnameKey)
				}
				if got, want := asJSON(t, term.LabelSelector.MatchLabels), asJSON(t, d.Spec.Selector.MatchLabels); got != want {
					t.Errorf("%s: anti-affinity selects %s, want the component's Pods %s", comp, got, want)
				}
			}
			if len(pdbs) != raised {
				t.Errorf("%d PodDisruptionBudgets, want %d", len(pdbs), raised)
			}
		})
	}
}

// The installer's own affinity stays next to the chart's anti-affinity.
func TestStandbyKeepsTheInstallersAffinity(t *testing.T) {
	const na = "executor.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution." +
		"nodeSelectorTerms[0].matchExpressions[0]"
	deps, _ := chartObjects(t, "executor.replicas=2", na+".key=pool", na+".operator=Exists")
	aff := deps[release+"-"+executorComp].Spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.PodAntiAffinity == nil {
		t.Fatalf("want the installer's node affinity and the chart's anti-affinity, got %s", asJSON(t, aff))
	}
}

func TestChartRefusesZeroReplicas(t *testing.T) {
	for _, comp := range components {
		if out := helmFails(t, fmt.Sprintf("%s.replicas=0", comp)); !strings.Contains(out, "minimum") {
			t.Errorf("%s: error does not mention the minimum:\n%s", comp, out)
		}
	}
}
