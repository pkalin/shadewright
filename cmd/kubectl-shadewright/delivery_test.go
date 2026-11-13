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
	"encoding/json"
	"maps"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/fluxstatus"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// testWatched is the namespace the analyzer of the healthy install watches.
const testWatched = "demo"

const (
	chkDelivery = "Delivery sources"
	// testHostPath is testRepo as the report prints it.
	testHostPath = "github.com/acme/fleet"
	notAURL      = "(not a URL)"
)

// watchingAnalyzer is the healthy analyzer Deployment, started with --watch-namespaces=namespaces.
func watchingAnalyzer(namespaces string) *appsv1.Deployment {
	dep := healthyDeployment("analyzer")
	dep.Spec.Template.Spec.Containers = []corev1.Container{{Name: "analyzer",
		Args: []string{"--leader-elect", "--" + watchNamespacesFlag + "=" + namespaces}}}
	return dep
}

// workload is a Deployment in ns labelled as applied by the Kustomization ksNamespace/ksName; empty values leave
// the labels out.
func workload(ns, name, ksNamespace, ksName string) *appsv1.Deployment {
	labels := map[string]string{}
	if ksName != "" {
		labels[sourcemap.LabelKustomizationName] = ksName
		labels[sourcemap.LabelKustomizationNamespace] = ksNamespace
	}
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
}

// readySource is a Ready GitRepository name of url and testBranch that fetched testRevision, with extra spec
// fields.
func readySource(name, url string, extra map[string]any) client.Object {
	spec := gitSpec(url, testBranch)
	maps.Copy(spec, extra)
	u := fluxObj(fluxstatus.KindGitRepository, name, spec, "True", "Succeeded", "")
	u.Object[fieldStatus].(map[string]any)["artifact"] = map[string]any{"revision": testRevision}
	return u
}

// readyKustomization is a Ready Kustomization name of the GitRepository source that applied testRevision, with
// extra spec fields.
func readyKustomization(name, source string, extra map[string]any) client.Object {
	spec := ksSpec(source)
	maps.Copy(spec, extra)
	u := fluxObj(fluxstatus.KindKustomization, name, spec, "True", "ReconciliationSucceeded", "")
	u.Object[fieldStatus].(map[string]any)["lastAppliedRevision"] = testRevision
	return u
}

// sourceOf returns the source of the report for the Kustomization flux-system/name.
func sourceOf(t *testing.T, d *Diagnosis, name string) DeliverySource {
	t.Helper()
	if d.Delivery == nil {
		t.Fatal("no delivery report")
	}
	for _, s := range d.Delivery.Sources {
		if s.Kustomization == testFluxNS+"/"+name {
			return s
		}
	}
	t.Fatalf("no source %s in %+v", name, d.Delivery.Sources)
	return DeliverySource{}
}

