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
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// ordinaryAnswersTable records the answers of list_events and get_resource for ordinary answers, under the largest
// entry the heap bound was measured for, as origin/main at ab900832 gave them, before #1106 bounded the entry of a
// read tool. The rows "generated 0" to "generated 3" hold control characters and were written again with the
// controls reading (#852), which hides more in such text: their notes differ from that commit's only by spans
// hidden, the kind of a placeholder, and one long reason that now fits its bound once redacted and is shown
// redacted. ORDINARY_ANSWERS_WRITE=1 writes the table again from the tools of the tree it runs in.
const ordinaryAnswersTable = "testdata/ordinary-answers-main.tsv"

// ordinaryRows is the number of generated scenes of list_events in the table.
const ordinaryRows = 8

// ordinaryCase is one answer of the table: the tool, its arguments and the scene.
type ordinaryCase struct {
	name  string
	tool  string
	args  map[string]any
	scene diffScene
}

// denseEvents returns n events about the pod, each with a note of size bytes made of unit repeated and starting
// with a secret, newest first (e0 is the newest).
func denseEvents(n, size int, unit string) []eventsv1.Event {
	evs := make([]eventsv1.Event, n)
	for i := range evs {
		head := fmt.Sprintf("event %d password=%s ", i, leakPassword)
		evs[i] = eventsv1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("e%d", i), Namespace: testNS},
			EventTime:  metav1.NewMicroTime(testNow.Add(-time.Duration(i) * time.Minute)),
			Type:       "Warning", Reason: "BackOff", Note: head + strings.Repeat(unit, (size-len(head))/len(unit)+1)[:size-len(head)],
			Regarding: corev1.ObjectReference{Kind: kindPod, Name: podName, Namespace: testNS},
		}
	}
	return evs
}

// ordinaryCases are the answers of the table: generated scenes of the fuzz grammar, and answers close to the
// largest entry (about 1.5 MiB) but under it.
func ordinaryCases() []ordinaryCase {
	events := map[string]any{argNamespace: testNS, argLimit: 50}
	cs := make([]ordinaryCase, 0, ordinaryRows+2)
	rng := rand.New(rand.NewPCG(1106, 0)) // #nosec G404 -- a fixed sequence of test inputs
	for i := range ordinaryRows {
		data := make([]byte, 16+rng.IntN(2048))
		for j := range data {
			data[j] = byte(rng.Uint32()) // #nosec G115 -- the low byte of each number is the input
		}
		cs = append(cs, ordinaryCase{name: fmt.Sprintf("generated %d", i), tool: NameListEvents, args: events,
			scene: diffScene{events: eventsScene(redacttest.Decode(data, 6))}})
	}
	words := "lorem ipsum dolor sit amet "
	cs = append(cs,
		ordinaryCase{name: "50 notes of 30 KiB", tool: NameListEvents, args: events,
			scene: diffScene{events: denseEvents(50, 30<<10, words)}},
		ordinaryCase{name: "an annotation of 1.4 MiB", tool: NameGetResource,
			args: map[string]any{argKind: kindPod, argNamespace: testNS, argName: podName},
			scene: diffScene{pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS,
				Annotations: map[string]string{"note": "password=" + leakPassword + " " + strings.Repeat(words, (1400<<10)/len(words))}}}}},
	)
	return cs
}

// answerDigest is the digest of a result: its status, text and values.
func answerDigest(r Result) string {
	h := sha256.New()
	h.Write([]byte(string(r.Status) + "\x00" + r.Text + "\x00" + strings.Join(r.Values, "\x00")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// An answer under the largest entry the heap bound was measured for is decided exactly as before #1106 (decision
// of 2026-11-22): the status, text and values of each answer of the table, digest for digest.
func TestOrdinaryAnswersAsOnMain(t *testing.T) {
	set, src := diffSet(t)
	cs := ordinaryCases()
	if raceDetector {
		cs = cs[:2]
	}
	if os.Getenv("ORDINARY_ANSWERS_WRITE") == "1" {
		var b strings.Builder
		b.WriteString("# The answers of TestOrdinaryAnswersAsOnMain at main ab900832, before #1106; generated 0 to 3 with the controls reading.\n# case\tdigest\n")
		for _, c := range cs {
			fmt.Fprintf(&b, "%s\t%s\n", c.name, answerDigest(src.run(set, c.scene, c.tool, c.args)))
		}
		if err := os.WriteFile(ordinaryAnswersTable, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := readOrdinaryAnswers(t)
	for _, c := range cs {
		r := src.run(set, c.scene, c.tool, c.args)
		if got, ok := want[c.name]; !ok {
			t.Errorf("%s: no row in %s", c.name, ordinaryAnswersTable)
		} else if d := answerDigest(r); d != got {
			t.Errorf("%s: answer %s, main answered %s; text %q", c.name, d, got, r.Text[:min(len(r.Text), 200)])
		}
	}
}

// readOrdinaryAnswers reads the table: the digest of each case.
func readOrdinaryAnswers(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(ordinaryAnswersTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			name, digest, ok := strings.Cut(line, "\t")
			if !ok {
				t.Fatalf("row %q has no digest", line)
			}
			rows[name] = digest
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}
