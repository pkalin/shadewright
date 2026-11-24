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
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/shadewright-labs/shadewright/internal/providers/model/openaicompatible"
)

const (
	selectorApp       = "app"
	selectorNamespace = "namespace"
)

// selectorCorpus builds Service pod selectors around the edges of the analyzer's label rules: key prefixes
// and names, value lengths and characters, value types and the entry count.
func selectorCorpus() []map[string]any {
	r := rand.New(rand.NewPCG(834, 4)) // #nosec G404 -- reproducible corpus
	keys := []string{selectorApp, "a", "A1", "app.kubernetes.io/name", "example.com/x", "Example.com/x", "-app",
		"app-", "_a", "a_b.c-d", "bad key!", "a/b/c", "/app", "app/", "a..b/c", "x y", "ü",
		strings.Repeat("k", 63), strings.Repeat("k", 64),
		strings.Repeat("p", 253) + "/n", strings.Repeat("p", 254) + "/n", "p." + strings.Repeat("q", 63) + "/n"}
	values := []any{"x", "", "x y", "Model-1.v_2", "-x", "x-", ".x", "x.", strings.Repeat("v", 63),
		strings.Repeat("v", 64), "a/b", "ü", 5, true, 1.5}
	out := make([]map[string]any, 0, len(keys)+len(values)+15)
	for _, k := range keys {
		out = append(out, map[string]any{k: "model"})
	}
	for _, v := range values {
		out = append(out, map[string]any{selectorApp: v})
	}
	for _, n := range []int{31, 32, 33} {
		m := map[string]any{}
		for i := range n {
			m[fmt.Sprintf("l%d", i)] = "v"
		}
		out = append(out, m)
	}
	for range 12 {
		m := map[string]any{}
		for range 1 + r.IntN(3) {
			m[keys[r.IntN(len(keys))]] = values[r.IntN(len(values))]
		}
		out = append(out, m)
	}
	return out
}

// The chart renders a Service pod selector exactly when the analyzer accepts it.
func TestCompatibleChartSelectorAgreesWithAnalyzer(t *testing.T) {
	helm := tool(t, "HELM", "helm")
	dir := t.TempDir()
	accepted, refused := 0, 0
	for i, sel := range selectorCorpus() {
		decl := map[string]any{"url": "https://model.demo.svc.cluster.local/v1", "inferenceAPI": "Responses",
			"models": []string{"m"}, "service": map[string]any{selectorNamespace: "demo", "podSelector": sel, "port": 8443}}
		raw, err := json.Marshal([]any{decl})
		if err != nil {
			t.Fatal(err)
		}
		_, analyzerErr := openaicompatible.ParseEndpoints(string(raw))

		values, err := json.Marshal(map[string]any{"analyzer": map[string]any{"analysis": map[string]any{
			"triageModel": "m", "disableFoundry": true,
			"openAICompatible": map[string]any{"endpoints": []any{decl}}}}})
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, fmt.Sprintf("selector-%d.yaml", i))
		if err := os.WriteFile(file, values, 0o600); err != nil {
			t.Fatal(err)
		}
		args := append(helmArgsWith(t, []string{"-f", testValues(t), "-f", file}), "--skip-schema-validation")
		out, chartErr := exec.CommandContext(t.Context(), helm, args...).Output()
		if (chartErr == nil) != (analyzerErr == nil) {
			t.Fatalf("selector %q: chart error %v, analyzer error %v", sel, chartErr, analyzerErr)
		}
		if chartErr != nil {
			refused++
			continue
		}
		accepted++
		r := rendered{chart: map[string]*unstructured.Unstructured{}, tests: map[string]*unstructured.Unstructured{}}
		for _, u := range objects(t, out) {
			r.chart[key(u)] = u
		}
		var flag string
		for _, arg := range container(t, r, analyzerDeployment).Args {
			if v, ok := strings.CutPrefix(arg, "--openai-compatible-endpoints="); ok {
				flag = v
			}
		}
		got, err := openaicompatible.ParseEndpoints(flag)
		if err != nil {
			t.Fatalf("selector %q: the analyzer refuses the rendered flag: %v", sel, err)
		}
		want := map[string]string{}
		for k, v := range sel {
			want[k] = v.(string)
		}
		if len(got) != 1 || got[0].Service == nil || !reflect.DeepEqual(got[0].Service.PodSelector, want) {
			t.Fatalf("selector %q rendered as %+v", sel, got)
		}
	}
	if accepted < 10 || refused < 10 {
		t.Fatalf("corpus is one-sided: %d accepted, %d refused", accepted, refused)
	}
}
