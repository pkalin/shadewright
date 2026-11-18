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

func TestGetResourceWorkIsLinear(t *testing.T) {
	var perByte []float64
	for _, n := range []int{64, 256} {
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
		perByte = append(perByte, float64(work)/float64(len(raw)))
		t.Logf("get_resource on a pod of %d containers: %d steps for %d bytes of JSON, %.1f per byte", n, work, len(raw), perByte[len(perByte)-1])
	}
	if perByte[1] > 1.1*perByte[0] {
		t.Errorf("work per byte grew from %.1f to %.1f at four times the containers", perByte[0], perByte[1])
	}
}
