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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/fluxstatus"
)

// Names of the doctor tests.
const (
	testFluxNS   = "flux-system"
	testSource   = "fleet"
	testKs       = "apps"
	testKeyID    = "https://vault-a.vault.azure.net/keys/github-app"
	testCRDName  = "incidents.remediation.shadewright.io"
	testBranch   = "main"
	fieldStatus  = "status"
	kindSettings = "RemediationSettings"
)

// Check name prefixes and details the tests look for.
const (
	chkSettings  = kindSettings
	chkKill      = "Kill switch"
	chkExecutor  = "Deployment executor"
	chkSource    = "Flux GitRepository"
	chkKs        = "Flux Kustomization"
	chkSources   = "Flux GitRepositories"
	msgForbidden = "not readable with your permissions"
	chkRepos     = "Repositories"
	metaName     = "name"
)

// gitSpec is the spec of a Flux GitRepository of url that tracks branch.
func gitSpec(url, branch string) map[string]any {
	return map[string]any{"url": url, "ref": map[string]any{"branch": branch}}
}

// ksSpec is the spec of a Flux Kustomization that applies the GitRepository source of its own namespace.
func ksSpec(source string) map[string]any {
	return map[string]any{"path": "./apps", "sourceRef": map[string]any{"kind": fluxstatus.KindGitRepository,
		metaName: source}}
}

// healthyCRD is an established CRD that serves v1alpha1, labelled with testRelease.
func healthyCRD(name string) *apiextensionsv1.CustomResourceDefinition {
	crd := testCRD(name, testRelease)
	crd.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true}}
	crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}
	return crd
}

// healthySettings has the kill switch off, a daily limit and one complete repository.
func healthySettings() *remediationv1alpha1.RemediationSettings {
	s := testSettings()
	s.Spec.Paused = new(false)
	s.Spec.Repositories[0].TargetBranch = testBranch
	s.Spec.Repositories[0].GitHub = &remediationv1alpha1.GitHubRepository{AppClientID: "Iv23abc", InstallationID: 42,
		Signer: remediationv1alpha1.Signer{AzureKeyVault: &remediationv1alpha1.AzureKeyVaultKey{KeyID: testKeyID}}}
	return s
}

// healthyDeployment is an available Deployment of comp with one replica.
func healthyDeployment(comp string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shadewright-" + comp, Namespace: testNS, Generation: 2,
			Labels: map[string]string{componentLabel: comp}},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
				Reason: "MinimumReplicasAvailable"}}},
	}
}

// fluxObj is a Flux object of kind with spec, Ready with the given status, reason and message for generation 1.
func fluxObj(kind, name string, spec map[string]any, ready, reason, msg string) *unstructured.Unstructured {
	gv := schema.GroupVersion{Group: fluxstatus.SourceGroup, Version: fluxstatus.SourceVersion}
	if kind == fluxstatus.KindKustomization {
		gv = schema.GroupVersion{Group: fluxstatus.KustomizeGroup, Version: fluxstatus.KustomizeVersion}
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": spec,
		fieldStatus: map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{
			"type": "Ready", fieldStatus: ready, "reason": reason, "message": msg, "observedGeneration": int64(1)}}},
	}}
	u.SetGroupVersionKind(gv.WithKind(kind))
	u.SetNamespace(testFluxNS)
	u.SetName(name)
	u.SetGeneration(1)
	return u
}

func healthySource() *unstructured.Unstructured {
	return fluxObj(fluxstatus.KindGitRepository, testSource, gitSpec(testRepo, testBranch), "True", "Succeeded", "")
}

func healthyKustomization() *unstructured.Unstructured {
	return fluxObj(fluxstatus.KindKustomization, testKs, ksSpec(testSource), "True",
		"ReconciliationSucceeded", "")
}

// healthyObjects is an install on which every check passes. It holds a failing Kustomization of another
// source, which no check may report.
func healthyObjects() []client.Object {
	unrelated := fluxObj(fluxstatus.KindKustomization, "unrelated", ksSpec("other"), "False", "BuildFailed", "")
	objs := make([]client.Object, 0, len(doctorCRDs)+6)
	for _, name := range doctorCRDs {
		objs = append(objs, healthyCRD(name))
	}
	return append(objs, healthySettings(), healthyDeployment("analyzer"), healthyDeployment("executor"),
		healthySource(), healthyKustomization(), unrelated)
}

