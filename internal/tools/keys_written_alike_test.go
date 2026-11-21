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

package tools

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Two map keys that encoding/json writes alike (an invalid byte and U+FFFD) are two members of the answer:
// get_resource reads each back with its own key and value, in the encoder's order, instead of failing to read the
// answer back (#873, review of 2026-11-21). The secret value under one of them is still redacted.
func TestGetResourceKeysWrittenAlike(t *testing.T) {
	set, src := diffSet(t)
	const secret = "ghp_FAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS},
		Spec: corev1.PodSpec{NodeSelector: map[string]string{
			"\xff": "first-value", "\ufffd": "second-value " + secret, "ok": "plain",
		}},
	}
	r := src.run(set, diffScene{pod: pod}, NameGetResource,
		map[string]any{argKind: kindPod, argNamespace: testNS, argName: podName})
	if r.Status != StatusOK {
		t.Fatalf("status %q, labels %v: want ok", r.Status, r.Labels)
	}
	for _, want := range []string{"first-value", "second-value", "plain"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("the answer lacks %q:\n%s", want, r.Text)
		}
	}
	if strings.Contains(r.Text, secret) {
		t.Errorf("the answer shows the token:\n%s", r.Text)
	}
}
