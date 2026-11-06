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
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testPluginVersion is the plugin version the CRD version tests compare against.
const testPluginVersion = "v0.2.0"

// testRelease is the release of testPluginVersion, as the chart labels CRDs.
const testRelease = "0.2.0"

func testCRD(name, label string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if label != "" {
		crd.Labels = map[string]string{crdVersionLabel: label}
	}
	return crd
}

func TestCRDVersionWarning(t *testing.T) {
	s := testScheme(t)
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	incidentsCRD, requestsCRD := shadewrightCRDs[0], shadewrightCRDs[1]
	cases := map[string]struct {
		objs   []client.Object
		plugin string
		want   string
	}{
		"same version": {[]client.Object{testCRD(incidentsCRD, testRelease)}, testPluginVersion, ""},
		"other version": {[]client.Object{testCRD(incidentsCRD, "0.1.0"), testCRD(requestsCRD, "0.1.0")}, "v0.2.0",
			"version 0.1.0 and this plugin is v0.2.0"},
		"no label":           {[]client.Object{testCRD(incidentsCRD, "")}, testPluginVersion, ""},
		"no CRD readable":    {nil, testPluginVersion, ""},
		"dev build":          {[]client.Object{testCRD(incidentsCRD, "0.1.0")}, devVersion, ""},
		"build past the tag": {[]client.Object{testCRD(incidentsCRD, testRelease)}, "v0.2.0-3-gabc1234-dirty", ""},
		"dirty build":        {[]client.Object{testCRD(incidentsCRD, testRelease)}, "v0.2.0-dirty", ""},
		"rc past the tag": {[]client.Object{testCRD(incidentsCRD, testRelease)}, "v0.3.0-rc.2-1-gce333cd",
			"version 0.2.0 and this plugin is v0.3.0-rc.2-1-gce333cd"},
		"untagged SHA":     {[]client.Object{testCRD(incidentsCRD, "0.1.0")}, "ce333cd", ""},
		"label is cleaned": {[]client.Object{testCRD(incidentsCRD, "0.1.0\x1b[2J")}, testPluginVersion, "version 0.1.0 and"},
	}
	for name, tc := range cases {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objs...).Build()
		got := crdVersionWarning(context.Background(), c, tc.plugin)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) || strings.Contains(got, "\x1b") {
			t.Errorf("%s: warning = %q, want it to hold %q", name, got, tc.want)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "kubectl-shadewright "+version+"\n" {
		t.Errorf("version printed %q", got)
	}
	if err := run([]string{"version", "extra"}, &out, &out); err == nil {
		t.Error("version with an argument succeeded")
	}
}

func TestReleaseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v0.2.0": testRelease, "0.2.0": testRelease, "v0.2.0-dirty": testRelease, "v0.2.0-3-gabc1234": testRelease,
		"v0.3.0-rc.2-1-gce333cd-dirty": "0.3.0-rc.2", "v0.3.0-rc.2": "0.3.0-rc.2",
		"dev": "", "": "", "ce333cd": "", "ce333cd-dirty": "",
	} {
		if got := releaseVersion(in); got != want {
			t.Errorf("releaseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
