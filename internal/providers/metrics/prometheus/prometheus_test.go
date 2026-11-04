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
	"slices"
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
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	s, err := New(Config{URL: srv.URL + "/prom"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	return s, srv, &n
}

func params(ns, w string) map[string]string {
	return map[string]string{ParamNamespace: ns, ParamWorkload: w}
}

func instant(template string) providers.MetricsQuery {
	return providers.MetricsQuery{Template: template, Params: params(testNS, testWorkload), End: now}
}

// rangeQuery asks for the given number of points per series, ending now.
func rangeQuery(points int) providers.MetricsQuery {
	return providers.MetricsQuery{
		Template: "container_restarts_total", Params: params(testNS, testWorkload),
		Start: now.Add(-time.Duration(points-1) * MinStep), End: now, Step: MinStep,
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
	const pods = `namespace="demo",pod=~"web-[a-z0-9]+(-[a-z0-9]+)?"`
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
			if _, err := s.Query(testCtx(t), instant(name)); err != nil {
				t.Fatal(err)
			}
			if got.Method != http.MethodGet || got.URL.Path != "/prom/api/v1/query" {
				t.Errorf("request = %s %s", got.Method, got.URL.Path)
			}
			q := got.URL.Query()
			if q.Get("query") != query {
				t.Errorf("query =\n%s\nwant\n%s", q.Get("query"), query)
			}
			if q.Get("time") != nowSecs+".000" || q.Get("timeout") != "30000ms" || len(q) != 3 {
				t.Errorf("parameters = %v", q)
			}
		})
	}
}

func TestRangeQueryParameters(t *testing.T) {
	var got *http.Request
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		writeJSON(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	})
	q := rangeQuery(241) // one hour
	if _, err := s.Query(testCtx(t), q); err != nil {
		t.Fatal(err)
	}
	v := got.URL.Query()
	if got.URL.Path != "/prom/api/v1/query_range" || v.Get("start") != "1793790000.000" ||
		v.Get("end") != nowSecs+".000" || v.Get("step") != "15000ms" || v.Has("time") {
		t.Errorf("request = %s %v", got.URL.Path, v)
	}
}

func TestWorkloadIsEscapedInRegexp(t *testing.T) {
	var query string
	s, _, _ := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		emptyVector(w, r)
	})
	q := instant("container_restarts_total")
	q.Params = params(testNS, "web.v2")
	if _, err := s.Query(testCtx(t), q); err != nil {
		t.Fatal(err)
	}
	if want := `pod=~"web\\.v2-[a-z0-9]+(-[a-z0-9]+)?"`; !strings.Contains(query, want) {
		t.Errorf("query = %s, want %s in it", query, want)
	}
}

