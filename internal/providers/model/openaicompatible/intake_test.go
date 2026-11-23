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

package openaicompatible

import (
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/pkalin/shadewright/internal/intake"
	"github.com/pkalin/shadewright/internal/testutil/intakerun"
)

// The scoring intake's judge, run on this adapter: the second request, which carries the judge's first reply
// (text next to its tool call) back with the tool result, is built and sent.
func TestIntakeJudgeSecondRequestIsSent(t *testing.T) {
	const answer = `{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful","note":"ok"}`
	var mu sync.Mutex
	sent := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		defer mu.Unlock()
		sent++
		if sent == 1 {
			reply(w, completionJSON("tool_calls", `{"role":"assistant","content":"I will check the limit.",`+
				`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"cross_check","arguments":`+
				strconv.Quote(`{"question":"Why 768Mi?"}`)+`}}]}`))
			return
		}
		reply(w, completionJSON("stop", `{"role":"assistant","content":`+strconv.Quote(answer)+`}`))
	}, nil)
	cross := &intakerun.CrossModel{Answer: "Because the heap grew past 512Mi."}
	s, err := intakerun.Score(t, h.p, intake.FamilyOpenAI, cross)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if s.Status != intake.StatusScored || s.CrossCheck != intake.CrossCheckAnswered || sent != 2 {
		t.Fatalf("status %q, cross-check %q, %d requests", s.Status, s.CrossCheck, sent)
	}
}
