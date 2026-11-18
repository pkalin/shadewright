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

package openaiwire

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// TestFoundryWireGolden pins requests, parsed replies and refusal errors generated before extraction.
func TestFoundryWireGolden(t *testing.T) {
	chatP := testCodec(APIChatCompletions)
	respP := testCodec(APIResponses)
	r := rand.New(rand.NewPCG(834, 501)) // #nosec G404 -- deterministic regression corpus
	var lines strings.Builder
	for range 200 {
		req := genRequest(r)
		a, _, err := chatP.encode(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := respP.encode(req)
		if err != nil {
			t.Fatal(err)
		}
		g := genReplyCase(r)
		structured := r.IntN(2) == 0
		c, e := parseResponse([]byte(g.chatJSON()), offeredTools(), structured)
		d, f := parseResponses([]byte(g.responsesJSON()), offeredTools(), structured)
		parsed, err := json.Marshal([]any{c, fmt.Sprint(e), d, fmt.Sprint(f)})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&lines, "%x %x %x\n", sha256.Sum256(a), sha256.Sum256(b), sha256.Sum256(parsed))
	}
	want, err := os.ReadFile("testdata/foundry-wire.sha256")
	if err != nil {
		t.Fatal(err)
	}
	if lines.String() != string(want) {
		t.Fatal("Foundry wire bytes or parse results changed")
	}
}

// TestFoundryDatedWireGolden pins the dated Chat Completions body (no model field).
// The corpus was generated with git show origin/main's Foundry encoder configured
// with APIVersion 2024-10-21, using the same seed and request generator.
func TestFoundryDatedWireGolden(t *testing.T) {
	p := testCodec(APIChatCompletions)
	p.SendModel = false
	r := rand.New(rand.NewPCG(834, 502)) // #nosec G404 -- deterministic regression corpus
	var lines strings.Builder
	for range 200 {
		body, _, err := p.encode(genRequest(r))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&lines, "%x\n", sha256.Sum256(body))
	}
	want, err := os.ReadFile("testdata/foundry-dated-wire.sha256")
	if err != nil {
		t.Fatal(err)
	}
	if lines.String() != string(want) {
		t.Fatal("dated Foundry wire bytes changed")
	}
}
