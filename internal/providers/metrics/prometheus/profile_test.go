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
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// The source profile: what each template computes when the source holds one scrape of the cluster, duplicate
// scrapes of it, a restarted container, or the series of two clusters (evalFake evaluates the queries).

// Names the tests repeat.
const (
	tWorkingSet  = "container_memory_working_set_bytes"
	tLimit       = "container_memory_limit_bytes"
	tCPU         = "container_cpu_usage_cores"
	tThrottled   = "container_cpu_throttled_ratio"
	tRestarts    = "container_restarts_total"
	tOOMKilled   = "container_last_terminated_oomkilled"
	tWaiting     = "container_waiting_reason"
	tUnavailable = "deployment_replicas_unavailable"

	jobLabel      = "job"
	kubeletJob    = "kubelet"
	testContainer = "app"
)

// truth is what one container of a cluster measures: the container app of the pod testStatefulPod.
type truth struct {
	workingSet, limit         float64 // bytes
	cpu                       float64 // cores
	throttled, periods        float64 // CFS periods per second
	restarts, oomKilled       float64
	crashLoop, unavailableDep float64
}

var labA = truth{workingSet: 200, limit: 400, cpu: 0.25, throttled: 2, periods: 10, restarts: 3, oomKilled: 1,
	crashLoop: 1, unavailableDep: 1}

var labB = truth{workingSet: 600, limit: 800, cpu: 0.75, throttled: 6, periods: 10, restarts: 5, oomKilled: 0,
	crashLoop: 0, unavailableDep: 2}

// scrape is where series come from: the cluster label a shared backend adds (empty for none), the jobs that
// collected them and the cgroup of the container instance.
type scrape struct {
	cluster, cadvisorJob, ksmJob, id string
}

func series(sc scrape, job, metric string, v float64, labels ...string) storedSeries {
	l := map[string]string{"__name__": metric, "namespace": testNS, jobLabel: job, "instance": job + "-0"}
	if sc.cluster != "" {
		l[clusterLabel] = sc.cluster
	}
	for i := 0; i+1 < len(labels); i += 2 {
		l[labels[i]] = labels[i+1]
	}
	return storedSeries{labels: l, value: v}
}

// cadvisor is the cAdvisor series of one container instance, as one job scraped it, with the pod-level cgroup
// series (no container label, or "POD") that the templates must leave out. Counters hold their rate.
func cadvisor(sc scrape, m truth) []storedSeries {
	c := func(metric string, v float64, container string) storedSeries {
		return series(sc, sc.cadvisorJob, metric, v, "pod", testStatefulPod, "container", container, "id", sc.id)
	}
	out := make([]storedSeries, 0, 12)
	for _, container := range []string{testContainer, "", "POD"} {
		scale := 1.0
		if container != testContainer {
			scale = 1000 // pod-level: large, so that counting it would show
		}
		out = append(out,
			c("container_memory_working_set_bytes", scale*m.workingSet, container),
			c("container_cpu_usage_seconds_total", scale*m.cpu, container),
			c("container_cpu_cfs_throttled_periods_total", scale*m.throttled, container),
			c("container_cpu_cfs_periods_total", scale*m.periods, container))
	}
	return out
}

// ksm is the kube-state-metrics series of the container, its pod and its Deployment, as one job scraped them.
func ksm(sc scrape, m truth) []storedSeries {
	k := func(metric string, v float64, labels ...string) storedSeries {
		return series(sc, sc.ksmJob, metric, v, append([]string{"pod", testStatefulPod, "container", testContainer},
			labels...)...)
	}
	return []storedSeries{
		k("kube_pod_container_resource_limits", m.limit, "resource", "memory", "unit", "byte"),
		k("kube_pod_container_resource_limits", 0.5, "resource", "cpu", "unit", "core"),
		k("kube_pod_container_status_restarts_total", m.restarts),
		k("kube_pod_container_status_last_terminated_reason", m.oomKilled, "reason", "OOMKilled"),
		k("kube_pod_container_status_waiting_reason", m.crashLoop, "reason", "CrashLoopBackOff"),
		k("kube_pod_container_status_waiting_reason", 0, "reason", "ImagePullBackOff"),
		series(sc, sc.ksmJob, "kube_deployment_status_replicas_unavailable", m.unavailableDep,
			"deployment", testWorkload),
	}
}