// replace returns objs with each object of the same type, namespace and name as one of with replaced by it.
func replace(objs []client.Object, with ...client.Object) []client.Object {
	key := func(o client.Object) string {
		kind := ""
		if u, ok := o.(*unstructured.Unstructured); ok {
			kind = u.GetKind()
		}
		return fmt.Sprintf("%T/%s/%s/%s", o, kind, o.GetNamespace(), o.GetName())
	}
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		replaced := false
		for _, w := range with {
			if key(w) == key(o) {
				replaced = true
			}
		}
		if !replaced {
			out = append(out, o)
		}
	}
	return append(out, with...)
}

// without returns objs without the objects for which drop is true.
func without(objs []client.Object, drop func(client.Object) bool) []client.Object {
	var out []client.Object
	for _, o := range objs {
		if !drop(o) {
			out = append(out, o)
		}
	}
	return out
}

func doctorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := testScheme(t)
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// setGVK gives typed objects their kind, so replace can compare them.
func setGVK(t *testing.T, s *runtime.Scheme, objs []client.Object) {
	t.Helper()
	for _, o := range objs {
		if _, ok := o.(*unstructured.Unstructured); ok {
			continue
		}
		gvks, _, err := s.ObjectKinds(o)
		if err != nil {
			t.Fatal(err)
		}
		o.GetObjectKind().SetGroupVersionKind(gvks[0])
	}
}

func runChecks(t *testing.T, objs []client.Object, funcs *interceptor.Funcs) *Diagnosis {
	t.Helper()
	s := doctorScheme(t)
	setGVK(t, s, objs)
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&appsv1.Deployment{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return doctor(context.Background(), b.Build(), testNS, testPluginVersion)
}

// level returns the level of the one check whose name starts with prefix, failing when there is not exactly one.
func level(t *testing.T, d *Diagnosis, prefix string) Check {
	t.Helper()
	var found []Check
	for _, c := range d.Checks {
		if strings.HasPrefix(c.Name, prefix) {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d checks named %q, want 1: %+v", len(found), prefix, d.Checks)
	}
	return found[0]
}

func forbiddenGet(match func(client.ObjectKey, client.Object) bool) *interceptor.Funcs {
	return &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if match(key, obj) {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "x"}, key.Name, nil)
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

func forbiddenList(match func(client.ObjectList) bool, err error) *interceptor.Funcs {
	return &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if match(list) {
			return err
		}
		return c.List(ctx, list, opts...)
	}}
}

func listKind(kind string) func(client.ObjectList) bool {
	return func(l client.ObjectList) bool {
		if u, ok := l.(*unstructured.UnstructuredList); ok {
			return u.GetKind() == kind
		}
		_, isDeployments := l.(*appsv1.DeploymentList)
		return isDeployments && kind == "DeploymentList"
	}
}

var errForbidden = apierrors.NewForbidden(schema.GroupResource{Resource: "x"}, "", nil)

// TestDoctorAllPass runs every check on a healthy install: each passes and the diagnosis has not failed.
func TestDoctorAllPass(t *testing.T) {
	d := runChecks(t, healthyObjects(), nil)
	// 4 CRDs, settings, kill switch, 2 Deployments, 1 repository, 1 GitRepository, 1 Kustomization.
	if len(d.Checks) != 11 {
		t.Fatalf("got %d checks, want 11: %+v", len(d.Checks), d.Checks)
	}
	for _, c := range d.Checks {
		if c.Level != levelPass {
			t.Errorf("%s: %s %s, want pass", c.Name, c.Level, c.Detail)
		}
		if !strings.HasPrefix(c.Link, doctorURL+"#") {
			t.Errorf("%s: link %q", c.Name, c.Link)
		}
	}
	if d.Failed {
		t.Error("Failed is true")
	}
}

