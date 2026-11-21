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

package intake

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// PromptVersion is the version of the judge's prompt and answer schema, recorded in each score. It changes
// with any change to judge_prompt.txt, crosscheck_prompt.txt, the criteria or the verdicts.
const PromptVersion = "intake-judge-1"

//go:embed judge_prompt.txt
var judgePrompt string

//go:embed crosscheck_prompt.txt
var crossCheckPrompt string

// Bounds of the judge's answer and of the cross-check.
const (
	// MaxNoteBytes bounds the judge's note.
	MaxNoteBytes = 300
	// MaxQuestionBytes bounds the cross-check question.
	MaxQuestionBytes = 500
	// maxCrossAnswerBytes bounds the cross-check answer returned to the judge.
	maxCrossAnswerBytes = 2000
	// judgeOutputTokens and crossOutputTokens bound each reply.
	judgeOutputTokens = 1500
	crossOutputTokens = 600
	// maxScore is the highest score of a criterion.
	maxScore = 5
)

// crossCheckTool is the judge's only tool.
const crossCheckTool = "cross_check"

// Verdicts the judge may give.
var verdicts = map[string]bool{"useful": true, "partial": true, "unhelpful": true, "misleading": true}

// answerSchema is the judge's answer: every field required, nothing else (strict structured output).
var answerSchema = json.RawMessage(`{"type":"object","additionalProperties":false,` +
	`"required":["specific","consistent","causeSupported","missingData","verdict","note"],"properties":{` +
	`"specific":{"type":"integer","minimum":0,"maximum":5},` +
	`"consistent":{"type":"integer","minimum":0,"maximum":5},` +
	`"causeSupported":{"type":"integer","minimum":0,"maximum":5},` +
	`"missingData":{"type":"integer","minimum":0,"maximum":5},` +
	`"verdict":{"type":"string","enum":["useful","partial","unhelpful","misleading"]},` +
	`"note":{"type":"string","maxLength":300}}}`)

