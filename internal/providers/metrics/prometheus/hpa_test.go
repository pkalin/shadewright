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

package prometheus

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// hpaTemplates are the templates that read one HorizontalPodAutoscaler's metrics.
var hpaTemplates = []string{tHPACurrent, tHPATarget}

// subdomainByConstruction is the DNS-1123 subdomain grammar written out here, independent of the validation
// package the adapter uses: dot-separated labels of lower case letters, digits and inner hyphens, at most 253
// characters in all.
var subdomainByConstruction = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func isSubdomainByConstruction(s string) bool {
	return len(s) <= 253 && subdomainByConstruction.MatchString(s)
}

// hpaMatcher finds the horizontalpodautoscaler matcher of a query and its quoted value.
var hpaMatcher = regexp.MustCompile(`horizontalpodautoscaler(=|!=|=~|!~)("(?:[^"\\]|\\.)*")`)

// A generated corpus of HPA names, valid and hostile, with the answer known by construction: a name the
// subdomain grammar accepts reaches the query as one exact matcher whose value unquotes to the name itself, and
// any other name is refused before a request, with an error that does not repeat it.
func TestHPANameCorpus(t *testing.T) {
	// Each piece is valid inside a subdomain or breaks it in a known way: quotes, braces, PromQL operators,
	// regexp metacharacters, control and non-ASCII characters, upper case.
	pieces := []string{"a", "z", "0", "9", "-", ".", "keda-hpa-", "worker", `"`, `\`, "{", "}", "(", ")", ",", "=",
		"~", "!", "|", "*", "+", "[", "]", " ", "\n", "\r", "\t", "\x00", "#", "/", "A", "é", "е", "_", ":",
		`"} or vector(1) #`, `",namespace=~".+`}
	// A fixed seed: the corpus is the same on every run, so a failure reproduces.
	rng := rand.New(rand.NewPCG(480, 1)) // #nosec G404 -- a reproducible test corpus, not a secret
	names := make([]string, 0, 4100)
	for range 2000 {
		var b strings.Builder
		for range 1 + rng.IntN(12) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		names = append(names, b.String())
	}
	// Names from the subdomain alphabet only, most of them valid, some not (a leading or trailing hyphen or dot,
	// two dots).
	validPieces := []string{"a", "z", "0", "9", "-", ".", "keda-hpa-", "worker"}
	for range 2000 {
		var b strings.Builder
		for range 1 + rng.IntN(8) {
			b.WriteString(validPieces[rng.IntN(len(validPieces))])
		}
		names = append(names, b.String())
	}
	// Lengths around the bound, valid and not.
	label := strings.Repeat("a", 63)
	long := strings.Join([]string{label, label, label, strings.Repeat("b", 61)}, ".") // 253
	names = append(names, long, long+"c", strings.Repeat("a", 64), strings.Repeat("a", 254),
		strings.Repeat(`"}`, 200), strings.Repeat("a\n", 200))

	var query string
	s, _, n := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		emptyVector(w, r)
	})
	// Every refusal gives the same text, so none can repeat the name it refused.
	_, refusal := s.Query(testCtx(t), func() providers.MetricsQuery {
		q := validQuery(t, hpaTemplates[0])
		q.Params[ParamHPA] = "Refused"
		return q
	}())
	if !errors.Is(refusal, ErrInvalidQuery) {
		t.Fatalf("refusal = %v", refusal)
	}
	valid, hostile := 0, 0
	for _, name := range names {
		for _, tmpl := range hpaTemplates {
			q := validQuery(t, tmpl)
			q.Params[ParamHPA] = name
			before := n.Load()
			query = ""
			_, err := s.Query(testCtx(t), q)
			if !isSubdomainByConstruction(name) {
				hostile++
				if !errors.Is(err, ErrInvalidQuery) || err.Error() != refusal.Error() {
					t.Fatalf("%s %q: error = %v, want %v", tmpl, name, err, refusal)
				}
				if n.Load() != before {
					t.Fatalf("%s %q: a request reached the server", tmpl, name)
				}
				continue
			}
			valid++
			if err != nil {
				t.Fatalf("%s %q: error = %v", tmpl, name, err)
			}
			m := hpaMatcher.FindAllStringSubmatch(query, -1)
			if len(m) != 1 || m[0][1] != "=" {
				t.Fatalf("%s %q: want one exact matcher in %s", tmpl, name, query)
			}
			if got, err := strconv.Unquote(m[0][2]); err != nil || got != name {
				t.Fatalf("%s %q: matcher value %s", tmpl, name, m[0][2])
			}
		}
	}
	// The corpus must exercise both sides, or it proves nothing.
	if valid < 100 || hostile < 1000 {
		t.Fatalf("corpus too one-sided: %d valid, %d hostile", valid, hostile)
	}
}