// TestDoctorChecks makes each check fail, warn or be forbidden on an otherwise healthy install, and checks that
// only that check changes level.
func TestDoctorChecks(t *testing.T) {
	notServed := healthyCRD(testCRDName)
	notServed.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1beta1", Served: true, Storage: true}, {Name: "v1alpha1", Served: false}}
	notEstablished := healthyCRD(testCRDName)
	notEstablished.Status.Conditions = nil
	otherRelease := healthyCRD(testCRDName)
	otherRelease.Labels[crdVersionLabel] = "0.1.0"

	paused := healthySettings()
	paused.Spec.Paused = new(true)
	noLimit := healthySettings()
	noLimit.Spec.Analysis = nil
	zeroLimit := healthySettings()
	zeroLimit.Spec.Analysis.MaxAnalysesPerDay = ptr.To[int32](0)
	incomplete := healthySettings()
	incomplete.Spec.Repositories[0].GitHub.InstallationID = 0
	incomplete.Spec.Repositories[0].GitHub.Signer.AzureKeyVault = nil
	noBranch := healthySettings()
	noBranch.Spec.Repositories[0].TargetBranch = ""
	noRepos := healthySettings()
	noRepos.Spec.Repositories = nil

	unavailable := healthyDeployment("executor")
	unavailable.Status.AvailableReplicas = 0
	unavailable.Status.Conditions[0].Status = corev1.ConditionFalse
	unavailable.Status.Conditions[0].Reason = "MinimumReplicasUnavailable"
	rollout := healthyDeployment("executor")
	rollout.Status.ObservedGeneration = 1
	scaledDown := healthyDeployment("executor")
	scaledDown.Spec.Replicas = ptr.To[int32](0)

	otherBranch := healthySource()
	otherBranch.Object["spec"] = gitSpec(testRepo, "develop")
	sourceFailing := fluxObj(fluxstatus.KindGitRepository, testSource, gitSpec(testRepo, testBranch), "False",
		"GitOperationFailed", "auth failed")
	sourceReconciling := fluxObj(fluxstatus.KindGitRepository, testSource, gitSpec(testRepo, testBranch), "Unknown",
		"Progressing", "")
	otherURL := healthySource()
	otherURL.Object["spec"] = gitSpec(testRepo+"-other", testBranch)
	ksFailing := fluxObj(fluxstatus.KindKustomization, testKs, ksSpec(testSource), "False", "BuildFailed",
		"kustomize build failed")

	isKind := func(kind string) func(client.Object) bool {
		return func(o client.Object) bool { return o.GetObjectKind().GroupVersionKind().Kind == kind }
	}
	isName := func(name string) func(client.ObjectKey, client.Object) bool {
		return func(k client.ObjectKey, _ client.Object) bool { return k.Name == name }
	}

	for name, tc := range map[string]struct {
		change func([]client.Object) []client.Object
		funcs  *interceptor.Funcs
		check  string
		want   string
		detail string
		// also are other checks whose level the change sets, by name prefix.
		also map[string]string
	}{
		"CRD missing": {change: func(o []client.Object) []client.Object {
			return without(o, func(x client.Object) bool { return x.GetName() == testCRDName })
		}, check: "CRD " + testCRDName, want: levelFail, detail: "does not exist"},
		"CRD not served": {change: func(o []client.Object) []client.Object { return replace(o, notServed) },
			check: "CRD " + testCRDName, want: levelFail, detail: "does not serve v1alpha1"},
		"CRD not established": {change: func(o []client.Object) []client.Object { return replace(o, notEstablished) },
			check: "CRD " + testCRDName, want: levelFail, detail: "not Established"},
		"CRD other release": {change: func(o []client.Object) []client.Object { return replace(o, otherRelease) },
			check: "CRD " + testCRDName, want: levelWarn, detail: "version 0.1.0"},
		"CRD forbidden": {funcs: forbiddenGet(isName(testCRDName)),
			check: "CRD " + testCRDName, want: levelWarn, detail: msgForbidden},

		"settings missing": {change: func(o []client.Object) []client.Object {
			return without(o, isKind(kindSettings))
		}, check: chkSettings, want: levelFail, detail: "does not exist",
			also: map[string]string{chkKill: levelWarn, chkRepos: levelWarn}},
		"settings forbidden": {funcs: forbiddenGet(isName(remediationv1alpha1.SettingsName)),
			check: chkSettings, want: levelWarn, detail: msgForbidden,
			also: map[string]string{chkKill: levelWarn, chkRepos: levelWarn}},
		"settings without a limit": {change: func(o []client.Object) []client.Object { return replace(o, noLimit) },
			check: chkSettings, want: levelFail, detail: "maxAnalysesPerDay is not set"},
		"settings limit 0": {change: func(o []client.Object) []client.Object { return replace(o, zeroLimit) },
			check: chkSettings, want: levelWarn, detail: "analysis is disabled"},

		"kill switch on": {change: func(o []client.Object) []client.Object { return replace(o, paused) },
			check: chkKill, want: levelWarn, detail: "ON"},

		"executor missing": {change: func(o []client.Object) []client.Object {
			return without(o, func(x client.Object) bool { return x.GetName() == "shadewright-executor" })
		}, check: chkExecutor, want: levelFail, detail: "no Deployment labelled control-plane=executor"},
		"executor unavailable": {change: func(o []client.Object) []client.Object { return replace(o, unavailable) },
			check: chkExecutor, want: levelFail, detail: "MinimumReplicasUnavailable"},
		"executor rollout": {change: func(o []client.Object) []client.Object { return replace(o, rollout) },
			check: chkExecutor, want: levelWarn, detail: "rollout"},
		"executor scaled to 0": {change: func(o []client.Object) []client.Object { return replace(o, scaledDown) },
			check: chkExecutor, want: levelWarn, detail: "scaled to 0"},
		"deployments forbidden": {funcs: forbiddenList(listKind("DeploymentList"), errForbidden),
			check: chkExecutor, want: levelWarn, detail: msgForbidden,
			also: map[string]string{"Deployment analyzer": levelWarn}},

		"repository incomplete": {change: func(o []client.Object) []client.Object { return replace(o, incomplete) },
			check: "Repository " + testRepoName, want: levelFail,
			detail: "github.installationID, github.signer.azureKeyVault.keyID"},
		"repository without target branch": {change: func(o []client.Object) []client.Object {
			return replace(o, noBranch)
		}, check: "Repository " + testRepoName, want: levelWarn, detail: "main is used"},
		"no repositories": {change: func(o []client.Object) []client.Object { return replace(o, noRepos) },
			check: chkRepos, want: levelWarn, detail: "observe-only",
			// No repository, so Flux is not checked at all.
			also: map[string]string{}},

		"source tracks another branch": {change: func(o []client.Object) []client.Object {
			return replace(o, otherBranch)
		}, check: chkSource, want: levelFail, detail: `tracks branch "develop"`,
			also: map[string]string{chkKs: levelPass}},
		"source failing": {change: func(o []client.Object) []client.Object { return replace(o, sourceFailing) },
			check: chkSource, want: levelFail, detail: "GitOperationFailed: auth failed",
			also: map[string]string{chkKs: levelPass}},
		"source reconciling": {change: func(o []client.Object) []client.Object {
			return replace(o, sourceReconciling)
		}, check: chkSource, want: levelWarn, detail: "reconciling",
			also: map[string]string{chkKs: levelPass}},
		"no source for the repository": {change: func(o []client.Object) []client.Object { return replace(o, otherURL) },
			check: "Flux for repository", want: levelWarn, detail: "no Flux GitRepository has the URL"},
		"kustomization failing": {change: func(o []client.Object) []client.Object { return replace(o, ksFailing) },
			check: chkKs, want: levelFail, detail: "BuildFailed: kustomize build failed",
			also: map[string]string{chkSource: levelPass}},
		"no kustomization": {change: func(o []client.Object) []client.Object {
			return without(o, isKind(fluxstatus.KindKustomization))
		}, check: chkKs + "s of", want: levelWarn, detail: "no Kustomization applies",
			also: map[string]string{chkSource: levelPass}},
		"sources forbidden": {funcs: forbiddenList(listKind(fluxGitRepositories.Kind), errForbidden),
			check: chkSources, want: levelWarn, detail: msgForbidden},
		"kustomizations forbidden": {funcs: forbiddenList(listKind(fluxKustomizations.Kind), errForbidden),
			check: chkKs + "s", want: levelWarn, detail: msgForbidden,
			also: map[string]string{chkSource: levelPass}},
		"flux not installed": {funcs: forbiddenList(listKind(fluxGitRepositories.Kind),
			&meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: fluxstatus.SourceGroup, Kind: "GitRepository"}}),
			check: chkSources, want: levelWarn, detail: "does not serve Flux"},
		"flux list error": {funcs: forbiddenList(listKind(fluxGitRepositories.Kind), errors.New("connection reset")),
			check: chkSources, want: levelFail, detail: "could not be read: connection reset"},
	} {
		t.Run(name, func(t *testing.T) {
			objs := healthyObjects()
			if tc.change != nil {
				s := doctorScheme(t)
				setGVK(t, s, objs)
				objs = tc.change(objs)
			}
			d := runChecks(t, objs, tc.funcs)
			got := level(t, d, tc.check)
			if got.Level != tc.want || !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("%s: %s %q, want %s containing %q", got.Name, got.Level, got.Detail, tc.want, tc.detail)
			}
			if d.Failed != (tc.want == levelFail) {
				t.Errorf("Failed = %v, want %v", d.Failed, tc.want == levelFail)
			}
			for _, c := range d.Checks {
				want := levelPass
				if strings.HasPrefix(c.Name, tc.check) {
					continue
				}
				for prefix, l := range tc.also {
					if strings.HasPrefix(c.Name, prefix) {
						want = l
					}
				}
				if c.Level != want {
					t.Errorf("%s: %s %q, want %s", c.Name, c.Level, c.Detail, want)
				}
			}
		})
	}
}

