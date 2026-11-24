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

package anthropic

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/intake"
	"github.com/shadewright-labs/shadewright/internal/testutil/intakerun"
)

// The scoring intake's judge, run on this adapter over an httptest server: the second request, which carries
// the judge's first reply back with the tool result, is one the adapter builds and the API accepts, whatever
// the reply held next to its tool call.
func TestIntakeJudgeSecondRequestIsSent(t *testing.T) {
	const answer = `{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful","note":"ok"}`
	call := `{"type":"tool_use","id":"toolu_1","name":"cross_check","input":{"question":"Why 768Mi?"}}`
	cases := map[string]string{
		"text and tool call":           `[{"type":"text","text":"I will check the limit."},` + call + `]`,
		"thinking, text and tool call": `[{"type":"thinking","thinking":"","signature":"sig-1"},{"type":"text","text":"Checking."},` + call + `]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []json.RawMessage
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				var raw json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
					t.Errorf("decoding the request: %v", err)
				}
				if msg := apiTurnError(raw); msg != "" {
					t.Errorf("a request breaks the API's turn rules: %s", msg)
				}
				mu.Lock()
				defer mu.Unlock()
				bodies = append(bodies, raw)
				if len(bodies) == 1 {
					reply(w, messageJSON(testModel, stopToolUse, content))
					return
				}
				reply(w, messageJSON(testModel, stopEndTurn, `[{"type":"text","text":`+jsonString(answer)+`}]`))
			})
			cross := &intakerun.CrossModel{Answer: "Because the heap grew past 512Mi."}
			s, err := intakerun.Score(t, h.p, intake.FamilyAnthropic, cross)
			if err != nil {
				t.Fatalf("Score: %v (requests sent: %d)", err, h.requests.Load())
			}
			if s.Status != intake.StatusScored || s.CrossCheck != intake.CrossCheckAnswered || len(bodies) != 2 {
				t.Fatalf("status %q, cross-check %q, %d requests", s.Status, s.CrossCheck, len(bodies))
			}
		})
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