// TestDoctorDelivery covers the cases of the issue: a plain source, a Kustomization with a transform, a
// HelmRelease-delivered workload, a source the settings do not name and a suspended source, each with its state
// and reason, plus workloads without Flux labels.
func TestDoctorDelivery(t *testing.T) {
	helm := workload(testWatched, "chart", "", "")
	helm.Labels[sourcemap.LabelHelmReleaseName] = "podinfo"
	objs := append(healthyObjects(),
		readyKustomization("patched", testSource, map[string]any{"images": []any{map[string]any{metaName: "x"}}}),
		readySource("other", "https://github.com/acme/other", nil),
		readyKustomization("other-apps", "other", nil),
		readySource("frozen", testRepo, map[string]any{"suspend": true}),
		readyKustomization("frozen-apps", "frozen", nil),
		workload(testWatched, "patched-web", testFluxNS, "patched"),
		workload(testWatched, "other-web", testFluxNS, "other-apps"),
		workload(testWatched, "frozen-web", testFluxNS, "frozen-apps"),
		workload(testWatched, "missing-web", testFluxNS, "missing"),
		workload(testWatched, "bad-labels", "Not_A_Namespace", "x"),
		workload(testWatched, "hand-made", "", ""),
		helm,
		workload("not-watched", "elsewhere", testFluxNS, "elsewhere"),
	)
	d := runChecks(t, objs, nil)
	for name, want := range map[string]struct{ state, reason, settings string }{
		testKs:        {sourceOK, "", settingsNamed},
		"patched":     {sourceRefused, string(sourcemap.ReasonKustomizationTransforms), settingsNamed},
		"other-apps":  {sourceRefused, string(sourcemap.ReasonRepositoryNotConfigured), settingsNotNamed},
		"frozen-apps": {sourceRefused, string(sourcemap.ReasonSourceOptions), settingsNamed},
		"missing":     {sourceRefused, string(sourcemap.ReasonKustomizationNotFound), settingsUnknown},
	} {
		got := sourceOf(t, d, name)
		if got.State != want.state || got.Reason != want.reason || got.Settings != want.settings {
			t.Errorf("%s: state %s reason %s settings %s, want %+v (%+v)", name, got.State, got.Reason,
				got.Settings, want, got)
		}
		if got.State != sourceOK && got.Detail == "" {
			t.Errorf("%s: no detail", name)
		}
	}
	if len(d.Delivery.Sources) != 5 {
		t.Errorf("got %d sources, want 5 (a Kustomization of an unwatched namespace counts): %+v",
			len(d.Delivery.Sources), d.Delivery.Sources)
	}
	plain := sourceOf(t, d, testKs)
	if plain.GitRepository != testFluxNS+"/"+testSource || plain.URL != testHostPath ||
		plain.Branch != testBranch || plain.Repository != "fleet" || plain.Workloads != 1 {
		t.Errorf("plain source: %+v", plain)
	}
	want := NamespaceDelivery{Namespace: testWatched, Workloads: 8, Kustomization: 5, HelmRelease: 1,
		NoFluxLabels: 1, InvalidFluxLabels: 1}
	if len(d.Delivery.Namespaces) != 1 {
		t.Fatalf("namespaces: %+v", d.Delivery.Namespaces)
	}
	if got := d.Delivery.Namespaces[0]; got.Namespace != want.Namespace || got.Workloads != want.Workloads ||
		got.Kustomization != want.Kustomization || got.HelmRelease != want.HelmRelease ||
		got.NoFluxLabels != want.NoFluxLabels || got.InvalidFluxLabels != want.InvalidFluxLabels ||
		len(got.Unchecked) != 0 {
		t.Errorf("namespace counts %+v, want %+v", got, want)
	}
	if c := level(t, d, chkDelivery); c.Level != levelWarn {
		t.Errorf("delivery check %s: %s", c.Level, c.Detail)
	}

	var out bytes.Buffer
	if err := renderDoctorText(&out, d); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"  Namespace demo: 8 workloads: 5 from Kustomizations, 1 from a HelmRelease (HelmManaged), 1 without Flux " +
			"labels (NoFluxLabels), 1 with invalid Flux labels (InvalidFluxLabels)\n",
		"  OK         Kustomization flux-system/apps (1 workloads), GitRepository flux-system/fleet " +
			"github.com/acme/fleet branch main, settings: repository fleet\n",
		"  REFUSED    Kustomization flux-system/other-apps (1 workloads), GitRepository flux-system/other " +
			"github.com/acme/other branch main, settings: not named; RepositoryNotConfigured: ",
		"  REFUSED    Kustomization flux-system/frozen-apps (1 workloads), GitRepository flux-system/frozen " +
			"github.com/acme/fleet, settings: repository fleet; SourceOptions: GitRepository flux-system/frozen " +
			"sets spec.suspend",
		"  REFUSED    Kustomization flux-system/patched (1 workloads), GitRepository flux-system/fleet " +
			"github.com/acme/fleet branch main, settings: repository fleet; KustomizationTransforms: ",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("text lacks %q:\n%s", line, out.String())
		}
	}

	var js bytes.Buffer
	if err := renderJSON(&js, d); err != nil {
		t.Fatal(err)
	}
	var back Diagnosis
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Delivery == nil || len(back.Delivery.Sources) != 5 || back.Delivery.Namespaces[0].HelmRelease != 1 {
		t.Errorf("JSON delivery: %s", js.String())
	}
}

