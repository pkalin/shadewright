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

package sourcemap_test

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// A bare CR ends a line for the YAML parsers but not for kustomize's split, which drops the rest of a
// separator line as a comment: the text after it must not reach the parser of the kustomization file
// unchecked (#594).
func TestResolveRefusesTextAfterABareCR(t *testing.T) {
	cases := map[string]string{
		"aliases": "# config\n--- # comment\rmetadata: {annotations: &a {x: y}, labels: *a}\n",
		"an alias bomb": "# config\n--- # comment\rmetadata:\r" +
			strings.ReplaceAll(aliasBomb("  "), "\n", "\r") + "\n",
	}
	for name, head := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.files[ksFile] = head + f.files[ksFile]
			var (
				res sourcemap.Result
				err error
			)
			n := allocated(func() { res, _, err = f.run(t) })
			if err != nil || res.Outcome != sourcemap.NeedsIntervention || res.Reason != sourcemap.ReasonFileNotReadable {
				t.Fatalf("result = %s/%s (%s), error %v; want NeedsIntervention/FileNotReadable",
					res.Outcome, res.Reason, res.Message, err)
			}
			if want := ksFile + " line 2: a line ends in a line break other than LF or CRLF"; res.Message != want {
				t.Fatalf("message %q, want %q", res.Message, want)
			}
			if n >= executorMemory/2 {
				t.Errorf("the mapping allocated %d MiB, want less than half the executor's %d MiB", n>>20, executorMemory>>20)
			}
		})
	}
}

// A YAML directive is refused with its own reason: kustomize decodes the part before the first separator on
// its own, where the directive does not parse, while sigs.k8s.io/yaml reads the whole file (#594).
func TestResolveRefusesYAMLDirectives(t *testing.T) {
	f := newFixture()
	f.files[ksFile] = "%YAML 1.1\n---\n" + f.files[ksFile]
	res, _, err := f.run(t)
	if err != nil || res.Outcome != sourcemap.NeedsIntervention || res.Reason != sourcemap.ReasonFileNotReadable {
		t.Fatalf("result = %s/%s (%s), error %v; want NeedsIntervention/FileNotReadable", res.Outcome, res.Reason, res.Message, err)
	}
	if want := ksFile + " line 1: YAML directives (lines starting with %) are not supported"; res.Message != want {
		t.Fatalf("message %q, want %q", res.Message, want)
	}
}

// The forms of YAML that sigs.k8s.io/yaml accepts and the check lets through still map (#594).
func TestResolveAcceptsKustomizationYAMLForms(t *testing.T) {
	forms := map[string]func(string) string{
		"leading separator":   func(s string) string { return "---\n" + s },
		"commented separator": func(s string) string { return "# config\n--- # config starts here\n" + s },
		"document end":        func(s string) string { return s + "...\n" },
		"BOM":                 func(s string) string { return "\xef\xbb\xbf" + s },
		"CRLF":                func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") },
		"block scalar": func(s string) string {
			return s + "metadata:\n  annotations:\n    example: |\n      --- # text\n      &anchor *alias\n"
		},
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.files[ksFile] = form(f.files[ksFile])
			var baseline map[string]any
			if err := yaml.UnmarshalStrict([]byte(f.files[ksFile]), &baseline); err != nil {
				t.Fatalf("sigs.k8s.io/yaml refuses the fixture: %v", err)
			}
			res, _, err := f.run(t)
			if err != nil || res.Outcome != sourcemap.Resolved {
				t.Fatalf("valid kustomization refused: %s/%s (%s), error %v", res.Outcome, res.Reason, res.Message, err)
			}
		})
	}
}

// The same forms in a manifest that is not the target.
func TestResolveAcceptsManifestYAMLForms(t *testing.T) {
	forms := map[string]func(string) string{
		"leading separator":   func(s string) string { return "---\n" + s },
		"commented separator": func(s string) string { return "--- # config starts here\n" + s },
		"document end":        func(s string) string { return s + "...\n" },
		"BOM":                 func(s string) string { return "\xef\xbb\xbf" + s },
		"CRLF":                func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") },
		"block scalar": func(s string) string {
			return s + "  annotations:\n    example: |\n      --- # text\n      &anchor *alias\n"
		},
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.files[soFile] += form("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: example\n")
			res, _, err := f.run(t)
			if err != nil || res.Outcome != sourcemap.Resolved {
				t.Fatalf("valid manifest refused: %s/%s (%s), error %v", res.Outcome, res.Reason, res.Message, err)
			}
		})
	}
}