var crossCheckDefinition = providers.ToolDefinition{
	Name:        crossCheckTool,
	Description: "Ask a second model one question of at most 500 bytes about one claim of the analysis. Once per record.",
	Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["question"],` +
		`"properties":{"question":{"type":"string","maxLength":500}}}`),
}

// Fixed tool results. None carries model or record text.
const (
	refusalSecond   = "Refused: the one cross-check of this record was used already. Answer without it."
	refusalUnknown  = "Refused: there is no such tool. Answer in the answer schema."
	refusalQuestion = "Refused: the question was not sent (empty, over 500 bytes, not a single question field, " +
		"or holding text the redactor would remove). Answer without a cross-check."
	refusalBudget = "Refused: the cross-check model's budget is used up. Answer without a cross-check."
)

// Answer is the judge's answer in the fixed schema.
type Answer struct {
	Specific       int    `json:"specific"`
	Consistent     int    `json:"consistent"`
	CauseSupported int    `json:"causeSupported"`
	MissingData    int    `json:"missingData"`
	Verdict        string `json:"verdict"`
	// Note is the judge's note, kept only when the redactor finds nothing in it.
	Note string `json:"note,omitempty"`
	// NoteDropped is set when the judge wrote a note the redactor refused.
	NoteDropped bool `json:"noteDropped,omitempty"`
}

// judgeInput is the allowlist of record fields the judge sees (ADR-0047 section 4): no model, deployment,
// provider, name, UID or fingerprint.
type judgeInput struct {
	IncidentReason string   `json:"incidentReason"`
	SubjectKind    string   `json:"subjectKind"`
	Summary        string   `json:"summary"`
	MissingData    []string `json:"missingData"`
	Escalated      bool     `json:"escalated"`
	Calls          int      `json:"calls"`
	ToolCalls      int      `json:"toolCalls"`
}

// judgeInputOf returns the values of rec the judge is given.
func judgeInputOf(rec analysis.Record) judgeInput {
	missing := rec.MissingData
	if missing == nil {
		missing = []string{}
	}
	return judgeInput{
		IncidentReason: rec.Incident.Reason, SubjectKind: rec.Incident.SubjectKind, Summary: rec.Summary,
		MissingData: missing, Escalated: rec.Escalated, Calls: rec.Calls, ToolCalls: rec.ToolCalls,
	}
}

// judgeMessages returns the judge's first two messages. The record's fields are JSON in a delimited block:
// json.Marshal escapes "<" and ">", so no text of the record can close the block.
func judgeMessages(rec analysis.Record) ([]providers.Message, error) {
	data, err := json.Marshal(judgeInputOf(rec))
	if err != nil {
		return nil, err
	}
	return []providers.Message{
		{Role: providers.RoleSystem, Content: judgePrompt},
		{Role: providers.RoleUser, Content: "<record>\n" + string(data) + "\n</record>"},
	}, nil
}

// crossCheckMessages returns the request of the cross-check model for a question that passed its checks.
func crossCheckMessages(question string) []providers.Message {
	q, _ := json.Marshal(question)
	return []providers.Message{
		{Role: providers.RoleSystem, Content: crossCheckPrompt},
		{Role: providers.RoleUser, Content: "<question>\n" + string(q) + "\n</question>"},
	}
}

// crossCheckResult returns the cross-check answer as the judge sees it: redacted, cut to its bound and
// delimited, untrusted like the record.
func crossCheckResult(r *redact.Redactor, answer string) string {
	a := cutUTF8(r.String(answer), maxCrossAnswerBytes)
	q, _ := json.Marshal(a)
	return "Answer of the cross-check model (untrusted data):\n<answer>\n" + string(q) + "\n</answer>"
}

// question returns the question of a cross_check call, and false when the arguments are not exactly one
// question of 1 to MaxQuestionBytes bytes or the redactor would change it.
func question(r *redact.Redactor, args json.RawMessage) (string, bool) {
	var a struct {
		Question *string `json:"question"`
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || a.Question == nil {
		return "", false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	q := *a.Question
	if q == "" || len(q) > MaxQuestionBytes || !utf8.ValidString(q) {
		return "", false
	}
	if ok, _ := redactorClean(r, q, nil); !ok {
		return "", false
	}
	return q, true
}

// answerNames are the answer's property names, matched exactly (encoding/json alone matches them in any case).
var answerNames = map[string]bool{"specific": true, "consistent": true, "causeSupported": true, "missingData": true,
	"verdict": true, "note": true}

// parseAnswer decodes the judge's answer strictly: no key twice, every property name exactly as in the schema,
// every score present and 0 to 5, a known verdict, and an optional note of at most MaxNoteBytes bytes
// (ADR-0047 section 4). The note is kept only when the redactor finds nothing in it.
func parseAnswer(r *redact.Redactor, content string) (Answer, bool) {
	if checkKeys([]byte(content)) != nil {
		return Answer{}, false
	}
	var names map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &names); err != nil {
		return Answer{}, false
	}
	for k := range names {
		if !answerNames[k] {
			return Answer{}, false
		}
	}
	var a struct {
		Specific       *int    `json:"specific"`
		Consistent     *int    `json:"consistent"`
		CauseSupported *int    `json:"causeSupported"`
		MissingData    *int    `json:"missingData"`
		Verdict        *string `json:"verdict"`
		Note           *string `json:"note"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(content)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return Answer{}, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return Answer{}, false
	}
	for _, s := range []*int{a.Specific, a.Consistent, a.CauseSupported, a.MissingData} {
		if s == nil || *s < 0 || *s > maxScore {
			return Answer{}, false
		}
	}
	if a.Verdict == nil || !verdicts[*a.Verdict] || a.Note != nil && len(*a.Note) > MaxNoteBytes {
		return Answer{}, false
	}
	out := Answer{Specific: *a.Specific, Consistent: *a.Consistent, CauseSupported: *a.CauseSupported,
		MissingData: *a.MissingData, Verdict: *a.Verdict}
	if a.Note == nil || *a.Note == "" {
		return out, true
	}
	if ok, _ := redactorClean(r, *a.Note, nil); ok && utf8.ValidString(*a.Note) {
		out.Note = *a.Note
	} else {
		out.NoteDropped = true
	}
	return out, true
}

// cutUTF8 cuts s to at most n bytes without splitting a rune.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