// TestDoctorDeliveryNotReady checks the states of a Kustomization and a GitRepository Flux has not reconciled.
func TestDoctorDeliveryNotReady(t *testing.T) {
	behind := readyKustomization(testKs, testSource, nil).(*unstructured.Unstructured)
	behind.Object[fieldStatus].(map[string]any)["lastAppliedRevision"] = testBranch + "@sha1:" +
		strings.Repeat("1", 40)
	failing := fluxObj(fluxstatus.KindKustomization, testKs, ksSpec(testSource), "False", "BuildFailed", "x")
	srcFailing := fluxObj(fluxstatus.KindGitRepository, testSource, gitSpec(testRepo, testBranch), "False",
		"GitOperationFailed", "x")
	for name, tc := range map[string]struct {
		with   client.Object
		reason sourcemap.Reason
	}{
		"behind":        {behind, sourcemap.ReasonRevisionMismatch},
		"kustomization": {failing, sourcemap.ReasonKustomizationNotReady},
		"gitrepository": {srcFailing, sourcemap.ReasonSourceNotReady},
	} {
		t.Run(name, func(t *testing.T) {
			objs := healthyObjects()
			setGVK(t, doctorScheme(t), objs)
			d := runChecks(t, replace(objs, tc.with), nil)
			got := sourceOf(t, d, testKs)
			if got.State != sourceNotReady || got.Reason != string(tc.reason) {
				t.Errorf("got %+v, want not-ready %s", got, tc.reason)
			}
		})
	}
}

