//go:build redactwork

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
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// The cost of get_resource on a large pod, by work count (#845, third decision of 2026-11-18): the object is
// laid out as one text and decided once, so the work is linear in the size of the answer. The pod has n
// containers, each with arguments, a command, labels of the pod, ports, probes with headers and volume
// mounts; the work per byte of the object as JSON (the answer before its cut to 16 KiB) is logged and must stay
// the same at n and 4n.

// largePod returns a pod with n containers.
func largePod(n int) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fieldsPod, Namespace: fieldsNS, Labels: map[string]string{}}}
	for i := range n {
		p.Labels[fmt.Sprintf("app.kubernetes.io/part-%d", i)] = "shop"
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{
			Name: fmt.Sprintf("c%d", i), Image: "registry.example/shop/api:1.4.2",
			Command: []string{"/bin/api", "serve"},
			Args:    []string{"--port=8080", "--log-level", "info", "--config", "/etc/api/config.yaml", "--password", "x"},
			Ports:   []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Probe", Value: "1"}},
			}}},
			VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/api"}},
		})
	}
	return p
}

// getResourceMainWork holds main's work on the same pods, "containers<TAB>steps<TAB>bytes", written by
// TestWriteGetResourceMainWork in a checkout of main with the tag redactwork (the commit its header names). This
// file no longer runs unchanged there: it needs getresource_alloc_test.go (layoutPod, getResourceOf) and
// fields_entry_test.go (newFieldEnv, callFieldTool), and fields_entry_test.go needs the exports of
// export_keys_test.go and export_test.go, which name code main does not have. Writing the table again means
// copying this file with only the helpers TestWriteGetResourceMainWork calls.
const getResourceMainWork = "testdata/getresource-main-work.tsv"

// getResourceWork returns the work of get_resource on a pod of n containers and the size of the pod as JSON.
func getResourceWork(t *testing.T, n int) (int64, int) {
	t.Helper()
	e := newFieldEnv(t)
	e.reader.obj = largePod(n)
	raw, err := json.MarshalIndent(e.reader.obj, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	var text string
	work := redact.Work(func() {
		text = callFieldTool(t, e.set, tools.NameGetResource, map[string]any{fieldsArgKind: "Pod", fieldsArgNS: fieldsNS, fieldsArgName: fieldsPod})
	})
	if strings.Contains(text, "not available") {
		t.Fatalf("no answer: %s", text)
	}
	return work, len(raw)
}

// getResourceSizes are the numbers of containers the cost is measured at.
var getResourceSizes = []int{64, 256}

func TestGetResourceWorkIsLinear(t *testing.T) {
	main := readGetResourceMainWork(t)
	var perByte []float64
	for _, n := range getResourceSizes {
		work, size := getResourceWork(t, n)
		if work <= 0 {
			t.Fatalf("get_resource on a pod of %d containers counted %d steps", n, work)
		}
		perByte = append(perByte, float64(work)/float64(size))
		t.Logf("get_resource on a pod of %d containers: %d steps for %d bytes of JSON, %.1f per byte; main %.1f per byte",
			n, work, size, perByte[len(perByte)-1], main[n])
	}
	if perByte[1] > 1.1*perByte[0] {
		t.Errorf("work per byte grew from %.1f to %.1f at four times the containers", perByte[0], perByte[1])
	}
}

// readGetResourceMainWork reads getResourceMainWork: main's steps per byte by number of containers.
func readGetResourceMainWork(t *testing.T) map[int]float64 {
	t.Helper()
	raw, err := os.ReadFile(getResourceMainWork)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]float64{}
	for line := range strings.Lines(string(raw)) {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		var n int
		var steps, size int64
		if _, err := fmt.Sscanf(line, "%d\t%d\t%d", &n, &steps, &size); err != nil {
			t.Fatalf("%s: %q: %v", getResourceMainWork, line, err)
		}
		out[n] = float64(steps) / float64(size)
	}
	for _, n := range getResourceSizes {
		if _, ok := out[n]; !ok {
			t.Fatalf("%s has no row for %d containers (write it again from main)", getResourceMainWork, n)
		}
	}
	return out
}

// TestWriteGetResourceMainWork writes getResourceMainWork from the code it runs with, when
// GETRESOURCE_MAIN_WORK names the output path (and GETRESOURCE_MAIN_COMMIT the commit); it is run once with the
// tag redactwork in a checkout of main.
func TestWriteGetResourceMainWork(t *testing.T) {
	path := os.Getenv("GETRESOURCE_MAIN_WORK")
	if path == "" {
		t.Skip("GETRESOURCE_MAIN_WORK is not set")
	}
	var b strings.Builder
	b.WriteString("# The work (steps, -tags redactwork) of get_resource on a pod of n containers (#845), at main " +
		os.Getenv("GETRESOURCE_MAIN_COMMIT") + ".\n# containers\tsteps\tbytes of the pod as JSON\n")
	for _, n := range getResourceSizes {
		work, size := getResourceWork(t, n)
		if work == 0 {
			t.Fatal("no work counted: run with -tags redactwork")
		}
		fmt.Fprintf(&b, "%d\t%d\t%d\n", n, work, size)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}

// The work of get_resource on pods made of the characters a layout of the answer could use (layoutPod: runs of
// private use characters, quotes, backslashes and escaped quotes, each followed by as many short arguments) is
// linear too (#845, seventh decision of 2026-11-18, point 2). The read tools count the bytes of the answer they
// walk (redact.Tick) with the redactor's work, so a layout that grew faster than the pod would show here; the
// layout with stand-ins before this decision did, and counted nothing.
func TestGetResourceLayoutWorkIsLinear(t *testing.T) {
	e := newFieldEnv(t)
	var perByte []float64
	for _, n := range layoutPodSizes {
		pod := layoutPod(n)
		size := podJSONSize(t, pod)
		work := redact.Work(func() { getResourceOf(t, e, pod) })
		if work <= 0 {
			t.Fatalf("get_resource on a pod of %d bytes (n %d) counted %d steps", size, n, work)
		}
		perByte = append(perByte, float64(work)/float64(size))
		t.Logf("get_resource on a pod of %d bytes (n %d): %d steps, %.1f per byte", size, n, work, perByte[len(perByte)-1])
	}
	if last := perByte[len(perByte)-1]; last > 1.1*perByte[0] {
		t.Errorf("work per byte grew from %.1f to %.1f at %d times the size", perByte[0], last,
			layoutPodSizes[len(layoutPodSizes)-1]/layoutPodSizes[0])
	}
}
