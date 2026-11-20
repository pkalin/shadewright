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

package kube

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// The generated corpus of the kubelet's answer for a container whose log it no longer holds: a logs request
// answered with HTTP 200 and the body "unable to retrieve container logs for <runtime>://<id>". Each input is
// built so that its right answer is known: a whole body that is the kubelet's sentence for the instance asked
// for is no log, and every log that merely holds the sentence (on one line among others, after a timestamp,
// with other text on its line, for another container) stays log text, whole.

const collectedPrefix = "unable to retrieve container logs for "

// corpusCase is one input of the corpus.
type corpusCase struct {
	name string
	// previous: the query asks for the previous instance; otherwise for the current one.
	previous bool
	// id is the container ID the pod's status gives for the instance asked for ("" for none).
	id string
	// body is what the logs request answers.
	body string
	// collected: the right answer is "no log"; otherwise body is the log.
	collected bool
}

var (
	corpusRuntimes = []string{"containerd", "cri-o", "docker", "cri-dockerd", "gvisor.runsc"}
	idChars        = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
)

// corpusID returns a runtime and ID as the pod status gives them: a known runtime or a made-up scheme, and a
// 64-digit hex ID or a shorter one of other characters.
func corpusID(rng *rand.Rand) string {
	scheme := corpusRuntimes[rng.IntN(len(corpusRuntimes))]
	if rng.IntN(4) == 0 {
		var b strings.Builder
		b.WriteByte("abcdefghijklmnopqrstuvwxyz"[rng.IntN(26)])
		for range rng.IntN(8) {
			b.WriteByte("abcdefghijklmnopqrstuvwxyz0123456789.+-"[rng.IntN(39)])
		}
		scheme = b.String()
	}
	var b strings.Builder
	if rng.IntN(2) == 0 {
		for range 64 {
			b.WriteByte("0123456789abcdef"[rng.IntN(16)])
		}
	} else {
		for range 1 + rng.IntN(40) {
			b.WriteByte(idChars[rng.IntN(len(idChars))])
		}
	}
	return scheme + "://" + b.String()
}

// corpusLine returns an ordinary log line.
func corpusLine(rng *rand.Rand) string {
	return []string{
		"level=info msg=\"starting server\" port=8080",
		"panic: runtime error: invalid memory address or nil pointer dereference",
		"Error: config file /etc/app/config.yaml: no such file or directory",
		"flag provided but not defined: -foo",
		"2026-11-20T10:00:00Z ERROR could not connect to db:5432",
	}[rng.IntN(5)]
}

// corpus builds the inputs from a fixed seed.
func corpus() []corpusCase {
	rng := rand.New(rand.NewPCG(943, 1)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var cs []corpusCase
	for i := range 200 {
		id := corpusID(rng)
		sentence := collectedPrefix + id
		previous := i%3 != 0
		known := id
		if rng.IntN(5) == 0 {
			known = "" // the status names no instance: the sentence alone decides
		}
		nl := []string{"", "\n"}[rng.IntN(2)]
		cs = append(cs, corpusCase{name: "kubelet answer", previous: previous, id: known, body: sentence + nl, collected: true})

		line, other := corpusLine(rng), corpusLine(rng)
		text := func(name, body string) {
			cs = append(cs, corpusCase{name: name, previous: previous, id: known, body: body})
		}
		text("sentence then a line", sentence+"\n"+line+"\n")
		text("a line then the sentence", line+"\n"+sentence+nl)
		text("sentence between lines", line+"\n"+sentence+"\n"+other+"\n")
		text("timestamp prefix", "2026-11-20T10:00:00.123456789Z "+sentence+"\n")
		text("leading space", " "+sentence+nl)
		text("text after it on its line", sentence+" (retrying in 5s)"+nl)
		text("prefixed on its line", "error: "+sentence+nl)
		text("twice", sentence+"\n"+sentence+"\n")
		text("CRLF", sentence+"\r\n")
		text("blank line after it", sentence+"\n\n")
		text("no scheme", collectedPrefix+strings.TrimPrefix(id, id[:strings.Index(id, "://")+3])+nl)
		text("capitalised", "U"+sentence[1:]+nl)
		if known != "" {
			// Another container's sentence, as a log collector might print it, is that log's text.
			text("another container", collectedPrefix+corpusID(rng)+nl)
		}
	}
	return cs
}

// podFor returns the test pod with id as the container ID of the instance asked for.
func podFor(c corpusCase) *corev1.Pod {
	pod := testPodObject()
	st := &pod.Status.ContainerStatuses[0]
	if c.previous {
		st.LastTerminationState.Terminated.ContainerID = c.id
	} else {
		st.ContainerID = c.id
	}
	return pod
}

func TestCollectedAnswerCorpus(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	collected, text := 0, 0
	for i, c := range corpus() {
		l := newLogServerFor(podFor(c), "")
		if c.previous {
			l.previous = []byte(c.body)
		} else {
			l.current = []byte(c.body)
		}
		s, err := New(Config{Pods: fakePods(t, l.cs), Namespaces: []string{testNS}, Redactor: red})
		if err != nil {
			t.Fatal(err)
		}
		q := query()
		q.Previous = c.previous
		got, err := s.Logs(context.Background(), q)
		if c.collected {
			collected++
			if !errors.Is(err, providers.ErrNotFound) || got.Text != "" {
				t.Errorf("%d %s (previous %v, id %q): body %q gave %q, %v; want no log, not found", i, c.name,
					c.previous, c.id, c.body, got.Text, err)
			}
			if !errors.Is(err, providers.ErrLogGone) {
				t.Errorf("%d %s: error %v does not match ErrLogGone", i, c.name, err)
			}
			continue
		}
		text++
		want, _ := Excerpt(s.redactor, []byte(c.body), false, DefaultBytes)
		if err != nil || got.Text != want {
			t.Errorf("%d %s (previous %v, id %q): body %q gave %q, %v; want the log text %q", i, c.name,
				c.previous, c.id, c.body, got.Text, err, want)
		}
	}
	if collected < 100 || text < 1000 {
		t.Fatalf("corpus too small: %d kubelet answers, %d logs", collected, text)
	}
}