// settingsGet returns interceptor functions whose Get of the RemediationSettings fills it with obj, as read
// from an API server whose CRD differs from the plugin's.
func settingsGet(obj map[string]any) *interceptor.Funcs {
	return &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		o client.Object, opts ...client.GetOption) error {
		u, ok := o.(*unstructured.Unstructured)
		if !ok || u.GetKind() != kindSettings {
			return c.Get(ctx, key, o, opts...)
		}
		u.Object = obj
		return nil
	}}
}

// TestDoctorSettingsSpec checks the spec of RemediationSettings as the API server returns it: a spec that does
// not parse fails, and a field the plugin does not know warns.
func TestDoctorSettingsSpec(t *testing.T) {
	spec := func(extra map[string]any) map[string]any {
		s := map[string]any{"analysis": map[string]any{"maxAnalysesPerDay": int64(5)}}
		maps.Copy(s, extra)
		return map[string]any{"apiVersion": remediationv1alpha1.GroupVersion.String(), "kind": kindSettings,
			"metadata": map[string]any{"name": "default"}, "spec": s}
	}
	for name, tc := range map[string]struct {
		obj          map[string]any
		want, detail string
	}{
		"parses":        {obj: spec(nil), want: levelPass, detail: "at most 5 analyses per day"},
		"wrong type":    {obj: spec(map[string]any{"paused": "yes"}), want: levelFail, detail: "does not parse"},
		"unknown field": {obj: spec(map[string]any{"newKnob": true}), want: levelWarn, detail: "does not know"},
	} {
		t.Run(name, func(t *testing.T) {
			d := runChecks(t, healthyObjects(), settingsGet(tc.obj))
			got := level(t, d, chkSettings)
			if got.Level != tc.want || !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("%s %q, want %s containing %q", got.Level, got.Detail, tc.want, tc.detail)
			}
		})
	}
}