// cluster is one scrape of a whole cluster.
func cluster(sc scrape, m truth) []storedSeries {
	return append(cadvisor(sc, m), ksm(sc, m)...)
}

var (
	scrapeA  = scrape{cluster: "lab-a", cadvisorJob: kubeletJob, ksmJob: "kube-state-metrics", id: "/kubepods/pod-a/c1"}
	scrapeA2 = scrape{cluster: scrapeA.cluster, cadvisorJob: "cadvisor", ksmJob: "kube-state-metrics-2", id: scrapeA.id}
	scrapeB  = scrape{cluster: "lab-b", cadvisorJob: kubeletJob, ksmJob: scrapeA.ksmJob, id: "/kubepods/pod-b/c1"}
)

// measure runs every template against the stored series, with the source scoped to cluster (empty for none),
// and returns, per template, the value of the container app of testStatefulPod (CrashLoopBackOff for
// container_waiting_reason, the Deployment for deployment_replicas_unavailable), failing when a template gives
// another number of series than one or returns the cluster label.
func measure(t *testing.T, cluster string, stored []storedSeries) map[string]float64 {
	t.Helper()
	s, _, _ := testSourceWith(t, Config{Namespaces: []string{testNS}, Cluster: cluster}, evalFake(t, stored))
	got := map[string]float64{}
	for _, info := range Templates() {
		res, err := s.Query(testCtx(t), validQuery(t, info.Name))
		if err != nil {
			t.Fatalf("%s: %v", info.Name, err)
		}
		if len(res.Series) != 1 {
			t.Fatalf("%s: %d series, want 1: %v", info.Name, len(res.Series), res.Series)
		}
		if _, ok := res.Series[0].Labels[clusterLabel]; ok {
			t.Errorf("%s: the result keeps the cluster label: %v", info.Name, res.Series[0].Labels)
		}
		got[info.Name] = res.Series[0].Samples[0].Value
	}
	return got
}

// want is what each template must give for m.
func want(m truth) map[string]float64 {
	return map[string]float64{
		tWorkingSet:  m.workingSet,
		tLimit:       m.limit,
		tCPU:         m.cpu,
		tThrottled:   m.throttled / m.periods,
		tRestarts:    m.restarts,
		tOOMKilled:   m.oomKilled,
		tWaiting:     m.crashLoop,
		tUnavailable: m.unavailableDep,
	}
}

func checkValues(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for name, w := range want {
		if math.Abs(got[name]-w) > 1e-9 {
			t.Errorf("%s = %g, want %g", name, got[name], w)
		}
	}
	// The memory ratio a caller derives from the two memory templates.
	ratio := got[tWorkingSet] / got[tLimit]
	if w := want[tWorkingSet] / want[tLimit]; math.Abs(ratio-w) > 1e-9 {
		t.Errorf("working set / limit = %g, want %g", ratio, w)
	}
}

// One scrape of one cluster, with or without a cluster label on its series.
func TestOneScrape(t *testing.T) {
	checkValues(t, measure(t, "", cluster(scrapeA, labA)), want(labA))
	unlabelled := scrapeA
	unlabelled.cluster = ""
	checkValues(t, measure(t, "", cluster(unlabelled, labA)), want(labA))
}

// Two jobs collect the same cAdvisor and kube-state-metrics series: no value and no ratio may double.
func TestDuplicateScrapeJobs(t *testing.T) {
	stored := append(cluster(scrapeA, labA), cluster(scrapeA2, labA)...)
	checkValues(t, measure(t, "", stored), want(labA))
}

