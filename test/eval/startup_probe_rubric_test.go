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

package eval

import "testing"

// TestStartupProbeMemoryFact scores the memory fact of startup-probe alone (#843): the phrasings of real
// replies that rule memory out pass it, while a reply that blames memory or says nothing about it fails.
func TestStartupProbeMemoryFact(t *testing.T) {
	const fact = "no container was OOMKilled and nothing points at memory"
	s := mustLoad(t, "startup-probe")
	var r rubric
	for _, f := range s.rubric.Facts {
		if f.Fact == fact {
			r.Facts = append(r.Facts, f)
		}
	}
	if len(r.Facts) != 1 {
		t.Fatalf("the rubric of startup-probe has %d facts %q, want 1", len(r.Facts), fact)
	}
	cases := []struct {
		name string
		text string
		pass bool
	}{
		// Replies of real runs that state the fact in their own words: each passes.
		{
			name: "no evidence of the limit",
			text: "There is no evidence that the pod-template memory limit of 64Mi caused this failure.",
			pass: true,
		},
		{name: "not a memory issue", text: "It is not a memory issue.", pass: true},
		{
			name: "not memory or scaling",
			text: "This looks like a transient startup probe failure, not memory or scaling; no catalog action applies.",
			pass: true,
		},
		{
			name: "nothing points to memory",
			text: "No failure is ongoing and nothing points to memory, so no change is proposed.",
			pass: true,
		},
		{name: "limit not implicated", text: "The memory limit is not implicated.", pass: true},
		// A reply that says memory is the cause: none of the alternatives may match it.
		{name: "is a memory issue", text: "It is a memory issue.", pass: false},
		{name: "points to memory", text: "The restart pattern points to memory.", pass: false},
		{name: "limit implicated", text: "The memory limit is implicated.", pass: false},
		// Replies of real runs silent about memory: a real miss, so they still fail.
		{
			name: "silent, available",
			text: "Subject is Deployment demo/worker. One Unhealthy (Warning) readiness event occurred once, " +
				"at 2026-11-14T15:29:44Z, reported by kubelet: the probe on port 9898 got connection refused. " +
				"The replica set has 2 pods, both Running, ready true, restarts 0: worker-5d9f7c6b8-m4c8d " +
				"(running since 08:00) and worker-5d9f7c6b8-x7q2n (created 15:29:40Z, the likely source of the " +
				"event). No failing replica remains in the blocks, and the Deployment is Available=True.",
			pass: false,
		},
		{
			name: "silent, attribution",
			text: "Deployment demo/worker had one readiness probe failure because the connection to port 9898 " +
				"was refused. The affected pod is not identified. Both current pods, worker-5d9f7c6b8-m4c8d and " +
				"worker-5d9f7c6b8-x7q2n, are now ready with zero restarts; the Deployment reports 2 ready and " +
				"0 unavailable.",
			pass: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scoreDiagnosis(r, c.text, subjectText(s.incident))
			if got.Pass != c.pass {
				t.Errorf("fact passes = %v, want %v for %q (%+v)", got.Pass, c.pass, c.text, got.Facts)
			}
		})
	}
}
