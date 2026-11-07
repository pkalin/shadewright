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

// Command gen creates the synthetic load of hack/load/measure.sh on the cluster of the current kubeconfig
// (the script points KUBECONFIG at its own Kind cluster): namespaces, Pods, a backlog of events.k8s.io/v1
// Events and a stream of them. Test tooling only; it deletes nothing.
//
// Usage: gen namespaces|pods|events|stream [flags]
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	prefix  = "load-"
	appName = "app"
)

type warning struct{ reason, note string }

// Typical Warning events of a cluster, with notes of a realistic length.
var warnings = []warning{
	{"BackOff", "Back-off restarting failed container app in pod %s"},
	{"FailedScheduling", "0/1 nodes are available: 1 Insufficient memory. " +
		"preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod."},
	{"Unhealthy", "Readiness probe failed: Get \"http://10.244.0.17:8080/healthz\": " +
		"context deadline exceeded (Client.Timeout exceeded while awaiting headers)"},
	{"FailedMount", "MountVolume.SetUp failed for volume \"config\" : configmap \"app-config\" not found"},
}

var normals = []warning{
	{"Scheduled", "Successfully assigned %s to node kind-control-plane"},
	{"Pulled", "Container image \"registry.example.com/team/app:1.4.2\" already present on machine"},
	{"Created", "Created container app"},
	{"Started", "Started container app"},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gen namespaces|pods|events|stream [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	namespaces := fs.Int("namespaces", 20, "number of load namespaces (load-0 ...)")
	pods := fs.Int("pods", 1000, "total Pods, spread over the namespaces")
	running := fs.Int("running", 40, "how many of the Pods can be scheduled and run (the rest stay Pending)")
	image := fs.String("image", "registry.k8s.io/pause:3.10", "image of the Pods")
	count := fs.Int("count", 10000, "events: Events to create")
	ratio := fs.Float64("warning-ratio", 0.1, "events, stream: share of Warning events")
	perMin := fs.Int("per-minute", 200, "stream: Events per minute")
	minutes := fs.Float64("minutes", 5, "stream: duration")
	hot := fs.Int("hot-pods", 20, "events, stream: Warning events repeat on this many Pods, so some Incidents open")
	_ = fs.Parse(os.Args[2:])

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	must(err)
	cfg.QPS, cfg.Burst = 200, 400
	cs, err := kubernetes.NewForConfig(cfg)
	must(err)
	ctx := context.Background()

	switch os.Args[1] {
	case "namespaces":
		for i := range *namespaces {
			n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns(i)}}
			_, err := cs.CoreV1().Namespaces().Create(ctx, n, metav1.CreateOptions{})
			ignoreExists(err)
		}
	case "pods":
		createPods(ctx, cs, *namespaces, *pods, *running, *image)
	case "events":
		parallel(*count, func(i int) { createEvent(ctx, cs, *namespaces, *pods, *hot, *ratio, i) })
	case "stream":
		stream(ctx, cs, *namespaces, *pods, *hot, *ratio, *perMin, *minutes)
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand", os.Args[1])
		os.Exit(2)
	}
}

func ns(i int) string { return fmt.Sprintf("%s%d", prefix, i) }

func podName(i int) string { return fmt.Sprintf("app-%d", i) }

// podNS spreads Pod i over the namespaces the same way for the Pods and for the events about them.
func podNS(i, namespaces int) string { return ns(i % namespaces) }

func createPods(ctx context.Context, cs kubernetes.Interface, namespaces, total, running int, image string) {
	parallel(total, func(i int) {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: podName(i), Namespace: podNS(i, namespaces),
				Labels:      map[string]string{appName: podName(i), "tier": "backend", "app.kubernetes.io/name": "app"},
				Annotations: map[string]string{"example.com/revision": "42", "example.com/owner": "team-a"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: appName, Image: image,
					Env: []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}, {Name: "REGION", Value: "westeurope"}},
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1m"), corev1.ResourceMemory: resource.MustParse("4Mi")}},
				}},
			},
		}
		if i >= running {
			// Never scheduled: the object is in the API server and in the analyzer's cache, without a container.
			p.Spec.NodeSelector = map[string]string{"load.shadewright.io/none": "true"}
		}
		_, err := cs.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{})
		ignoreExists(err)
	})
	// One Pod that fails for good: a symptom (CrashLoopBackOff, BackOff events) the collector turns into an Incident.
	_, err := cs.CoreV1().Pods(ns(0)).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "failing", Namespace: ns(0), Labels: map[string]string{appName: "failing"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: appName, Image: "busybox:1.37", Command: []string{"sh", "-c", "echo fail; exit 1"}}}},
	}, metav1.CreateOptions{})
	ignoreExists(err)
}

func createEvent(ctx context.Context, cs kubernetes.Interface, namespaces, pods, hot int, ratio float64, i int) {
	isWarning := rand.Float64() < ratio // #nosec G404 -- test load, not security
	set, typ := normals, corev1.EventTypeNormal
	if isWarning {
		set, typ = warnings, corev1.EventTypeWarning
	}
	w := set[rand.IntN(len(set))] // #nosec G404 -- test load, not security
	// Warnings repeat on a few hot Pods; everything else is spread over all Pods.
	pod := rand.IntN(pods)                 // #nosec G404 -- test load, not security
	if isWarning && rand.Float64() < 0.5 { // #nosec G404 -- test load, not security
		pod = rand.IntN(max(hot, 1)) // #nosec G404 -- test load, not security
	}
	name := podName(pod)
	namespace := podNS(pod, namespaces)
	note := w.note
	if containsVerb(note) {
		note = fmt.Sprintf(note, namespace+"/"+name)
	}
	now := metav1.NewMicroTime(time.Now())
	ev := &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-ev-", Namespace: namespace},
		EventTime:  now, Type: typ, Reason: w.reason, Note: note, Action: "Reconciling",
		ReportingController: "kubelet", ReportingInstance: "kind-control-plane",
		Regarding: corev1.ObjectReference{Kind: "Pod", Namespace: namespace, Name: name, APIVersion: "v1"},
	}
	_, err := cs.EventsV1().Events(namespace).Create(ctx, ev, metav1.CreateOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "event", i, err)
	}
}

func containsVerb(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '%' && s[i+1] == 's' {
			return true
		}
	}
	return false
}

func stream(ctx context.Context, cs kubernetes.Interface, namespaces, pods, hot int, ratio float64,
	perMin int, minutes float64) {
	tick := time.NewTicker(time.Minute / time.Duration(perMin))
	defer tick.Stop()
	end := time.Now().Add(time.Duration(minutes * float64(time.Minute)))
	for i := 0; time.Now().Before(end); i++ {
		<-tick.C
		go createEvent(ctx, cs, namespaces, pods, hot, ratio, i)
	}
}

// parallel runs f for 0..n-1 on 16 workers.
func parallel(n int, f func(int)) {
	var wg sync.WaitGroup
	next := make(chan int)
	for range 16 {
		wg.Go(func() {
			for i := range next {
				f(i)
			}
		})
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
}

func ignoreExists(err error) {
	if err != nil && !apierrors.IsAlreadyExists(err) {
		must(err)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}