// The kubelet's resource endpoint gives a container's CPU and memory without the id label, next to cAdvisor's
// series with it; a scrape may also drop id altogether. Neither may count a container twice.
func TestSeriesWithoutID(t *testing.T) {
	resource := scrape{cluster: scrapeA.cluster, cadvisorJob: "kubelet-resource", ksmJob: scrapeA.ksmJob}
	resource2 := resource
	resource2.cadvisorJob = "kubelet-resource-2"
	cases := map[string][]storedSeries{
		"with and without id": append(cluster(scrapeA, labA), cadvisor(resource, labA)...),
		"without id, twice":   append(cluster(resource, labA), cadvisor(resource2, labA)...),
		"with id twice and without": append(append(cluster(scrapeA, labA), cadvisor(scrapeA2, labA)...),
			cadvisor(resource, labA)...),
	}
	for name, stored := range cases {
		t.Run(name, func(t *testing.T) {
			checkValues(t, measure(t, "", stored), want(labA))
		})
	}
}

// A restarted container has a cAdvisor series per container instance (a cgroup id each), here also scraped
// twice, and the resource endpoint's series without id. A gauge is the larger of the instances, never their
// sum; a CPU rate is the sum over the instances, each counted once, since each ran for its own part of the
// window; kube-state-metrics has one series per container.
func TestRestartedContainer(t *testing.T) {
	before := truth{workingSet: 150, cpu: 0.1, throttled: 1, periods: 4}
	after := truth{workingSet: 200, cpu: 0.15, throttled: 1, periods: 6}
	oldA, oldA2 := scrapeA, scrapeA2
	oldA.id, oldA2.id = "/kubepods/pod-a/c0", "/kubepods/pod-a/c0"
	resource := scrape{cluster: scrapeA.cluster, cadvisorJob: "kubelet-resource"}
	stored := make([]storedSeries, 0, 5*len(cadvisor(scrapeA, before))+2*len(ksm(scrapeA, labA)))
	for _, sc := range []scrape{oldA, oldA2} {
		stored = append(stored, cadvisor(sc, before)...)
	}
	for _, sc := range []scrape{scrapeA, scrapeA2} {
		stored = append(stored, cadvisor(sc, after)...)
		stored = append(stored, ksm(sc, labA)...)
	}
	stored = append(stored, cadvisor(resource, after)...) // the current instance only
	w := want(labA)
	w[tWorkingSet] = 200 // the larger instance, not 350
	w[tCPU] = 0.25       // 0.1 + 0.15, each instance once
	w[tThrottled] = 0.2  // (1 + 1) / (4 + 6)
	checkValues(t, measure(t, "", stored), w)
}

// Without a cluster setting, a source that holds two clusters with the same namespace, pod and container
// gives two series of one container that differ only in cluster: every template refuses the answer rather
// than mix them. A source that labels only some of its series is refused the same way.
func TestTwoClustersWithoutSettingAreRefused(t *testing.T) {
	unlabelled := scrapeB
	unlabelled.cluster = ""
	for name, stored := range map[string][]storedSeries{
		"two clusters":         append(cluster(scrapeA, labA), cluster(scrapeB, labB)...),
		"labelled and not":     append(cluster(scrapeA, labA), cluster(unlabelled, labB)...),
		"two clusters, copies": append(append(cluster(scrapeA, labA), cluster(scrapeA2, labA)...), cluster(scrapeB, labB)...),
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := testSourceWith(t, Config{Namespaces: []string{testNS}}, evalFake(t, stored))
			for _, info := range Templates() {
				if info.Name == tWaiting {
					continue // labB waits for nothing: one series only, from labA
				}
				res, err := s.Query(testCtx(t), validQuery(t, info.Name))
				if !errors.Is(err, ErrInvalidResponse) || res.Series != nil {
					t.Errorf("%s: result = %v, error = %v; want ErrInvalidResponse", info.Name, res.Series, err)
				}
			}
		})
	}
}

