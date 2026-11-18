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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers/fake"
)

// The release before #845 read a value, then read it again with its own markers in place, and read the whole
// answer twice more: an address glued to a flag ("192.168.1.1--password") becomes "[ip]--password" in the first
// pass, and the second pass takes the line after the flag as its value. The redactor of that release is kept as
// one reading of each answer, applied where and as often as its tools applied it (#845, thirteenth decision of
// 2026-11-18), so the secret stays hidden at both entry points that carry such a text.
func TestTheReleasedReadingHidesWhatItsSecondPassFound(t *testing.T) {
	const secret = "UNIQUESecretProbe938"
	text := "192.168.1.1--password\n" + secret + "\n"
	p := minimalPod([]string{text})
	e := newEnv(t, []client.Object{p})
	e.logs.Set(fake.LogKey{Namespace: testNS, Pod: podName, Container: container}, text)
	for _, c := range []struct{ name, args string }{
		{NameGetResource, fmt.Sprintf(`{"kind":"Pod","namespace":%q,"name":%q}`, testNS, podName)},
		{NameGetPodLogs, fmt.Sprintf(`{"namespace":%q,"pod":%q,"container":%q}`, testNS, podName, container)},
	} {
		r := e.call(t, c.name, c.args)
		if r.Status != StatusOK {
			t.Fatalf("%s answered %s: %s", c.name, r.Status, r.Text)
		}
		if strings.Contains(r.Text, secret) || strings.Contains(r.Text, secret[:8]) {
			t.Errorf("%s shows the secret after the flag:\n%s", c.name, r.Text)
		}
		if !strings.Contains(r.Text, "--password") {
			t.Errorf("%s lost the flag itself:\n%s", c.name, r.Text)
		}
	}
}

// minimalPod is a pod with one container whose arguments are args, and nothing else.
func minimalPod(args []string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Args: args}}},
	}
}

// The answer of get_resource is valid JSON whenever the release before gave valid JSON: its size limit is
// fitted on the text after the replacements (#845, thirteenth decision of 2026-11-18). An argument that opens a
// key in a JSON string ("authorization\": \"") makes the next argument a value hidden on the answer as written,
// where a replacement may be longer than the one the release before wrote on the value. The second argument
// is sized so that the answer of the release before is exactly the limit.
func TestGetResourceIsValidJSONAtTheLimit(t *testing.T) {
	p := minimalPod([]string{"\nauthorization\": \"", strings.Repeat("x ", 8060) + "x"})
	e := newEnv(t, []client.Object{p})
	r := e.call(t, NameGetResource, fmt.Sprintf(`{"kind":"Pod","namespace":%q,"name":%q}`, testNS, podName))
	if r.Status != StatusOK {
		t.Fatalf("get_resource answered %s: %s", r.Status, r.Text)
	}
	if len(r.Text) > resourceBytes {
		t.Fatalf("answer of %d bytes, over the limit of %d", len(r.Text), resourceBytes)
	}
	if !json.Valid([]byte(r.Text)) {
		t.Errorf("answer of %d bytes is not valid JSON; it ends %q", len(r.Text), r.Text[max(0, len(r.Text)-80):])
	}
}
