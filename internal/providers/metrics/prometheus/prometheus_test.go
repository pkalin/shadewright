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
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

var now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// The namespace and workload of the valid queries.
const (
	testNS       = "demo"
	testWorkload = "web"
)

// testSource serves h and returns a Source pointed at it, with the clock fixed at now, and a counter of the
// requests that reached the server.
func testSource(t *testing.T, h http.HandlerFunc) (*Source, *httptest.Server, *atomic.Int32) {
	t.Helper()
	return testSourceIn(t, []string{testNS}, h)
}

// testSourceIn is testSource with the given allowed namespaces.
func testSourceIn(t *testing.T, namespaces []string, h http.HandlerFunc) (*Source, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	s, err := New(Config{URL: srv.URL + "/prom", Namespaces: namespaces})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	return s, srv, &n
}

// testStatefulPod is a pod of a StatefulSet "web".
const testStatefulPod = "web-0"

// testPods are the target's pods of the valid queries: one of a Deployment and one of a StatefulSet.
var testPods = []string{"web-7d9c8b6f4-x2x9q", testStatefulPod}

func params(ns, w string) map[string]string {
	return map[string]string{ParamNamespace: ns, ParamWorkload: w}
}

// isPodTemplate reports whether the named template selects pods.
func isPodTemplate(t *testing.T, name string) bool {
	t.Helper()
	tmpl, ok := registry[name]
	if !ok {
		t.Fatalf("no template %s", name)
	}
	return tmpl.info.Pods
}

// validQuery is a valid instant query of the template in testNS: with testPods for a pod template, with
// testWorkload for the others.
func validQuery(t *testing.T, template string) providers.MetricsQuery {
	t.Helper()
	q := providers.MetricsQuery{Template: template, Params: params(testNS, testWorkload), End: now}
	if isPodTemplate(t, template) {
		q.Params = map[string]string{ParamNamespace: testNS}
		q.Pods = slices.Clone(testPods)
	}
	return q
}

// instant is a valid instant query of a pod template.
func instant(template string) providers.MetricsQuery {
	return providers.MetricsQuery{
		Template: template, Params: map[string]string{ParamNamespace: testNS}, Pods: slices.Clone(testPods), End: now,
	}
}

// maxRangePoints is the points per series of a range query over MaxRange at MinStep.
const maxRangePoints = int(MaxRange/MinStep) + 1

// rangeQuery asks for the given number of points per series, ending now.
func rangeQuery(points int) providers.MetricsQuery {
	return providers.MetricsQuery{
		Template: "container_restarts_total", Params: map[string]string{ParamNamespace: testNS},
		Pods: slices.Clone(testPods), Start: now.Add(-time.Duration(points-1) * MinStep), End: now, Step: MinStep,
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

const nowSecs = "1793793600"

func emptyVector(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
}

// testCtx bounds a test that a missing guard would make wait.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestTemplatesRender(t *testing.T) {
	// Sorted: the same set always gives the same text.
	const pods = `namespace="demo",pod=~"^(web-0|web-7d9c8b6f4-x2x9q)$"`
	const containers = pods + `,container!="",container!="POD"`
	want := map[string]string{
		"container_memory_working_set_bytes": `sum by (pod, container) (container_memory_working_set_bytes{` +
			containers + `})`,
		"container_memory_limit_bytes": `max by (pod, container) (kube_pod_container_resource_limits{` + pods +
			`,resource="memory"})`,
		"container_cpu_usage_cores": `sum by (pod, container) (rate(container_cpu_usage_seconds_total{` +
			containers + `}[5m]))`,
		"container_cpu_throttled_ratio": `sum by (pod, container) (rate(container_cpu_cfs_throttled_periods_total{` +
			containers + `}[5m])) / sum by (pod, container) (rate(container_cpu_cfs_periods_total{` + containers +
			`}[5m]))`,
		"container_restarts_total": `max by (pod, container) (kube_pod_container_status_restarts_total{` +
			pods + `})`,
		"container_last_terminated_oomkilled": `max by (pod, container) ` +
			`(kube_pod_container_status_last_terminated_reason{` + pods + `,reason="OOMKilled"})`,
		"container_waiting_reason": `max by (pod, container, reason) (kube_pod_container_status_waiting_reason{` +
			pods + `} == 1)`,
		"deployment_replicas_unavailable": `max by (deployment) (kube_deployment_status_replicas_unavailable{` +
			`namespace="demo",deployment="web"})`,
	}
	names := make([]string, 0, len(want))
	for _, info := range Templates() {
		names = append(names, info.Name)
	}
	if got := slices.Sorted(maps.Keys(want)); !slices.Equal(names, got) {
		t.Fatalf("templates = %v, want a test for each: %v", names, got)
	}
	for name, query := range want {
		t.Run(name, func(t *testing.T) {
			var got *http.Request
			s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
				got = r
				emptyVector(w, r)
			})
			// No shorter deadline than Config.Timeout: the server answers at once.
			if _, err := s.Query(t.Context(), validQuery(t, name)); err != nil {
				t.Fatal(err)
			}
			if got.Method != http.MethodGet || got.URL.Path != "/prom/api/v1/query" {
				t.Errorf("request = %s %s", got.Method, got.URL.Path)
			}
			q := got.URL.Query()
			if q.Get("query") != query {
				t.Errorf("query =\n%s\nwant\n%s", q.Get("query"), query)
			}
			if q.Get("time") != nowSecs+".000" || len(q) != 3 {
				t.Errorf("parameters = %v", q)
			}
			// A tenth below the 30 seconds left, in whole milliseconds.
			if ms := serverTimeoutMs(t, q.Get("timeout")); ms > 27000 || ms < 26000 {
				t.Errorf("timeout = %s, want just below 27000ms", q.Get("timeout"))
			}
		})
	}
}