// TestDoctorStatusIsShownOnly checks invariant 6: whatever the RemediationSettings status says, no level
// changes; the detection it reports is only shown, cleaned.
func TestDoctorStatusIsShownOnly(t *testing.T) {
	base := runChecks(t, healthyObjects(), nil)
	s := healthySettings()
	s.Status = remediationv1alpha1.RemediationSettingsStatus{
		Conditions: []metav1.Condition{{Type: "DetectionFailed", Status: metav1.ConditionTrue, Reason: "Failed",
			LastTransitionTime: metav1.Now()}},
		Platform: &remediationv1alpha1.PlatformStatus{Detected: "aks", Active: "aks"},
		Integrations: []remediationv1alpha1.IntegrationStatus{
			{Name: "keda\x1b]0;x\x07", Detected: true, Version: "keda.sh/v1alpha1", Active: true},
			{Name: "kyverno", Detected: false, Active: false}},
	}
	objs := healthyObjects()
	setGVK(t, doctorScheme(t), objs)
	d := runChecks(t, replace(objs, s), nil)
	for i, c := range d.Checks {
		if c.Level != base.Checks[i].Level || c.Detail != base.Checks[i].Detail {
			t.Errorf("%s: %s %q, without status %s %q", c.Name, c.Level, c.Detail, base.Checks[i].Level,
				base.Checks[i].Detail)
		}
	}
	got := level(t, d, chkSettings).Reported
	want := "platform aks detected, aks active; integration keda active; integration kyverno enabled, not detected"
	if got != want {
		t.Errorf("Reported = %q, want %q", got, want)
	}
}

