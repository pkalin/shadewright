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

package redact_test

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/tools"
)

// The memory get_resource takes grows with the size of the pod, not faster (#845, seventh decision of
// 2026-11-18, point 2). The layout of the head before this decision wrote a stand-in made of a private use
// character (U+E000) for every string, one character longer than the longest run of it in any value: a pod with
// one argument of n such characters and n more arguments made n stand-ins of n characters each, and a pod of
// 89 KB allocated 1,975 MB. Here the pods are made of the characters a layout of the answer could use: a run of
// private use characters, runs of quotes, of backslashes and of escaped quotes, each followed by as many short
// arguments, at n, 2n, 4n and 8n; the bytes allocated per byte of the pod as JSON may not double from the
// smallest to the largest.

// layoutPod returns a pod whose container arguments are runs of n characters that a layout of the answer could
// use, each followed by n arguments "x".
func layoutPod(n int) *corev1.Pod {
	runs := []string{"\uE000", `"`, `\`, `\"`, "\uE000\""}
	args := make([]string, 0, (1+n)*len(runs))
	for _, run := range []string{"", `"`, `\`, `\"`, "\""} {
		args = append(args, strings.Repeat(run, n))
		for range n {
			args = append(args, "x")
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fieldsPod, Namespace: fieldsNS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: fieldsContainer, Args: args}}},
	}
}

// layoutPodSizes are the n of layoutPod the cost is measured at.
var layoutPodSizes = []int{128, 256, 512, 1024}

// getResourceOf calls get_resource on pod and returns the size of the pod as JSON and the answer.
func getResourceOf(t *testing.T, e *fieldEnv, pod *corev1.Pod) (int, string) {
	t.Helper()
	e.reader.obj = pod
	raw, err := json.MarshalIndent(pod, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	text := callFieldTool(t, e.set, tools.NameGetResource, map[string]any{fieldsArgKind: "Pod", fieldsArgNS: fieldsNS, fieldsArgName: fieldsPod})
	if strings.Contains(text, "not available") {
		t.Fatalf("no answer: %.200s", text)
	}
	return len(raw), text
}

func TestGetResourceMemoryIsLinear(t *testing.T) {
	e := newFieldEnv(t)
	perByte := make([]float64, 0, len(layoutPodSizes))
	for _, n := range layoutPodSizes {
		pod := layoutPod(n)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		size, _ := getResourceOf(t, e, pod)
		runtime.ReadMemStats(&after)
		alloc := after.TotalAlloc - before.TotalAlloc
		perByte = append(perByte, float64(alloc)/float64(size))
		t.Logf("get_resource on a pod of %d bytes (n %d): %d bytes allocated, %.0f per byte", size, n, alloc, perByte[len(perByte)-1])
	}
	if last := perByte[len(perByte)-1]; last > 2*perByte[0] {
		t.Errorf("bytes allocated per byte of the pod grew from %.0f to %.0f at %d times the size", perByte[0], last,
			layoutPodSizes[len(layoutPodSizes)-1]/layoutPodSizes[0])
	}
}