// serverTimeoutMs parses the timeout parameter of a request, in milliseconds.
func serverTimeoutMs(t *testing.T, v string) int64 {
	t.Helper()
	ms, err := strconv.ParseInt(strings.TrimSuffix(v, "ms"), 10, 64)
	if err != nil || !strings.HasSuffix(v, "ms") {
		t.Fatalf("timeout = %q, want milliseconds", v)
	}
	return ms
}

// The server's evaluation timeout follows the sooner deadline, the caller's included, and stays below it, so
// that the server gives up first.
func TestServerTimeoutIsBelowTheDeadline(t *testing.T) {
	var timeout string
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		timeout = r.URL.Query().Get("timeout")
		emptyVector(w, r)
	})
	ctx, cancel := context.WithTimeout(testCtx(t), 2*time.Second)
	defer cancel()
	if _, err := s.Query(ctx, instant("container_restarts_total")); err != nil {
		t.Fatal(err)
	}
	if ms := serverTimeoutMs(t, timeout); ms > 1800 || ms < 1000 {
		t.Errorf("timeout = %s, want just below 1800ms", timeout)
	}
}

func TestRangeQueryParameters(t *testing.T) {
	var got *http.Request
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		writeJSON(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	})
	q := rangeQuery(61) // one hour
	if _, err := s.Query(testCtx(t), q); err != nil {
		t.Fatal(err)
	}
	v := got.URL.Query()
	if got.URL.Path != "/prom/api/v1/query_range" || v.Get("start") != "1793790000.000" ||
		v.Get("end") != nowSecs+".000" || v.Get("step") != "60000ms" || v.Has("time") {
		t.Errorf("request = %s %v", got.URL.Path, v)
	}
}

func TestPodNamesAreEscapedInRegexp(t *testing.T) {
	var query string
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		emptyVector(w, r)
	})
	q := instant("container_restarts_total")
	q.Pods = []string{"web.v2-0", "web.v2-1", "web.v2-0"}
	if _, err := s.Query(testCtx(t), q); err != nil {
		t.Fatal(err)
	}
	if want := `pod=~"^(web\\.v2-0|web\\.v2-1)$"`; !strings.Contains(query, want) {
		t.Errorf("query = %s, want %s in it", query, want)
	}
}

// refusedBeforeRequest checks that err is ErrInvalidQuery and repeats none of the values.
func refusedBeforeRequest(t *testing.T, what string, err error, values ...string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("%s: error = %v, want ErrInvalidQuery", what, err)
	}
	for _, v := range values {
		if v != "" && strings.Contains(err.Error(), v) {
			t.Errorf("%s: error repeats the value %q: %v", what, v, err)
		}
	}
}