// TestDoctorText checks the text output: a line per check, the docs link under a warn or fail only, the
// reported detection, and no escape sequence from the cluster.
func TestDoctorText(t *testing.T) {
	failing := fluxObj(fluxstatus.KindKustomization, testKs, ksSpec(testSource), "False", "BuildFailed",
		"bad\x1b[2Joutput")
	paused := healthySettings()
	paused.Spec.Paused = new(true)
	paused.Status.Platform = &remediationv1alpha1.PlatformStatus{Active: "generic"}
	objs := healthyObjects()
	setGVK(t, doctorScheme(t), objs)
	d := runChecks(t, replace(objs, failing, paused), nil)
	var out bytes.Buffer
	if err := renderDoctorText(&out, d); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"Namespace " + testNS + "\n",
		"PASS  CRD incidents.remediation.shadewright.io: established, serves v1alpha1\n",
		"WARN  Kill switch: ON: writes are paused (spec.paused)\n      See " + doctorURL + anchorKillSwitch + "\n",
		"      Reported by the analyzer (shown only): platform none detected, generic active\n",
		"FAIL  Flux Kustomization flux-system/apps: failing, Ready BuildFailed: badoutput\n      See " + doctorURL +
			anchorFlux + "\n",
		"9 passed, 1 warnings, 1 failed\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Error("text holds an escape character")
	}
	if strings.Contains(text, "See "+doctorURL+anchorCRDs) {
		t.Error("a passing check prints its link")
	}
}

// TestRunDoctorExit checks the exit rule: an error only when a check fails, after the output; warns alone do
// not fail the command. The JSON output carries every check.
func TestRunDoctorExit(t *testing.T) {
	paused := healthySettings()
	paused.Spec.Paused = new(true)
	objs := healthyObjects()
	s := doctorScheme(t)
	setGVK(t, s, objs)
	withWarn := replace(objs, paused)
	withFail := without(objs, func(o client.Object) bool { return o.GetName() == "shadewright-analyzer" })
	for name, tc := range map[string]struct {
		objs    []client.Object
		wantErr bool
	}{
		"pass": {objs: objs}, "warn": {objs: withWarn}, "fail": {objs: withFail, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objs...).Build()
			var out bytes.Buffer
			err := runDoctor(context.Background(), c, testNS, outputJSON, &out)
			if tc.wantErr != errors.Is(err, errChecksFailed) || (!tc.wantErr && err != nil) {
				t.Fatalf("err = %v, want failure %v", err, tc.wantErr)
			}
			var d Diagnosis
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			if d.Failed != tc.wantErr || len(d.Checks) == 0 {
				t.Errorf("JSON failed=%v with %d checks", d.Failed, len(d.Checks))
			}
		})
	}
}

// TestParseDoctor checks that doctor takes the flags of status without -A, and no argument, and that its help
// states the exit rule.
func TestParseDoctor(t *testing.T) {
	f, err := parseDoctor([]string{"-n", testNS, "-o", "json"})
	if err != nil || f.namespace != testNS || f.output != outputJSON {
		t.Fatalf("got %+v, %v", f, err)
	}
	for _, args := range [][]string{{"-A"}, {"unexpected-arg"}, {"-o", "xml"}} {
		if _, err := parseDoctor(args); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	if _, err := parseDoctor([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
	var out bytes.Buffer
	if err := run([]string{"doctor", "--help"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "doctor also exits 1 when any check fails, never for a warn alone") {
		t.Errorf("help does not state the exit rule:\n%s", out.String())
	}
}

// TestDoctorAnchors keeps the anchors of the checks equal to "## " sections of the how-to page.
func TestDoctorAnchors(t *testing.T) {
	page, err := os.ReadFile("../../docs/how-to/check-install.md")
	if err != nil {
		t.Fatal(err)
	}
	var sections []string
	for line := range strings.SplitSeq(string(page), "\n") {
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			sections = append(sections, slug(heading))
		}
	}
	for _, a := range []string{anchorCRDs, anchorSettings, anchorKillSwitch, anchorDeployments, anchorRepositories,
		anchorFlux} {
		if !slices.Contains(sections, strings.TrimPrefix(a, "#")) {
			t.Errorf("anchor %s is no section of the page (sections: %v)", a, sections)
		}
	}
}
