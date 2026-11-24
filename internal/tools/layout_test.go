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
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

// The pieces of get_resource are the string tokens of the JSON it returns (#845, seventh decision of 2026-11-18).

// A string inside a type that writes its own JSON is a piece like any other: a probe's port written as a flag and
// a value hides the value, as main hid it; the layout with stand-ins left the IntOrString's text out of the pieces
// and showed it.
func TestJSONAnswerDecidesStringsOfTypesThatWriteThemselves(t *testing.T) {
	const secret = "V9qSecret"
	e := newEnv(t, nil)
	type withRaw struct {
		Port intstr.IntOrString   `json:"port"`
		Raw  runtime.RawExtension `json:"raw"`
	}
	for name, obj := range map[string]any{
		"probe port": &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: container, LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromString("--password " + secret)}}},
		}}}},
		"raw string": &withRaw{Raw: runtime.RawExtension{Raw: []byte(`"password: ` + secret + `"`)}},
		"raw object": &withRaw{Raw: runtime.RawExtension{Raw: []byte(`{"password":"` + secret + `"}`)}},
		"raw list":   &withRaw{Raw: runtime.RawExtension{Raw: []byte(`["--password","` + secret + `"]`)}},
	} {
		text, _, err := e.set.jsonAnswer(obj, -1)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(text, secret) {
			t.Errorf("%s: the secret shows:\n%s", name, text)
		}
		if !json.Valid([]byte(text)) {
			t.Errorf("%s: the answer is no valid JSON:\n%s", name, text)
		}
	}
}

// layoutRunes are the characters the answer's JSON writes in a way of its own or that a layout could use: quotes,
// backslashes, control characters, the line separators JSON escapes, a private use character, U+FFFD, a
// character outside the BMP (a surrogate pair when escaped) and an invalid byte.
var layoutRunes = []string{`"`, `\`, `\"`, "\n", "\r", "\t", "\x00", "\x1f", "\x7f", " ", " ", "", "�", "\U0001F600", "\xff", "\xc3", "<", "&", "a", " ", "{", "}", "[", "]", ","}

// layoutWord returns a word of n characters of layoutRunes drawn by r.
func layoutWord(r *rand.Rand, n int) string {
	var b strings.Builder
	for range n {
		b.WriteString(layoutRunes[r.IntN(len(layoutRunes))])
	}
	return b.String()
}

// With values made of those characters, and no secret in them, the answer is valid JSON and decodes to the
// values as the release before wrote them (each run of invalid bytes as "?"), in arguments, map keys and values, and an
// IntOrString; with a secret flag and its value added to the arguments, the value is hidden and every other
// argument still decodes as it was. The right answer is known by construction: nothing of the generated values is
// a secret.
func TestJSONAnswerKeepsEveryValueItDoesNotHide(t *testing.T) {
	// answered is a value as the answer gives it: each run of invalid bytes one "?", as the release before made each
	// value valid UTF-8 before it encoded it (#845, fifteenth decision, P4).
	answered := func(s string) string { return strings.ToValidUTF8(s, "?") }
	const secret = "Qz7secretValue1"
	e := newEnv(t, nil)
	r := rand.New(rand.NewPCG(845, 7)) // #nosec G404 -- a reproducible test corpus, not a secret
	for turn := range 300 {
		var args []string
		labels := map[string]string{}
		for range 1 + r.IntN(6) {
			args = append(args, layoutWord(r, 1+r.IntN(12)))
			labels["k-"+layoutWord(r, 1+r.IntN(6))] = layoutWord(r, r.IntN(8))
		}
		port := layoutWord(r, 1+r.IntN(8))
		withSecret := turn%2 == 1
		if withSecret {
			args = append(args, "--password", secret)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Labels: labels},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: container, Args: args,
				LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(port)}}}}}},
		}
		text, _, err := e.set.jsonAnswer(pod, -1)
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		var got corev1.Pod
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatalf("turn %d: the answer is no valid JSON: %v\n%s", turn, err, text)
		}
		if strings.Contains(text, secret) {
			t.Errorf("turn %d: the secret shows", turn)
		}
		gotArgs := got.Spec.Containers[0].Args
		if withSecret {
			if gotArgs[len(gotArgs)-2] != "--password" {
				t.Errorf("turn %d: the flag is %q", turn, gotArgs[len(gotArgs)-2])
			}
			gotArgs, args = gotArgs[:len(gotArgs)-2], args[:len(args)-2]
		}
		want := make([]string, len(args))
		for i, a := range args {
			want[i] = answered(a)
		}
		if !slices.Equal(gotArgs, want) {
			t.Errorf("turn %d: arguments %q, want %q", turn, gotArgs, want)
		}
		wantLabels := map[string]string{}
		for k, v := range labels {
			wantLabels[answered(k)] = answered(v)
		}
		if (len(got.Labels) > 0 || len(wantLabels) > 0) && !reflect.DeepEqual(got.Labels, wantLabels) {
			t.Errorf("turn %d: labels %q, want %q", turn, got.Labels, wantLabels)
		}
		if p := got.Spec.Containers[0].LivenessProbe.TCPSocket.Port.String(); p != answered(port) {
			t.Errorf("turn %d: port %q, want %q", turn, p, answered(port))
		}
	}
}