func TestParametersAreValidated(t *testing.T) {
	// Values of the workload parameter, refused by the templates that take it.
	workloads := map[string]string{
		"quote and PromQL":       `web"} or vector(1) #`,
		"closing brace":          "web}",
		"regexp wildcard":        "web.*",
		"regexp alternation":     "web|db",
		"regexp class":           "[a-z]+",
		"backslash":              `web\`,
		"newline":                "web\nx",
		"upper case":             "Web",
		"unicode":                "wеb", // Cyrillic e
		"empty workload":         "",
		"too long workload":      strings.Repeat("a", 254),
		"double dot in workload": "web..v2",
	}
	// Values of the namespace parameter, refused by every template.
	namespaces := map[string]string{
		"dot in namespace":   "demo.x",
		"quote in namespace": `demo",namespace=~".+`,
		"empty namespace":    "",
		"too long namespace": strings.Repeat("a", 64),
	}
	// Changes to a valid query; each returns false when it does not apply to the template.
	shapes := map[string]func(q *providers.MetricsQuery, pods bool) bool{
		"missing namespace": func(q *providers.MetricsQuery, _ bool) bool { delete(q.Params, ParamNamespace); return true },
		"extra parameter":   func(q *providers.MetricsQuery, _ bool) bool { q.Params["query"] = "up"; return true },
		"no parameters":     func(q *providers.MetricsQuery, _ bool) bool { q.Params = nil; return true },
		"missing workload": func(q *providers.MetricsQuery, pods bool) bool {
			delete(q.Params, ParamWorkload)
			return !pods
		},
		// The pod templates select pods by the given names only, never by a workload name.
		"workload for a pod template": func(q *providers.MetricsQuery, pods bool) bool {
			q.Params[ParamWorkload] = testWorkload
			return pods
		},
	}
	s, _, n := testSource(t, emptyVector)
	for _, info := range Templates() {
		for name, w := range workloads {
			if !slices.Contains(info.Params, ParamWorkload) {
				continue
			}
			q := validQuery(t, info.Name)
			q.Params[ParamWorkload] = w
			_, err := s.Query(testCtx(t), q)
			refusedBeforeRequest(t, info.Name+", "+name, err, w)
		}
		for name, ns := range namespaces {
			q := validQuery(t, info.Name)
			q.Params[ParamNamespace] = ns
			_, err := s.Query(testCtx(t), q)
			refusedBeforeRequest(t, info.Name+", "+name, err, ns)
		}
		for name, change := range shapes {
			q := validQuery(t, info.Name)
			if !change(&q, info.Pods) {
				continue
			}
			_, err := s.Query(testCtx(t), q)
			refusedBeforeRequest(t, info.Name+", "+name, err)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d requests reached the server", n.Load())
	}
}

// The pod set comes from the caller, which resolves it from the target's owner references; the adapter
// bounds and validates it before any request, and never repeats a name.
func TestPodSetIsValidated(t *testing.T) {
	oversized := make([]string, MaxPods+1)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("web-%d", i)
	}
	cases := map[string][]string{
		"nil":                nil,
		"empty set":          {},
		"more than MaxPods":  oversized,
		"empty name":         {testStatefulPod, ""},
		"quote and PromQL":   {`web-0"} or vector(1) #`},
		"regexp wildcard":    {"web-.*"},
		"regexp alternation": {"web-0|db-0"},
		"regexp group":       {"web-(0)"},
		"backslash":          {`web-0\`},
		"newline":            {"web-0\nx"},
		"upper case":         {"Web-0"},
		"unicode":            {"wеb-0"}, // Cyrillic e
		"double dot":         {"web..0"},
		"longer than a pod":  {strings.Repeat("a", MaxPodName+1)},
		"subdomain length":   {strings.Repeat("a", 253)},
	}
	s, _, n := testSource(t, emptyVector)
	for _, info := range Templates() {
		if !info.Pods {
			continue
		}
		for name, pods := range cases {
			q := instant(info.Name)
			q.Pods = pods
			_, err := s.Query(testCtx(t), q)
			refusedBeforeRequest(t, info.Name+", "+name, err, pods...)
		}
	}
	// A template that does not select pods refuses them rather than ignore them.
	q := validQuery(t, "deployment_replicas_unavailable")
	q.Pods = []string{testStatefulPod}
	_, err := s.Query(testCtx(t), q)
	refusedBeforeRequest(t, "pods for the Deployment template", err)
	if n.Load() != 0 {
		t.Fatalf("%d requests reached the server", n.Load())
	}

	// The bounds themselves are accepted, duplicates included.
	full := make([]string, MaxPods)
	for i := range full {
		full[i] = fmt.Sprintf("%s-%d", strings.Repeat("a", MaxPodName-3), i%100)
	}
	for name, pods := range map[string][]string{
		"MaxPods names":   full,
		"MaxPodName long": {strings.Repeat("a", MaxPodName)},
		"duplicates":      {testStatefulPod, testStatefulPod},
		"dots":            {"web.v2-1"},
	} {
		q := instant("container_restarts_total")
		q.Pods = pods
		if _, err := s.Query(testCtx(t), q); err != nil {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

// fakePrometheus answers an instant query of container_restarts_total like Prometheus would: with a series
// for each pod it knows whose name the pod matcher of the query matches, anchored at both ends. So the test
// checks the query text against real names, not against its expected shape.
func fakePrometheus(t *testing.T, known []string) http.HandlerFunc {
	matcher := regexp.MustCompile(`pod=~("(?:[^"\\]|\\.)*")`)
	return func(w http.ResponseWriter, r *http.Request) {
		m := matcher.FindStringSubmatch(r.URL.Query().Get("query"))
		if m == nil {
			t.Errorf("query has no pod matcher: %s", r.URL.Query().Get("query"))
			emptyVector(w, r)
			return
		}
		re, err := strconv.Unquote(m[1])
		if err != nil {
			t.Errorf("pod matcher is not a quoted string: %s", m[1])
		}
		anchored := regexp.MustCompile("^(?:" + re + ")$")
		var result []string
		for _, pod := range known {
			if anchored.MatchString(pod) {
				result = append(result, fmt.Sprintf(`{"metric":{"pod":%q,"container":"app"},"value":[%s,"1"]}`,
					pod, nowSecs))
			}
		}
		writeJSON(w, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Join(result, ",")+`]}}`)
	}
}

// Only the target's own pods supply its evidence: not the pods of a workload whose name extends the target's
// (the StatefulSet "web-v2" next to the Deployment "web"), nor a standalone pod named like one of its own.
func TestOnlyTheTargetPodsAreSelected(t *testing.T) {
	const (
		deploymentPod = "web-7d9c8b6f4-x2x9q" // of the Deployment "web", the target
		oldRSPod      = "web-6c5b4a392-p9q8r" // of its previous ReplicaSet, during a rollout
		statefulPod   = "web-v2-0"            // of the StatefulSet "web-v2"
		otherRSPod    = "web-v2-5f6d7c8b9-k8m2p"
		lookalike     = "web-x7k2p" // standalone, named like a DaemonSet pod of "web"
	)
	cases := map[string]struct {
		target, want []string
	}{
		"Deployment next to a StatefulSet that extends its name": {
			target: []string{deploymentPod}, want: []string{deploymentPod},
		},
		"StatefulSet whose name extends another workload's": {
			target: []string{statefulPod}, want: []string{statefulPod},
		},
		"rolling Deployment with pods of two ReplicaSets": {
			target: []string{oldRSPod, deploymentPod}, want: []string{deploymentPod, oldRSPod},
		},
		"pod of the target that is gone": {
			target: []string{"web-7d9c8b6f4-gone1"}, want: nil,
		},
	}
	known := []string{deploymentPod, oldRSPod, statefulPod, otherRSPod, lookalike}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _ := testSource(t, fakePrometheus(t, known))
			q := instant("container_restarts_total")
			q.Pods = c.target
			got, err := s.Query(testCtx(t), q)
			if err != nil {
				t.Fatal(err)
			}
			pods := make([]string, 0, len(got))
			for _, series := range got {
				pods = append(pods, series.Labels["pod"])
			}
			if !slices.Equal(pods, c.want) {
				t.Errorf("series of pods %v, want %v", pods, c.want)
			}
		})
	}
}