// With the cluster setting, every template selects only that cluster's series, duplicates included.
func TestClusterSettingSelectsOneCluster(t *testing.T) {
	stored := append(append(cluster(scrapeA, labA), cluster(scrapeA2, labA)...), cluster(scrapeB, labB)...)
	checkValues(t, measure(t, scrapeA.cluster, stored), want(labA))

	// labB waits for nothing, so its container_waiting_reason has no series; check the others.
	s, _, _ := testSourceWith(t, Config{Namespaces: []string{testNS}, Cluster: scrapeB.cluster}, evalFake(t, stored))
	for name, w := range want(labB) {
		res, err := s.Query(testCtx(t), validQuery(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == tWaiting {
			if len(res.Series) != 0 {
				t.Errorf("%s: %v, want no series", name, res.Series)
			}
			continue
		}
		if len(res.Series) != 1 || math.Abs(res.Series[0].Samples[0].Value-w) > 1e-9 {
			t.Errorf("%s = %v, want %g", name, res.Series, w)
		}
	}
}

// A cluster label value in a response must be a plain name (the syntax of Config.Cluster), and appear once.
func TestClusterLabelInResponseIsChecked(t *testing.T) {
	for name, c := range map[string]struct {
		metric string
		ok     bool
	}{
		"ARN":      {`{"pod":"web-0","container":"app","cluster":"arn:aws:eks:eu-west-1:1:cluster/lab"}`, true},
		"free":     {`{"pod":"web-0","container":"app","cluster":"lab a"}`, false},
		"repeated": {`{"pod":"web-0","container":"app","cluster":"a","cluster":"b"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := testSource(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, `{"status":"success","data":{"resultType":"vector","result":[`+
					`{"metric":`+c.metric+`,"value":[`+nowSecs+`,"1"]}]}}`)
			})
			res, err := s.Query(testCtx(t), instant(tRestarts))
			if c.ok && (err != nil || len(res.Series) != 1 || len(res.Series[0].Labels) != 2) {
				t.Errorf("result = %v, error = %v; want the series without its cluster label", res.Series, err)
			}
			if !c.ok && !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

// Every selector of every rendered template carries the cluster setting, quoted like a parameter.
func TestClusterIsInEverySelector(t *testing.T) {
	const scope = `namespace="demo",cluster="arn:aws:eks:eu-west-1:1:cluster/lab_1.west"`
	const pods = scope + `,pod=~"^(web-0|web-7d9c8b6f4-x2x9q)$"`
	exact := map[string]string{
		tWorkingSet: `max by (pod, container, cluster) (container_memory_working_set_bytes{` +
			pods + `,container!="",container!="POD"})`,
		tUnavailable: `max by (deployment, cluster) (kube_deployment_status_replicas_unavailable{` +
			scope + `,deployment="web"})`,
	}
	for _, info := range Templates() {
		t.Run(info.Name, func(t *testing.T) {
			var query string
			s, _, _ := testSourceWith(t, Config{
				Namespaces: []string{testNS}, Cluster: "arn:aws:eks:eu-west-1:1:cluster/lab_1.west",
			}, func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query().Get("query")
				emptyVector(w, r)
			})
			if _, err := s.Query(testCtx(t), validQuery(t, info.Name)); err != nil {
				t.Fatal(err)
			}
			selectors := strings.Count(query, "{")
			if selectors == 0 || strings.Count(query, `{`+scope+`,`) != selectors {
				t.Errorf("query %s: not every one of its %d selectors starts with %s", query, selectors, scope)
			}
			if w, ok := exact[info.Name]; ok && query != w {
				t.Errorf("query =\n%s\nwant\n%s", query, w)
			}
		})
	}
}

// A refused cluster setting stops New; the error never repeats the value.
func TestNewValidatesCluster(t *testing.T) {
	const secret = "hunter2" // what no error may repeat
	for _, value := range []string{
		`x",namespace=~".+`, "x}", secret + " x", "-" + secret, secret + ".", secret + "/", secret + "\n",
		secret + strings.Repeat("a", MaxClusterValue-len(secret)+1), secret + "é", secret + ".*", secret + `\`,
		secret + "=x", secret + ",x",
	} {
		_, err := New(Config{URL: plainURL, Cluster: value})
		if !providers.IsInvalidConfig(err) || strings.Contains(err.Error(), secret) {
			t.Errorf("%q: error = %v, want ErrInvalidConfig without the value", value, err)
		}
	}
	for _, v := range []string{"", "a", "aks-lab_1.west", "AKS-Lab", "prod/eu-west-1",
		"arn:aws:eks:eu-west-1:123456789012:cluster/lab", strings.Repeat("a", MaxClusterValue)} {
		if _, err := New(Config{URL: plainURL, Cluster: v}); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
}