// An HPA template selects the series of exactly the named HPA in the query's namespace: not an HPA whose name
// extends it, not one of the same name in another namespace, and a duplicate scrape counts once.
func TestHPATemplatesSelectOnlyTheNamedHPA(t *testing.T) {
	series := func(metric, ns, hpa, name, typ, job string, v float64) storedSeries {
		return storedSeries{labels: map[string]string{"__name__": metric, "namespace": ns,
			"horizontalpodautoscaler": hpa, "metric_name": name, "metric_target_type": typ, "job": job,
			"instance": job + ":8080"}, value: v}
	}
	const current, target = "kube_horizontalpodautoscaler_status_target_metric",
		"kube_horizontalpodautoscaler_spec_target_metric"
	stored := make([]storedSeries, 0, 11)
	for _, m := range []string{current, target} {
		v := map[string]float64{current: 6.5, target: 5}[m]
		stored = append(stored,
			series(m, testNS, testHPA, "s0-prometheus", "average", "ksm", v),
			series(m, testNS, testHPA, "s0-prometheus", "average", "ksm-2", v), // a second scrape
			series(m, testNS, testHPA+"-2", "s0-prometheus", "average", "ksm", 99),
			series(m, testNS, "keda-hpa", "s0-prometheus", "average", "ksm", 98),
			series(m, "other", testHPA, "s0-prometheus", "average", "ksm", 97),
		)
	}
	stored = append(stored, series(current, testNS, testHPA, "cpu", "utilization", "ksm", 80))
	s, _, _ := testSource(t, evalFake(t, stored))
	for tmpl, want := range map[string]map[string]float64{
		tHPACurrent: {"s0-prometheus/average": 6.5, "cpu/utilization": 80},
		tHPATarget:  {"s0-prometheus/average": 5},
	} {
		res, err := s.Query(testCtx(t), validQuery(t, tmpl))
		if err != nil {
			t.Fatalf("%s: %v", tmpl, err)
		}
		got := map[string]float64{}
		for _, se := range res.Series {
			if len(se.Labels) != 3 || se.Labels["horizontalpodautoscaler"] != testHPA {
				t.Errorf("%s: labels %v", tmpl, se.Labels)
			}
			got[se.Labels["metric_name"]+"/"+se.Labels["metric_target_type"]] = se.Samples[0].Value
		}
		if len(got) != len(want) || len(res.Series) != len(want) {
			t.Errorf("%s: series %v, want %v", tmpl, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %v, want %v", tmpl, k, got[k], v)
			}
		}
	}
}

// An HPA template takes no pods, and refuses them rather than ignore them.
func TestHPATemplatesRefusePods(t *testing.T) {
	s, _, n := testSource(t, emptyVector)
	for _, tmpl := range hpaTemplates {
		q := validQuery(t, tmpl)
		q.Pods = []string{testStatefulPod}
		_, err := s.Query(testCtx(t), q)
		refusedBeforeRequest(t, tmpl+" with pods", err)
		if info := registry[tmpl].info; info.Pods || len(info.Params) != 2 || info.Params[1] != ParamHPA {
			t.Errorf("%s: info %+v", tmpl, info)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d requests reached the server", n.Load())
	}
}