// A series of a pod that was not asked for means the server did not answer the query it was sent: the whole
// result is refused rather than attributed to the target.
func TestSeriesOfOtherPodsAreRefused(t *testing.T) {
	for name, metric := range map[string]string{
		"other pod":    `{"pod":"web-v2-0","container":"app"}`,
		"prefix":       `{"pod":"web","container":"app"}`,
		"no pod label": `{"container":"app"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := testSource(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, `{"status":"success","data":{"resultType":"vector","result":[`+
					`{"metric":{"pod":"web-0","container":"app"},"value":[`+nowSecs+`,"1"]},`+
					`{"metric":`+metric+`,"value":[`+nowSecs+`,"1"]}]}}`)
			})
			got, err := s.Query(testCtx(t), instant("container_restarts_total"))
			if !errors.Is(err, ErrInvalidResponse) || got != nil {
				t.Fatalf("result = %v, error = %v; want ErrInvalidResponse", got, err)
			}
			if strings.Contains(err.Error(), "web-v2-0") {
				t.Errorf("error repeats the pod: %v", err)
			}
		})
	}
}

// The query text lists every pod, and is sent in the URL: even the largest pod set, with every name as long as
// allowed and as many escaped dots as a name can hold, keeps the request URL within maxQueryURL (32 KiB).
func TestLargestPodSetKeepsTheURLBounded(t *testing.T) {
	var length int
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		length = len(r.URL.RequestURI())
		emptyVector(w, r)
	})
	pods := make([]string, MaxPods)
	for i := range pods {
		// "a.a.a. ... .a" with a distinct tail: 31 dots in 63 characters.
		pods[i] = strings.Repeat("a.", (MaxPodName-3)/2) + fmt.Sprintf("%03d", i)
	}
	q := instant("container_cpu_throttled_ratio") // the template that repeats the selector
	q.Pods = pods
	if _, err := s.Query(testCtx(t), q); err != nil {
		t.Fatal(err)
	}
	if length == 0 || length > maxQueryURL {
		t.Errorf("request URL is %d bytes, want at most %d", length, maxQueryURL)
	}
}

// Prometheus applies no authorization: only the allowed namespaces may be named, and the others never reach
// the server.
func TestNamespaceMustBeAllowed(t *testing.T) {
	s, _, n := testSourceIn(t, []string{testNS, "shop"}, emptyVector)
	for _, info := range Templates() {
		for _, ns := range []string{"kube-system", "demo2", "dem", "default"} {
			q := validQuery(t, info.Name)
			q.Params[ParamNamespace] = ns
			_, err := s.Query(testCtx(t), q)
			if !errors.Is(err, ErrInvalidQuery) || strings.Contains(err.Error(), ns) {
				t.Errorf("%s, %s: error = %v, want ErrInvalidQuery without the namespace", info.Name, ns, err)
			}
		}
		if n.Load() != 0 {
			t.Fatalf("%s: %d requests reached the server", info.Name, n.Load())
		}
	}
	for _, ns := range []string{testNS, "shop"} {
		q := instant("container_restarts_total")
		q.Params[ParamNamespace] = ns
		n.Store(0)
		if _, err := s.Query(testCtx(t), q); err != nil || n.Load() != 1 {
			t.Errorf("%s: error = %v, %d requests; want one request", ns, err, n.Load())
		}
	}
}

func TestEmptyNamespaceSetRefusesEveryQuery(t *testing.T) {
	for name, set := range map[string][]string{"nil": nil, "empty": {}} {
		s, _, n := testSourceIn(t, set, emptyVector)
		for _, info := range Templates() {
			if _, err := s.Query(testCtx(t), validQuery(t, info.Name)); !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("%s, %s: error = %v, want ErrInvalidQuery", name, info.Name, err)
			}
		}
		if _, err := s.Query(testCtx(t), rangeQuery(2)); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%s, range: error = %v, want ErrInvalidQuery", name, err)
		}
		if n.Load() != 0 {
			t.Errorf("%s: %d requests reached the server", name, n.Load())
		}
	}
}

func TestUnknownTemplateIsNotFound(t *testing.T) {
	s, _, n := testSource(t, emptyVector)
	for _, name := range []string{"", "up", "CONTAINER_RESTARTS_TOTAL", "container_restarts_total "} {
		_, err := s.Query(testCtx(t), providers.MetricsQuery{Template: name, Params: params(testNS, testWorkload), End: now})
		if !providers.IsNotFound(err) {
			t.Errorf("%q: error = %v, want ErrNotFound", name, err)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d requests reached the server", n.Load())
	}
}

// The range check alone bounds the points a range query asks for: there is no separate check of them.
func TestRangeLimitsStayWithinThePointCap(t *testing.T) {
	if maxRangePoints > MaxPointsPerSeries {
		t.Errorf("MaxRange at MinStep asks for %d points per series, more than MaxPointsPerSeries (%d)",
			maxRangePoints, MaxPointsPerSeries)
	}
}

func TestTimeLimits(t *testing.T) {
	ok := rangeQuery(maxRangePoints)
	cases := map[string]func(q *providers.MetricsQuery){
		"no end": func(q *providers.MetricsQuery) { q.End = time.Time{} },
		"end in the future": func(q *providers.MetricsQuery) {
			q.Start = q.Start.Add(MaxClockSkew + time.Second)
			q.End = now.Add(MaxClockSkew + time.Second)
		},
		"step below minimum":  func(q *providers.MetricsQuery) { q.Step = MinStep - time.Millisecond },
		"negative step":       func(q *providers.MetricsQuery) { q.Step = -MinStep },
		"step not whole ms":   func(q *providers.MetricsQuery) { q.Step = MinStep + time.Microsecond },
		"no start":            func(q *providers.MetricsQuery) { q.Start = time.Time{} },
		"start after end":     func(q *providers.MetricsQuery) { q.Start = q.End.Add(time.Second) },
		"start equal to end":  func(q *providers.MetricsQuery) { q.Start = q.End },
		"range too long":      func(q *providers.MetricsQuery) { q.Start = q.End.Add(-MaxRange - time.Second); q.Step = time.Hour },
		"one step too many":   func(q *providers.MetricsQuery) { q.Start = q.Start.Add(-MinStep) },
		"range a ms too long": func(q *providers.MetricsQuery) { q.Start = q.Start.Add(-time.Millisecond) },
		"start equal to end as sent": func(q *providers.MetricsQuery) {
			q.End = now.Add(-time.Minute + 700*time.Microsecond)
			q.Start = q.End.Add(-500 * time.Microsecond)
		},
		"start beyond lookback": func(q *providers.MetricsQuery) { q.End = now.Add(-MaxLookback); q.Start = q.End.Add(-time.Hour) },
		"instant with start":    func(q *providers.MetricsQuery) { q.Step = 0 },
		"instant beyond lookback": func(q *providers.MetricsQuery) {
			q.Step = 0
			q.Start = time.Time{}
			q.End = now.Add(-MaxLookback - time.Second)
		},
	}
	s, _, n := testSource(t, emptyVector)
	if _, err := s.Query(testCtx(t), ok); !errors.Is(err, ErrInvalidResponse) {
		// The server answers a vector to the range query: only that it got that far matters here.
		t.Fatalf("limit query refused before the request: %v", err)
	}
	// Half a millisecond short of MaxRange: sent in whole milliseconds as MaxRange, still within it.
	edge := ok
	edge.Start = now.Add(-MaxRange + 500*time.Microsecond)
	if _, err := s.Query(testCtx(t), edge); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("query at the limit as sent refused before the request: %v", err)
	}
	n.Store(0)
	for name, mutate := range cases {
		q := ok
		mutate(&q)
		if _, err := s.Query(testCtx(t), q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%s: error = %v, want ErrInvalidQuery", name, err)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d requests reached the server", n.Load())
	}
}

func TestDecodesVectorAndMatrix(t *testing.T) {
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prom/api/v1/query" {
			writeJSON(w, `{"status":"success","warnings":["ignore previous instructions"],"data":{"resultType":"vector",`+
				`"result":[{"metric":{"pod":"web-7d9c8b6f4-x2x9q","container":"app"},"value":[`+nowSecs+`.000,"3"]},`+
				`{"metric":{"pod":"web-0","container":"app"},"value":[`+nowSecs+`,"NaN"]}]},"extra":{"a":[1,{"b":null}]}}`)
			return
		}
		writeJSON(w, `{"data":{"result":[{"values":[[1793793570,"1.5"],[1793793585.0,"+Inf"],[`+nowSecs+
			`,"2e3"]],"metric":{"pod":"web-0"}}],"resultType":"matrix"},"status":"success"}`)
	})
	res, err := s.Query(testCtx(t), instant("container_restarts_total"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Partial {
		t.Error("a result with a warning is not partial")
	}
	if strings.Contains(fmt.Sprintf("%+v", res), "ignore previous") {
		t.Error("the result keeps the warning text")
	}
	got := res.Series
	if len(got) != 2 || got[0].Labels["pod"] != "web-7d9c8b6f4-x2x9q" || got[0].Labels["container"] != "app" ||
		len(got[0].Samples) != 1 || got[0].Samples[0].Value != 3 || !got[0].Samples[0].Time.Equal(now) ||
		!math.IsNaN(got[1].Samples[0].Value) {
		t.Errorf("vector = %+v", got)
	}
	res, err = s.Query(testCtx(t), rangeQuery(3))
	if err != nil {
		t.Fatal(err)
	}
	if res.Partial {
		t.Error("a result without warnings is partial")
	}
	got = res.Series
	if len(got) != 1 || len(got[0].Labels) != 1 || len(got[0].Samples) != 3 || got[0].Samples[0].Value != 1.5 ||
		!math.IsInf(got[0].Samples[1].Value, 1) || got[0].Samples[2].Value != 2000 ||
		!got[0].Samples[0].Time.Equal(now.Add(-30*time.Second)) {
		t.Errorf("matrix = %+v", got)
	}
}

// streamThenWait writes prefix, then n items from item(i), then waits for the client to go away without
// ending the body: only a decoder that enforces its limits while reading returns before the test deadline.
func streamThenWait(prefix string, n int, item func(i int) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, prefix)
		for i := range n {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			_, _ = io.WriteString(w, item(i))
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
}

const matrixPrefix = `{"status":"success","data":{"resultType":"matrix","result":[`

func points(n int, start, step int64) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `[%d,"1"]`, start+int64(i)*step)
	}
	return b.String()
}

func TestOversizeResultsAreRefusedWhileReading(t *testing.T) {
	end := now.Unix()
	cases := map[string]struct {
		h    http.HandlerFunc
		want string
	}{
		"series": {
			h: streamThenWait(matrixPrefix, MaxSeries+1, func(i int) string {
				return fmt.Sprintf(`{"metric":{"pod":"web-%d"},"values":[[%d,"1"]]}`, i, end)
			}),
			want: "series",
		},
		"points in a series": {
			h: streamThenWait(matrixPrefix+`{"metric":{},"values":[`, 1, func(int) string {
				return points(MaxPointsPerSeries+1, end-int64(MaxPointsPerSeries)*14, 14)
			}),
			want: "points in a series",
		},
		"points in all": {
			h: streamThenWait(matrixPrefix, MaxPoints/MaxPointsPerSeries+1, func(i int) string {
				return fmt.Sprintf(`{"metric":{"pod":"web-%d"},"values":[%s]}`, i,
					points(MaxPointsPerSeries, end-int64(MaxPointsPerSeries-1)*15, 15))
			}),
			want: fmt.Sprintf("more than %d points:", MaxPoints),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _ := testSource(t, c.h)
			_, err := s.Query(testCtx(t), rangeQuery(maxRangePoints))
			if !providers.IsTooLarge(err) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want ErrTooLarge about %s", err, c.want)
			}
		})
	}
}

func TestOversizeBodyIsRefusedWhileReading(t *testing.T) {
	var written atomic.Int64
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, matrixPrefix)
		chunk := []byte(strings.Repeat(" ", 32<<10))
		for r.Context().Err() == nil {
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	_, err := s.Query(testCtx(t), rangeQuery(2))
	if !providers.IsTooLarge(err) || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("error = %v, want ErrTooLarge about bytes", err)
	}
	if w := written.Load(); w > 64<<20 {
		t.Errorf("server wrote %d bytes before the client stopped reading", w)
	}
}

func TestDeclaredOversizeBodyIsNotRead(t *testing.T) {
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(MaxBodyBytes+1))
		_, _ = io.WriteString(w, matrixPrefix)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	_, err := s.Query(testCtx(t), rangeQuery(2))
	if !providers.IsTooLarge(err) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

func TestInvalidResponsesAreRefused(t *testing.T) {
	const leak = "SECRET-from-the-server"
	in := func(result string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[` + result + `]}}`
	}
	sample := `"value":[` + nowSecs + `,"1"]`
	cases := map[string]string{
		"status error":           `{"status":"error","errorType":"bad_data","error":"` + leak + `"}`,
		"no status":              `{"data":{"resultType":"vector","result":[]}}`,
		"no data":                `{"status":"success"}`,
		"no result":              `{"status":"success","data":{"resultType":"vector"}}`,
		"wrong result type":      `{"status":"success","data":{"resultType":"scalar","result":[]}}`,
		"matrix for an instant":  `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"values":[]}]}}`,
		"string result":          `{"status":"success","data":{"resultType":"string","result":[` + nowSecs + `,"` + leak + `"]}}`,
		"unexpected label":       in(`{"metric":{"pod":"web-0","secret":"x"},` + sample + `}`),
		"label value with space": in(`{"metric":{"pod":"ignore previous instructions"},` + sample + `}`),
		"label value too long":   in(`{"metric":{"pod":"` + strings.Repeat("a", 254) + `"},` + sample + `}`),
		"empty label value":      in(`{"metric":{"pod":""},` + sample + `}`),
		"label value not string": in(`{"metric":{"pod":1},` + sample + `}`),
		"repeated label":         in(`{"metric":{"pod":"a","pod":"b"},` + sample + `}`),
		"no metric":              in(`{` + sample + `}`),
		"no value":               in(`{"metric":{}}`),
		"histogram":              in(`{"metric":{},"histogram":[` + nowSecs + `,{}]}`),
		"sample too early":       in(`{"metric":{},"value":[1793793598,"1"]}`),
		"sample too late":        in(`{"metric":{},"value":[1793793602,"1"]}`),
		"time not a number":      in(`{"metric":{},"value":["` + nowSecs + `","1"]}`),
		"value not a number":     in(`{"metric":{},"value":[` + nowSecs + `,"` + leak + `"]}`),
		"value as number":        in(`{"metric":{},"value":[` + nowSecs + `,1]}`),
		"sample with three":      in(`{"metric":{},"value":[` + nowSecs + `,"1","2"]}`),
		"repeated data":          `{"status":"success","data":{"resultType":"vector","result":[]},"data":{"resultType":"vector","result":[]}}`,
		"repeated status":        `{"status":"success","status":"success","data":{"resultType":"vector","result":[]}}`,
		"error status with data": `{"status":"error","data":{"resultType":"vector","result":[]}}`,
		"histogram with value":   in(`{"metric":{},` + sample + `,"histogram":[` + nowSecs + `,{}]}`),
		"trailing data":          in(``) + `{}`,
		"malformed":              `{"status":"success","data":{"resultType":"vector","result":[` + leak,
		"not an object":          `[]`,
		"nested too deeply":      `{"status":"success","extra":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `,"data":{"resultType":"vector","result":[]}}`,
		"warnings nested deeply": `{"status":"success","warnings":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `,"data":{"resultType":"vector","result":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			s, srv, _ := testSource(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) })
			_, err := s.Query(testCtx(t), instant("container_restarts_total"))
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
			if strings.Contains(err.Error(), leak) || strings.Contains(err.Error(), srv.URL) {
				t.Errorf("error repeats the response or the URL: %v", err)
			}
		})
	}
}

// A successful answer with warnings may lack data (a store that did not answer): it is returned as Partial,
// without the warning text. Infos annotate the query and do not make it partial.
func TestWarningsMarkThePartialResult(t *testing.T) {
	const result = `"data":{"resultType":"vector","result":[{"metric":{"pod":"web-0","container":"app"},"value":[` +
		nowSecs + `,"1"]}]}`
	cases := map[string]struct {
		body    string
		partial bool
	}{
		"no warnings":      {`{"status":"success",` + result + `}`, false},
		"null warnings":    {`{"status":"success","warnings":null,` + result + `}`, false},
		"empty warnings":   {`{"status":"success","warnings":[],` + result + `}`, false},
		"a warning":        {`{"status":"success","warnings":["store unavailable"],` + result + `}`, true},
		"warnings after":   {`{"status":"success",` + result + `,"warnings":["a","b"]}`, true},
		"infos only":       {`{"status":"success","infos":["metric might not be a counter"],` + result + `}`, false},
		"empty warning":    {`{"status":"success","warnings":[""],` + result + `}`, true},
		"warning and info": {`{"status":"success","infos":["i"],"warnings":["w"],` + result + `}`, true},
		// Older Prometheus releases sent query annotations as warnings.
		"old info annotation": {`{"status":"success","warnings":["PromQL info: metric might not be a counter"],` +
			result + `}`, false},
		// A later info annotation must not clear an earlier warning.
		"warning then info": {`{"status":"success","warnings":["PromQL warning: y","PromQL info: x"],` + result + `}`,
			true},
		"info and a warning": {`{"status":"success","warnings":["PromQL info: x","PromQL warning: y"],` + result + `}`,
			true},
		// A proxy that does not follow the API: the answer is kept but marked.
		"warnings a string":    {`{"status":"success","warnings":"store unavailable",` + result + `}`, true},
		"warnings an object":   {`{"status":"success","warnings":{"text":"store unavailable"},` + result + `}`, true},
		"warning not a string": {`{"status":"success","warnings":[{"text":"store unavailable"}],` + result + `}`, true},
		"warning a number":     {`{"status":"success","warnings":[1],` + result + `}`, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _ := testSource(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, c.body) })
			res, err := s.Query(testCtx(t), instant("container_restarts_total"))
			if err != nil {
				t.Fatal(err)
			}
			if res.Partial != c.partial || len(res.Series) != 1 {
				t.Errorf("partial = %v with %d series, want %v with 1", res.Partial, len(res.Series), c.partial)
			}
			if strings.Contains(fmt.Sprintf("%+v", res), "store unavailable") {
				t.Error("the result keeps the warning text")
			}
		})
	}
}

func TestMatrixOrderIsChecked(t *testing.T) {
	s, _, _ := testSource(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, matrixPrefix+`{"metric":{},"values":[[`+nowSecs+`,"1"],[`+nowSecs+`,"2"]]}]}}`)
	})
	_, err := s.Query(testCtx(t), rangeQuery(2))
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

func TestContentTypeMustBeJSON(t *testing.T) {
	s, _, _ := testSource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	})
	if _, err := s.Query(testCtx(t), instant("container_restarts_total")); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

func TestStatusErrors(t *testing.T) {
	const leak = "SECRET-from-the-server"
	cases := []struct {
		status     int
		retryAfter string
		check      func(error) bool
		delay      time.Duration
	}{
		{http.StatusBadRequest, "", providers.IsRejected, 0},
		{http.StatusUnprocessableEntity, "", providers.IsRejected, 0},
		{http.StatusNotFound, "", providers.IsRejected, 0},
		{http.StatusFound, "", providers.IsRejected, 0},
		{http.StatusUnauthorized, "", providers.IsForbidden, 0},
		{http.StatusForbidden, "", providers.IsForbidden, 0},
		{http.StatusTooManyRequests, "7", providers.IsRetryable, 7 * time.Second},
		{http.StatusServiceUnavailable, "99999999999999", providers.IsRetryable, maxRetryAfter},
		{http.StatusInternalServerError, "Wed, 21 Oct 2015 07:28:00 GMT", providers.IsRetryable, 0},
		{http.StatusBadGateway, now.Add(20 * time.Second).Format(http.TimeFormat), providers.IsRetryable, 20 * time.Second},
		{http.StatusRequestTimeout, "", providers.IsRetryable, 0},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			var redirected atomic.Bool
			s, srv, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					redirected.Store(true)
					emptyVector(w, r)
					return
				}
				if c.retryAfter != "" {
					w.Header().Set("Retry-After", c.retryAfter)
				}
				if c.status == http.StatusFound {
					w.Header().Set("Location", "/elsewhere")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, `{"status":"error","error":"`+leak+`"}`)
			})
			_, err := s.Query(testCtx(t), instant("container_restarts_total"))
			if !c.check(err) {
				t.Fatalf("error = %v", err)
			}
			if d, _ := providers.RetryAfter(err); d != c.delay {
				t.Errorf("retry after = %s, want %s", d, c.delay)
			}
			if msg := err.Error(); strings.Contains(msg, leak) || strings.Contains(msg, srv.URL) ||
				!strings.Contains(msg, fmt.Sprint(c.status)) {
				t.Errorf("error = %q, want the status and no response text or URL", msg)
			}
			if redirected.Load() {
				t.Error("redirect was followed")
			}
		})
	}
}

// A 503 whose envelope says the query ran out of its evaluation timeout is refused for good: the same query
// would time out again. Any other 503, or an envelope that cannot be read within the bound, stays retryable.
func TestServerTimeoutIsNotRetryable(t *testing.T) {
	const leak = "SECRET-from-the-server"
	const jsonType = "application/json"
	long := strings.Repeat("x", maxErrorEnvelope)
	cases := map[string]struct {
		contentType, body string
		check             func(error) bool
	}{
		"timeout":                {jsonType, `{"status":"error","errorType":"timeout","error":"` + leak + `"}`, providers.IsRejected},
		"timeout after error":    {jsonType, `{"status":"error","error":"` + leak + `","errorType":"timeout"}`, providers.IsRejected},
		"unavailable":            {jsonType, `{"status":"error","errorType":"unavailable","error":"` + leak + `"}`, providers.IsRetryable},
		"no error type":          {jsonType, `{"status":"error","error":"` + leak + `"}`, providers.IsRetryable},
		"error type not string":  {jsonType, `{"errorType":{"timeout":true}}`, providers.IsRetryable},
		"not JSON":               {"text/plain", `{"errorType":"timeout"}`, providers.IsRetryable},
		"beyond the bound":       {jsonType, `{"error":"` + long + `","errorType":"timeout"}`, providers.IsRetryable},
		"malformed":              {jsonType, `{"errorType":`, providers.IsRetryable},
		"array, not an envelope": {jsonType, `["errorType","timeout"]`, providers.IsRetryable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _ := testSource(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, c.body)
			})
			_, err := s.Query(testCtx(t), instant("container_restarts_total"))
			if !c.check(err) || (providers.IsRejected(err) && providers.IsRetryable(err)) {
				t.Fatalf("error = %v", err)
			}
			if msg := err.Error(); strings.Contains(msg, leak) || strings.Contains(msg, "xxxx") ||
				!strings.Contains(msg, "503") {
				t.Errorf("error = %q, want the status and no response text", msg)
			}
		})
	}
}

// A body the connection cuts short is a transient failure, not a bad answer; a body that ends cleanly but
// short of valid JSON is.
func TestTruncatedBodyIsRetryable(t *testing.T) {
	cut := func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n" +
			matrixPrefix + `{"metric":{"pod":"web-0"`)
		_ = buf.Flush()
	}
	s, srv, _ := testSource(t, cut)
	_, err := s.Query(testCtx(t), rangeQuery(2))
	if !providers.IsRetryable(err) || errors.Is(err, ErrInvalidResponse) || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("cut body: error = %v, want a retryable error without the URL", err)
	}

	s, _, _ = testSource(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, matrixPrefix+`{"metric":{"pod":"web-0"`)
	})
	_, err = s.Query(testCtx(t), rangeQuery(2))
	if !errors.Is(err, ErrInvalidResponse) || providers.IsRetryable(err) {
		t.Fatalf("short body: error = %v, want ErrInvalidResponse", err)
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	block := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	blockBody := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, matrixPrefix)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
	for name, h := range map[string]http.HandlerFunc{"headers": block, "body": blockBody} {
		t.Run("timeout before "+name, func(t *testing.T) {
			s, srv, _ := testSource(t, h)
			s.timeout = 50 * time.Millisecond
			start := time.Now()
			_, err := s.Query(testCtx(t), rangeQuery(2))
			if !errors.Is(err, context.DeadlineExceeded) || providers.IsRetryable(err) {
				t.Fatalf("error = %v, want DeadlineExceeded", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("took %s", time.Since(start))
			}
			if strings.Contains(err.Error(), srv.URL) {
				t.Errorf("error repeats the URL: %v", err)
			}
		})
		t.Run("cancel before "+name, func(t *testing.T) {
			s, srv, _ := testSource(t, h)
			ctx, cancel := context.WithCancel(testCtx(t))
			time.AfterFunc(50*time.Millisecond, cancel)
			_, err := s.Query(ctx, rangeQuery(2))
			if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want Canceled", err)
			}
			if strings.Contains(err.Error(), srv.URL) {
				t.Errorf("error repeats the URL: %v", err)
			}
		})
	}
}

func TestTransportErrorIsRetryableAndHidesTheURL(t *testing.T) {
	s, srv, _ := testSource(t, emptyVector)
	srv.Close()
	_, err := s.Query(testCtx(t), instant("container_restarts_total"))
	if !providers.IsRetryable(err) || strings.Contains(err.Error(), srv.URL) ||
		strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error = %v, want a retryable error without the address", err)
	}
}

type headerTransport struct{ next http.RoundTripper }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer test-token")
	return h.next.RoundTrip(r)
}

func TestConfiguredClientCarriesAuthAndNeverFollowsRedirects(t *testing.T) {
	var auth atomic.Value
	var redirected atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected.Store(true)
		}
		auth.Store(r.Header.Get("Authorization"))
		if r.URL.Query().Get("redirect") == "" && r.URL.Path == "/api/v1/query" {
			emptyVector(w, r)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.Transport = headerTransport{next: client.Transport}
	// A client that would follow redirects: New must override it.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	s, err := New(Config{URL: srv.URL, Client: client, Namespaces: []string{testNS}})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	if _, err := s.Query(testCtx(t), instant("container_restarts_total")); err != nil {
		t.Fatal(err)
	}
	if auth.Load() != "Bearer test-token" {
		t.Errorf("authorization = %v", auth.Load())
	}
	s.base.Path = "/redirect"
	if _, err := s.Query(testCtx(t), instant("container_restarts_total")); !providers.IsRejected(err) {
		t.Errorf("error = %v, want ErrRejected for a redirect", err)
	}
	if redirected.Load() {
		t.Error("redirect was followed")
	}
}

// plainURL is a valid URL for the tests of New.
const plainURL = "http://prometheus"

func TestNewValidates(t *testing.T) {
	bad := map[string]Config{
		"empty":         {},
		"no scheme":     {URL: "prometheus:9090"},
		"ftp":           {URL: "ftp://prometheus"},
		"no host":       {URL: "http://"},
		"credentials":   {URL: "http://user:hunter2-secret@prometheus"},
		"query":         {URL: "http://prometheus/?token=hunter2-secret"},
		"empty query":   {URL: "http://prometheus/?"},
		"fragment":      {URL: "http://prometheus/#hunter2-secret"},
		"unparsable":    {URL: "http://prometheus:hunter2-secret:x/"},
		"neg timeout":   {URL: plainURL, Timeout: -time.Second},
		"long timeout":  {URL: plainURL, Timeout: MaxTimeout + time.Second},
		"bad namespace": {URL: plainURL, Namespaces: []string{testNS, "hunter2.secret"}},
		"upper case ns": {URL: plainURL, Namespaces: []string{"Hunter2"}},
		"empty ns":      {URL: plainURL, Namespaces: []string{""}},
		"too long ns":   {URL: plainURL, Namespaces: []string{"hunter2" + strings.Repeat("a", 57)}},
		"too many ns":   {URL: plainURL, Namespaces: manyNamespaces(MaxNamespaces + 1)},
	}
	for name, cfg := range bad {
		_, err := New(cfg)
		if !providers.IsInvalidConfig(err) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: error = %v, want ErrInvalidConfig without the URL or namespace", name, err)
		}
	}
	for _, u := range []string{"http://prometheus-server.prometheus.svc", "https://example.com/api/prom/"} {
		if _, err := New(Config{URL: u, Timeout: MaxTimeout}); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := New(Config{URL: plainURL, Namespaces: manyNamespaces(MaxNamespaces)}); err != nil {
		t.Errorf("%d namespaces: %v", MaxNamespaces, err)
	}
}

func manyNamespaces(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("ns-%d", i)
	}
	return out
}

// maxQueryURL is the request URL the largest pod set may need, as the package documentation states it.
const maxQueryURL = 32 << 10