// TestHostPath checks that a source URL prints as host and path only: no credentials, port, query or fragment.
func TestHostPath(t *testing.T) {
	for raw, want := range map[string]string{
		// #nosec G101 -- a made-up credential, to check that it is dropped.
		"https://user:s3cret-token@github.com:443/acme/fleet.git?token=abc#frag": testHostPath + ".git",
		"ssh://git@github.com/acme/fleet":                                        testHostPath,
		testRepo:                                                                 testHostPath,
		"git@github.com:acme/fleet":                                              notAURL,
		"":                                                                       notAURL,
		"https://github.com/acme/\x1b[2Jfleet":                                   notAURL,
	} {
		if got := hostPath(raw); got != want {
			t.Errorf("hostPath(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestDoctorDeliveryURLCredentials checks that the credentials and query of a GitRepository URL reach neither
// output.
func TestDoctorDeliveryURLCredentials(t *testing.T) {
	const secret = "s3cret-token"
	url := "https://user:" + secret + "@github.com/acme/private?access_token=" + secret
	objs := append(healthyObjects(), readySource("private", url, nil), readyKustomization("private-apps", "private", nil),
		workload(testWatched, "private-web", testFluxNS, "private-apps"))
	d := runChecks(t, objs, nil)
	if got := sourceOf(t, d, "private-apps"); got.URL != "github.com/acme/private" {
		t.Errorf("URL %q", got.URL)
	}
	var text, js bytes.Buffer
	if err := renderDoctorText(&text, d); err != nil {
		t.Fatal(err)
	}
	if err := renderJSON(&js, d); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{text.String(), js.String()} {
		if strings.Contains(out, secret) || strings.Contains(out, "access_token") {
			t.Errorf("output holds the credentials:\n%s", out)
		}
	}
}

// metadataListOf matches a metadata list of the given list kind.
func metadataListOf(kind string) func(client.ObjectList) bool {
	return func(l client.ObjectList) bool {
		m, ok := l.(*metav1.PartialObjectMetadataList)
		return ok && m.GetObjectKind().GroupVersionKind().Kind == kind
	}
}

// TestDoctorDeliveryUnchecked checks that a read the caller may not make, or that fails, is reported as not
// checked with its verb and resource, warns, and never fails the command or reports a source as fine.
func TestDoctorDeliveryUnchecked(t *testing.T) {
	t.Run("workloads forbidden", func(t *testing.T) {
		d := runChecks(t, healthyObjects(), forbiddenList(metadataListOf("StatefulSetList"), errForbidden))
		n := d.Delivery.Namespaces[0]
		if len(n.Unchecked) != 1 || n.Unchecked[0] != "list statefulsets.apps in namespace demo: not allowed with "+
			"your permissions" {
			t.Errorf("unchecked %v", n.Unchecked)
		}
		if c := level(t, d, chkDelivery); c.Level != levelWarn || !strings.Contains(c.Detail, "1 reads not made") {
			t.Errorf("delivery check %s: %s", c.Level, c.Detail)
		}
		if d.Failed {
			t.Error("failed")
		}
	})
	t.Run("kustomizations fail", func(t *testing.T) {
		// The Flux check reports the failed read; the delivery report says the source is unknown.
		funcs := forbiddenList(listKind(fluxstatus.KindKustomization+"List"), errForbidden)
		d := runChecks(t, healthyObjects(), funcs)
		got := sourceOf(t, d, testKs)
		if got.State != sourceUnknown || !strings.Contains(got.Detail,
			"list kustomizations.kustomize.toolkit.fluxcd.io: not allowed with your permissions") {
			t.Errorf("got %+v", got)
		}
		if c := level(t, d, chkDelivery); c.Level != levelWarn {
			t.Errorf("delivery check %s", c.Level)
		}
	})
	t.Run("analyzer forbidden", func(t *testing.T) {
		d := runChecks(t, healthyObjects(), forbiddenList(listKind("DeploymentList"), errForbidden))
		if c := level(t, d, chkDelivery); c.Level != levelWarn || !strings.Contains(c.Detail, "unknown") {
			t.Errorf("delivery check %s: %s", c.Level, c.Detail)
		}
		if len(d.Delivery.Unchecked) != 1 {
			t.Errorf("unchecked %v", d.Delivery.Unchecked)
		}
	})
	t.Run("settings not read", func(t *testing.T) {
		funcs := forbiddenGet(func(key client.ObjectKey, _ client.Object) bool {
			return key.Name == remediationv1alpha1.SettingsName
		})
		d := runChecks(t, healthyObjects(), funcs)
		got := sourceOf(t, d, testKs)
		if got.State != sourceUnknown || got.Settings != settingsUnknown {
			t.Errorf("got %+v", got)
		}
	})
}

// TestWatchedNamespaces checks how --watch-namespaces is read from the analyzer's containers.
func TestWatchedNamespaces(t *testing.T) {
	dep := func(args ...string) appsv1.Deployment {
		d := healthyDeployment("analyzer")
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "analyzer", Args: args}}
		return *d
	}
	for name, tc := range map[string]struct {
		deps    []appsv1.Deployment
		want    string
		wantErr bool
	}{
		"equals":     {deps: []appsv1.Deployment{dep("--watch-namespaces=b, a,a")}, want: "a,b"},
		"separate":   {deps: []appsv1.Deployment{dep("-watch-namespaces", "team-a")}, want: "team-a"},
		"last wins":  {deps: []appsv1.Deployment{dep("--watch-namespaces=a", "--watch-namespaces=b")}, want: "b"},
		"absent":     {deps: []appsv1.Deployment{dep("--leader-elect")}, want: ""},
		"empty":      {deps: []appsv1.Deployment{dep("--watch-namespaces=")}, want: ""},
		"union":      {deps: []appsv1.Deployment{dep("--watch-namespaces=a"), dep("--watch-namespaces=c")}, want: "a,c"},
		"invalid":    {deps: []appsv1.Deployment{dep("--watch-namespaces=*")}, wantErr: true},
		"other flag": {deps: []appsv1.Deployment{dep("--watch-namespaces-x=a")}, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := watchedNamespaces(tc.deps)
			if tc.wantErr {
				if err == nil || strings.Contains(err.Error(), "*") {
					t.Errorf("err = %v, want an error without the value", err)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != tc.want {
				t.Errorf("got %v, %v, want %s", got, err, tc.want)
			}
		})
	}
}

// TestDoctorDeliveryNoWatched checks that an analyzer watching no namespace warns and lists nothing.
func TestDoctorDeliveryNoWatched(t *testing.T) {
	objs := healthyObjects()
	setGVK(t, doctorScheme(t), objs)
	d := runChecks(t, replace(objs, healthyDeployment("analyzer")), nil)
	if c := level(t, d, chkDelivery); c.Level != levelWarn || !strings.Contains(c.Detail, "watches no namespace") {
		t.Errorf("delivery check %s: %s", c.Level, c.Detail)
	}
	if len(d.Delivery.Sources) != 0 {
		t.Errorf("sources %+v", d.Delivery.Sources)
	}
}