// The text of a piece is the string of the object as it was read, not the token: an invalid byte, which the
// encoding writes as U+FFFD, reaches the redactor as the byte it is (read as indentation, unlike a letter).
func TestTokensReachTheStringsAsRead(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Labels: map[string]string{"k\xc3": "v\x80"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Args: []string{"a\xc3\xc3b", "�"}}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	w, err := tokens(raw, reflect.ValueOf(pod))
	if err != nil {
		t.Fatal(err)
	}
	var data []string
	for _, p := range w.ps {
		if p.Data {
			data = append(data, p.Text)
		}
	}
	for _, want := range []string{"k\xc3", "v\x80", "a\xc3\xc3b", "�", podName, container} {
		if !slices.Contains(data, want) {
			t.Errorf("no piece %q in %q", want, data)
		}
	}
	// A token that is not the encoding of the string it is read along fails the walk (fail closed).
	other := pod.DeepCopy()
	other.Spec.Containers[0].Args[0] = "changed"
	if _, err := tokens(raw, reflect.ValueOf(other)); err == nil {
		t.Error("a token read along another string was taken")
	}
}

// labelTier is the key of the pod's one label in TestTokensKeysAndWholeValues.
const labelTier = "tier"

// A map key is a value, a struct's field name is text the tool writes; values under a secret-named field are
// hidden whole, the key of that field not; a struct whose name is a secret key hides its value fields.
func TestTokensKeysAndWholeValues(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Labels: map[string]string{labelTier: "web"}},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
			Containers: []corev1.Container{{Name: container, LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Api-Key", Value: "hdr"}, {Name: "Accept", Value: "json"}}},
			}}}},
		},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	w, err := tokens(raw, reflect.ValueOf(pod))
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		key   string
		whole bool
		mapID int
	}
	pieces := map[string]got{}
	for i, p := range w.ps {
		if p.Data {
			pieces[p.Text] = got{p.Key, p.Whole, w.mapOf[i]}
		}
	}
	for text, want := range map[string]got{
		labelTier: {"", false, 0},
		"web":     {labelTier, false, -1},
		"regcred": {"name", true, -1},
		"hdr":     {"value", true, -1},
		"json":    {"value", false, -1},
		podName:   {"name", false, -1},
	} {
		if g, ok := pieces[text]; !ok || g != want {
			t.Errorf("piece %q: %+v (found %v), want %+v", text, g, ok, want)
		}
	}
	if _, ok := pieces["imagePullSecrets"]; ok {
		t.Error("a field name is a piece")
	}
	if w.maps != 1 {
		t.Errorf("%d maps, want 1", w.maps)
	}
}

// leaveOut keeps an answer within its bound as valid JSON (#845, fifteenth decision, P4), with answers known by
// construction: an object of two map keys decided alike (the second renamed " (2)") and two values. The smallest
// value whose note is enough goes first; keys go only when no value is left, and stay distinct; an answer whose
// text alone is over the bound is one note.
func TestLeaveOut(t *testing.T) {
	object := func(key, v1, v2 string) ([]string, walked) {
		ps := []redact.Piece{
			{Text: `{"`}, {Text: key, Data: true, MapKey: true}, {Text: `":"`}, {Text: v1, Data: true},
			{Text: `","`}, {Text: key, Data: true, MapKey: true}, {Text: `":"`}, {Text: v2, Data: true}, {Text: `"}`},
		}
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.Text
		}
		return out, walked{ps: ps, mapOf: []int{-1, 0, -1, -1, -1, 0, -1, -1, -1}, maps: 1}
	}
	long, longer := strings.Repeat("a", 100), strings.Repeat("b", 300)
	for _, tc := range []struct {
		name        string
		key, v1, v2 string
		limit       int
		want        string
		wantLeftOut bool
	}{
		{name: "fits", key: "k", v1: "x", v2: "y", limit: 100, want: `{"k":"x","k (2)":"y"}`},
		{name: "the smallest value that is enough", key: "k", v1: long, v2: longer, limit: 400,
			want: `{"k":"(100 bytes left out)","k (2)":"` + longer + `"}`, wantLeftOut: true},
		{name: "the largest value, then the next", key: "k", v1: long, v2: longer, limit: 80,
			want: `{"k":"(100 bytes left out)","k (2)":"(300 bytes left out)"}`, wantLeftOut: true},
		{name: "keys once no value is left", key: longer, v1: "x", v2: "y", limit: 100,
			want: `{"(300 bytes left out)":"x","(300 bytes left out) (2)":"y"}`, wantLeftOut: true},
		{name: "one note for the whole answer", key: "k", v1: long, v2: longer, limit: 30,
			want: `"(419 bytes left out)"`, wantLeftOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, w := object(tc.key, tc.v1, tc.v2)
			got, leftOut := leaveOut(out, w, tc.limit)
			if got != tc.want || leftOut != tc.wantLeftOut {
				t.Errorf("leaveOut = %s, %v; want %s, %v", got, leftOut, tc.want, tc.wantLeftOut)
			}
			if len(got) > tc.limit || !json.Valid([]byte(got)) {
				t.Errorf("%d bytes over the bound of %d, or not valid JSON: %s", len(got), tc.limit, got)
			}
		})
	}
}