func TestParametersAreValidated(t *testing.T) {
	cases := map[string]map[string]string{
		"quote and PromQL":       params(testNS, `web"} or vector(1) #`),
		"closing brace":          params(testNS, "web}"),
		"regexp wildcard":        params(testNS, "web.*"),
		"regexp alternation":     params(testNS, "web|db"),
		"regexp class":           params(testNS, "[a-z]+"),
		"backslash":              params(testNS, `web\`),
		"newline":                params(testNS, "web\nx"),
		"upper case":             params(testNS, "Web"),
		"unicode":                params(testNS, "wеb"), // Cyrillic e
		"empty workload":         params(testNS, ""),
		"too long workload":      params(testNS, strings.Repeat("a", 254)),
		"dot in namespace":       params("demo.x", testWorkload),
		"quote in namespace":     params(`demo",namespace=~".+`, testWorkload),
		"empty namespace":        params("", testWorkload),
		"too long namespace":     params(strings.Repeat("a", 64), testWorkload),
		"missing workload":       {ParamNamespace: testNS},
		"missing namespace":      {ParamWorkload: testWorkload},
		"extra parameter":        {ParamNamespace: testNS, ParamWorkload: testWorkload, "query": "up"},
		"no parameters":          nil,
		"double dot in workload": params(testNS, "web..v2"),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, n := testSource(t, emptyVector)
			for _, info := range Templates() {
				_, err := s.Query(testCtx(t), providers.MetricsQuery{Template: info.Name, Params: p, End: now})
				if !errors.Is(err, ErrInvalidQuery) {
					t.Fatalf("%s: error = %v, want ErrInvalidQuery", info.Name, err)
				}
				for _, v := range p {
					if v != "" && v != testNS && v != testWorkload && strings.Contains(err.Error(), v) {
						t.Errorf("error repeats the value: %v", err)
					}
				}
			}
			if n.Load() != 0 {
				t.Errorf("%d requests reached the server", n.Load())
			}
		})
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

func TestTimeLimits(t *testing.T) {
	ok := rangeQuery(MaxPointsPerSeries)
	cases := map[string]func(q *providers.MetricsQuery){
		"no end": func(q *providers.MetricsQuery) { q.End = time.Time{} },
		"end in the future": func(q *providers.MetricsQuery) {
			q.Start = q.Start.Add(MaxClockSkew + time.Second)
			q.End = now.Add(MaxClockSkew + time.Second)
		},
		"step below minimum":    func(q *providers.MetricsQuery) { q.Step = MinStep - time.Millisecond },
		"negative step":         func(q *providers.MetricsQuery) { q.Step = -MinStep },
		"step not whole ms":     func(q *providers.MetricsQuery) { q.Step = MinStep + time.Microsecond },
		"no start":              func(q *providers.MetricsQuery) { q.Start = time.Time{} },
		"start after end":       func(q *providers.MetricsQuery) { q.Start = q.End.Add(time.Second) },
		"start equal to end":    func(q *providers.MetricsQuery) { q.Start = q.End },
		"range too long":        func(q *providers.MetricsQuery) { q.Start = q.End.Add(-MaxRange - time.Second); q.Step = time.Hour },
		"too many points":       func(q *providers.MetricsQuery) { q.Start = q.Start.Add(-MinStep) },
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
			`,"2e3"]],"metric":{}}],"resultType":"matrix"},"status":"success"}`)
	})
	got, err := s.Query(testCtx(t), instant("container_restarts_total"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Labels["pod"] != "web-7d9c8b6f4-x2x9q" || got[0].Labels["container"] != "app" ||
		len(got[0].Samples) != 1 || got[0].Samples[0].Value != 3 || !got[0].Samples[0].Time.Equal(now) ||
		!math.IsNaN(got[1].Samples[0].Value) {
		t.Errorf("vector = %+v", got)
	}
	got, err = s.Query(testCtx(t), rangeQuery(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Labels) != 0 || len(got[0].Samples) != 3 || got[0].Samples[0].Value != 1.5 ||
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
			_, err := s.Query(testCtx(t), rangeQuery(MaxPointsPerSeries))
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
		"nested too deeply":      `{"status":"success","warnings":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `,"data":{"resultType":"vector","result":[]}}`,
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
	s, err := New(Config{URL: srv.URL, Client: client})
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

func TestNewValidates(t *testing.T) {
	bad := map[string]Config{
		"empty":        {},
		"no scheme":    {URL: "prometheus:9090"},
		"ftp":          {URL: "ftp://prometheus"},
		"no host":      {URL: "http://"},
		"credentials":  {URL: "http://user:hunter2-secret@prometheus"},
		"query":        {URL: "http://prometheus/?token=hunter2-secret"},
		"empty query":  {URL: "http://prometheus/?"},
		"fragment":     {URL: "http://prometheus/#hunter2-secret"},
		"unparsable":   {URL: "http://prometheus:hunter2-secret:x/"},
		"neg timeout":  {URL: "http://prometheus", Timeout: -time.Second},
		"long timeout": {URL: "http://prometheus", Timeout: MaxTimeout + time.Second},
	}
	for name, cfg := range bad {
		_, err := New(cfg)
		if !providers.IsInvalidConfig(err) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: error = %v, want ErrInvalidConfig without the URL", name, err)
		}
	}
	for _, u := range []string{"http://prometheus-server.prometheus.svc", "https://example.com/api/prom/"} {
		if _, err := New(Config{URL: u, Timeout: MaxTimeout}); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}
