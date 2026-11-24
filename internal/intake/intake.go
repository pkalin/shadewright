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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// RunTimeout bounds one run: the read, every model call and the reservations together. The job's queue
// visibility timeout must be longer (ADR-0047 section 2).
const RunTimeout = 8 * time.Minute

// ScoreSchemaVersion is the version of the Score's JSON.
const ScoreSchemaVersion = 1

// Status is the outcome of a run.
type Status string

// Outcomes. Scored, Unjudged and Invalid are final scores; Quarantined is a quarantine entry and Budget a
// budget entry (ADR-0047 section 5), which a file run writes as a score with that status.
const (
	StatusScored      Status = "scored"
	StatusUnjudged    Status = "unjudged"
	StatusInvalid     Status = "invalid"
	StatusQuarantined Status = "quarantined"
	StatusBudget      Status = "budget"
)

// CrossCheck says what came of the judge's one ask.
type CrossCheck string

// Cross-check outcomes; empty when the judge did not ask.
const (
	CrossCheckAnswered CrossCheck = "answered"
	CrossCheckNotSent  CrossCheck = "notSent"
	CrossCheckBudget   CrossCheck = "budget"
)

// ModelRef names a model of a score.
type ModelRef struct {
	Family Family `json:"family"`
	Model  string `json:"model"`
}

// Usage is what the providers reported, by role. The key is not "tokens": a key named like that with a value
// is what the redactor takes for a credential.
type Usage struct {
	JudgeInput  int64 `json:"judgeInput"`
	JudgeOutput int64 `json:"judgeOutput"`
	CrossInput  int64 `json:"crossInput"`
	CrossOutput int64 `json:"crossOutput"`
}

// Score is the outcome of one run. Every text in it is fixed by code, a checked shape, or the judge's note
// after the redactor found nothing in it.
type Score struct {
	SchemaVersion int    `json:"schemaVersion"`
	Status        Status `json:"status"`
	// Check names the check that quarantined the record (CheckName, CheckSize, CheckSchema, CheckRedactor).
	Check string `json:"check,omitempty"`
	// Reason is fixed text for a status other than scored.
	Reason string `json:"reason,omitempty"`
	// Kinds are the redactor's rule kinds that matched, for CheckRedactor.
	Kinds []string `json:"kinds,omitempty"`
	// RecordID is the ID of the record's name, set once the name passed its check.
	RecordID string `json:"recordId,omitempty"`
	// NameSHA256 is the hash of a refused name, never the name.
	NameSHA256   string     `json:"nameSha256,omitempty"`
	RecordSHA256 string     `json:"recordSha256,omitempty"`
	Size         int64      `json:"size,omitempty"`
	Month        string     `json:"month,omitempty"`
	Family       Family     `json:"recordFamily,omitempty"`
	Judge        *ModelRef  `json:"judge,omitempty"`
	CrossCheck   CrossCheck `json:"crossCheck,omitempty"`
	// PromptVersion is set once a judge was called.
	PromptVersion string  `json:"promptVersion,omitempty"`
	Answer        *Answer `json:"answer,omitempty"`
	// Reserved is what the run's reservations counted, each call at its maximum.
	Reserved int64 `json:"reserved"`
	Usage    Usage `json:"usage"`
}

// Model is a model the intake may call, of one family.
type Model struct {
	Provider providers.ModelProvider
	// Name is the configured model ID, recorded when the reply names none.
	Name string
}

// Limits are the token caps in code (ADR-0047 section 6). Each call is reserved at its maximum before it is
// made; the run ends without the call when any cap would be passed.
type Limits struct {
	// MaxCallTokens bounds one call: its estimated input and its output limit.
	MaxCallTokens int64
	// MaxRunTokens bounds the run's calls together.
	MaxRunTokens int64
	// MaxMonthTokens bounds the month's reservations of each family.
	MaxMonthTokens int64
}

// Scorer scores records. Models must hold a model of each family: the judge is of the other family than the
// record's model and the cross-check is of the record's.
type Scorer struct {
	Redactor *redact.Redactor
	Models   map[Family]Model
	Ledger   Ledger
	Limits   Limits
	// Run names the run's reservations; unique per run.
	Run string
	// Now is the intake's clock, read once per run; time.Now when nil.
	Now func() time.Time
	// ExtraAttemptFor names a record ID whose run goes through whatever its attempts of the day: the person's
	// flag to raise the limit for one run by hand. Empty for every other run. A run of another record with it
	// set ends with ErrExtraAttemptOtherRecord before any reservation.
	ExtraAttemptFor string
}

// MaxAttemptsPerDay is the attempts of one record on one UTC day: the distinct runs with a reservation of
// that record and day. A run that would be the third is refused before any reservation or model call.
const MaxAttemptsPerDay = 2

// AttemptsError ends a run without a model call because the record had its attempts of the day. It is not an
// outcome: no score is written, and the record is tried again on a later day or by hand (ExtraAttemptFor).
type AttemptsError struct {
	// Record is the record ID, in its checked shape.
	Record string
	// Day is the UTC day, "20060102".
	Day string
	// Attempts is the distinct runs counted for the record and the day.
	Attempts int
	// Raced is true when the run had reserved its first call and another run of the same record reserved too;
	// the run ends without the call.
	Raced bool
}

func (e *AttemptsError) Error() string {
	if e.Raced {
		return fmt.Sprintf("the record %s has %d attempts on %s with this run, over the limit of %d; another run "+
			"reserved at the same time, so this one makes no call", e.Record, e.Attempts, e.Day, MaxAttemptsPerDay)
	}
	return fmt.Sprintf("the record %s has %d attempts on %s, the limit of %d a day", e.Record, e.Attempts, e.Day,
		MaxAttemptsPerDay)
}

// ErrExtraAttemptOtherRecord ends a run whose ExtraAttemptFor names another record than the one read.
var ErrExtraAttemptOtherRecord = errors.New("the extra attempt names another record than the one read")

// Validate checks the configuration.
func (s *Scorer) Validate() error {
	switch {
	case s.Redactor == nil:
		return errors.New("intake: a redactor is required")
	case s.Ledger == nil:
		return errors.New("intake: a spend ledger is required")
	case !runPattern.MatchString(s.Run):
		return errors.New("intake: the run name must be 1 to 64 lower-case letters, digits and hyphens")
	case s.Limits.MaxCallTokens <= 0 || s.Limits.MaxRunTokens <= 0 || s.Limits.MaxMonthTokens <= 0:
		return errors.New("intake: every token cap must be above zero")
	case s.ExtraAttemptFor != "" && !checkedRecordID(s.ExtraAttemptFor):
		return errors.New("intake: the extra attempt must name a record ID")
	}
	for _, f := range []Family{FamilyAnthropic, FamilyOpenAI} {
		if m, ok := s.Models[f]; !ok || m.Provider == nil {
			return fmt.Errorf("intake: a model of the %s family is required", f)
		}
	}
	return nil
}

// run is the state of one run.
type run struct {
	*Scorer
	score Score
	// day is the UTC day of the run's one clock read, "20060102", the same read as score.Month.
	day string
	// words are the fixed words that replace the record's names in every text built for a model (nameWords).
	words map[string]string
}

// Score scores the record of src. A returned error is a failed step for a retry (a read, a reservation, a
// model call): never a verdict, and its text holds nothing of the record. Every other outcome is the Score.
func (s *Scorer) Score(ctx context.Context, src Source) (Score, error) {
	if err := s.Validate(); err != nil {
		return Score{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now().UTC()
	r := &run{Scorer: s, day: t.Format(dayLayout),
		score: Score{SchemaVersion: ScoreSchemaVersion, Month: t.Format("2006-01")}}
	return r.do(ctx, src)
}

func (r *run) quarantine(check, reason string, kinds []string) (Score, error) {
	r.score.Status, r.score.Check, r.score.Reason, r.score.Kinds = StatusQuarantined, check, reason, kinds
	return r.score, nil
}

func (r *run) end(status Status, reason string) (Score, error) {
	r.score.Status, r.score.Reason = status, reason
	return r.score, nil
}

func (r *run) do(ctx context.Context, src Source) (Score, error) {
	// 1. Name and size, before the content is read.
	name := src.Name()
	id, ok := recordIDFromName(name)
	if !ok {
		h := sha256.Sum256([]byte(name))
		r.score.NameSHA256 = hex.EncodeToString(h[:])
		return r.quarantine(CheckName, "the record's name is not <record ID>.json with an ID the analyzer makes", nil)
	}
	r.score.RecordID = id
	if r.ExtraAttemptFor != "" && r.ExtraAttemptFor != id {
		return Score{}, ErrExtraAttemptOtherRecord
	}
	size, err := src.Size(ctx)
	if err != nil {
		return Score{}, fmt.Errorf("reading the record's size: %w", err)
	}
	r.score.Size = size
	if size > MaxRecordBytes {
		return r.quarantine(CheckSize, "the record is over the size limit; it was not read", nil)
	}
	data, err := src.Read(ctx, MaxRecordBytes)
	if err != nil {
		return Score{}, fmt.Errorf("reading the record: %w", err)
	}
	h := sha256.Sum256(data)
	r.score.RecordSHA256 = hex.EncodeToString(h[:])

	// 2. Schema, its version first.
	rec, refused := decodeRecord(data, id)
	if refused != nil {
		return r.quarantine(refused.check, refused.reason, nil)
	}

	// 3. The redactor, again, keeping only values whose shape is checked.
	keep, refused := keptValues(rec, id)
	if refused != nil {
		return r.quarantine(refused.check, refused.reason, nil)
	}
	if kinds, clean := checkRedaction(r.Redactor, data, keep); !clean {
		return r.quarantine(CheckRedactor, reasonUnsent, kinds)
	}
	if kinds, clean := checkJudgeInput(r.Redactor, rec, keep); !clean {
		return r.quarantine(CheckRedactor, reasonUnsent, kinds)
	}
	r.words = nameWords(rec, keep)

	// The judge is of the other family than the record's model.
	fam, why := recordFamily(rec)
	if why != "" {
		return r.end(StatusUnjudged, why)
	}
	r.score.Family = fam
	return r.judge(ctx, rec, fam.Other())
}

// billedAttempts is the attempts of one call the service may bill: one, because each call is reserved once at
// its maximum. The adapters are also built with one attempt (cmd/intake); a failed call is an error for the
// next run, which reserves again.
const billedAttempts = 1

// reserve counts a call at its maximum before it is made: it returns false, with the cap's name, when the call
// would pass a cap, and an error when the month's sum cannot be read or the reservation cannot be created. After
// the reservation it reads the month's sum again and returns false when the sum, its own reservation included,
// is over the monthly cap (a run that reserved at the same time); that reservation stays.
func (r *run) reserve(ctx context.Context, fam Family, call int, cr checkedRequest) (bool, string, error) {
	req := cr.req
	out := int64(req.MaxOutputTokens)
	if b, ok := r.Models[fam].Provider.(providers.OutputBounder); ok {
		out = int64(b.OutputSpend(req.MaxOutputTokens))
	}
	tokens := providers.EstimateInputTokens(req) + out
	if tokens > r.Limits.MaxCallTokens {
		return false, "the call would pass the per-call token cap", nil
	}
	if r.score.Reserved+tokens > r.Limits.MaxRunTokens {
		return false, "the call would pass the per-run token cap", nil
	}
	rs, err := r.Ledger.List(ctx, r.score.Month, fam)
	if err != nil {
		return false, "", fmt.Errorf("reading the month's spend reservations: %w", err)
	}
	if Spent(rs)+tokens > r.Limits.MaxMonthTokens {
		return false, "the call would pass the monthly token cap of the " + string(fam) + " family", nil
	}
	if err := r.Ledger.Reserve(ctx, Reservation{Month: r.score.Month, Day: r.day, Record: r.score.RecordID,
		Family: fam, Run: r.Run, Call: call, Tokens: tokens}); err != nil {
		return false, "", fmt.Errorf("reserving the spend of call %d: %w", call, err)
	}
	r.score.Reserved += tokens
	// The sum again, with this reservation in it: two runs that both read the month under the cap and reserved
	// at the same time cannot both call, because the later of their two listings sees both reservations. Both
	// may stop; the reservation of a stopped run stays and counts, so the sum errs high, never low.
	rs, err = r.Ledger.List(ctx, r.score.Month, fam)
	if err != nil {
		return false, "", fmt.Errorf("reading the month's spend reservations again: %w", err)
	}
	if Spent(rs) > r.Limits.MaxMonthTokens {
		return false, "another run reserved at the same time and the calls would pass the monthly token cap of the " +
			string(fam) + " family", nil
	}
	return true, "", nil
}

// checkAttempts counts the record's attempts of the day: the distinct runs among the reservations of both
// families' listings of the month with this record and day. Before the run's first reservation (reserved false)
// a count of MaxAttemptsPerDay or more ends the run. After it (reserved true) the count includes this run, and
// a count over MaxAttemptsPerDay ends the run before its first call: two runs that both passed the first count
// and reserved at the same time cannot both make calls, because the later of their two listings sees both
// reservations (the reservations are create-only and a listing sees every name created before it). Both may
// end, which costs the record its day. A failed listing is an error for a retry. ExtraAttemptFor naming the
// record skips the count.
func (r *run) checkAttempts(ctx context.Context, reserved bool) error {
	if r.ExtraAttemptFor == r.score.RecordID {
		return nil
	}
	var all []Reservation
	for _, fam := range []Family{FamilyAnthropic, FamilyOpenAI} {
		rs, err := r.Ledger.List(ctx, r.score.Month, fam)
		if err != nil {
			return fmt.Errorf("reading the month's spend reservations: %w", err)
		}
		all = append(all, rs...)
	}
	n := Attempts(all, r.day, r.score.RecordID)
	if (!reserved && n >= MaxAttemptsPerDay) || (reserved && n > MaxAttemptsPerDay) {
		return &AttemptsError{Record: r.score.RecordID, Day: r.day, Attempts: n, Raced: reserved}
	}
	return nil
}

// judge makes the judge's call, at most one cross-check and, after it, one more judge call with its result.
func (r *run) judge(ctx context.Context, rec analysis.Record, judgeFam Family) (Score, error) {
	judge := r.Models[judgeFam]
	r.score.Judge = &ModelRef{Family: judgeFam, Model: judge.Name}
	msgs, err := judgeMessages(rec, r.words)
	if err != nil {
		return Score{}, fmt.Errorf("building the judge's request: %w", err)
	}
	req := providers.CompletionRequest{Messages: msgs, Tools: []providers.ToolDefinition{crossCheckDefinition},
		ResponseSchema: answerSchema, MaxOutputTokens: judgeOutputTokens, MaxBilledAttempts: billedAttempts}
	first, kinds, ok := checkRequest(r.Redactor, req)
	if !ok {
		return r.quarantine(CheckRedactor, reasonUnsent, kinds)
	}
	if err := r.checkAttempts(ctx, false); err != nil {
		return Score{}, err
	}
	if ok, why, err := r.reserve(ctx, judgeFam, 1, first); err != nil || !ok {
		if err != nil {
			return Score{}, err
		}
		return r.end(StatusBudget, why)
	}
	if err := r.checkAttempts(ctx, true); err != nil {
		return Score{}, err
	}
	r.score.PromptVersion = PromptVersion
	c, err := r.send(ctx, judge, first, true)
	if err != nil || len(c.ToolCalls) == 0 {
		return r.answer(c, err)
	}

	// The judge asked for a tool. Only the first cross_check is served; every other call gets a fixed refusal.
	asked := false
	results := make([]providers.Message, 0, len(c.ToolCalls))
	for _, tc := range c.ToolCalls {
		text := refusalUnknown
		if tc.Name == crossCheckTool {
			if asked {
				text = refusalSecond
			} else {
				asked = true
				var kinds []string
				if text, kinds, err = r.crossCheck(ctx, judgeFam.Other(), tc.Arguments); err != nil {
					return Score{}, err
				}
				if kinds != nil {
					return r.quarantine(CheckRedactor, reasonCrossAnswer, kinds)
				}
			}
		}
		results = append(results, providers.Message{Role: providers.RoleTool, Content: text, ToolCallID: tc.ID})
	}
	// The judge's reply goes back as the analysis loop sends it: its tool calls and the adapter's blocks, no
	// separate text (an adapter whose blocks hold the reply's text refuses a message with both). checkRequest
	// reads the text and tool calls inside the blocks.
	req.Messages = append(append(req.Messages, providers.Message{Role: providers.RoleAssistant,
		ToolCalls: c.ToolCalls, ProviderBlocks: c.ProviderBlocks}), results...)
	second, kinds, ok := checkRequest(r.Redactor, req)
	if !ok {
		return r.quarantine(CheckRedactor, reasonSecondUnsent, kinds)
	}
	if ok, why, err := r.reserve(ctx, judgeFam, 3, second); err != nil || !ok {
		if err != nil {
			return Score{}, err
		}
		return r.end(StatusBudget, why)
	}
	c, err = r.send(ctx, judge, second, true)
	if err == nil && len(c.ToolCalls) > 0 {
		return r.end(StatusInvalid, "the judge asked for a tool again after its one turn of tool results")
	}
	return r.answer(c, err)
}

// crossCheck serves the judge's one ask and returns the tool result. The question is checked before anything
// is sent; one that fails is not sent and still counts as the ask. The answer is checked as it came, with
// nothing kept: when the redactor would change it, crossCheck returns the rule kinds that matched and no
// result, and the record is quarantined; nothing is redacted and handed on.
func (r *run) crossCheck(ctx context.Context, fam Family, args []byte) (string, []string, error) {
	q, ok := question(r.Redactor, args)
	if !ok {
		r.score.CrossCheck = CrossCheckNotSent
		return refusalQuestion, nil, nil
	}
	req, _, ok := checkRequest(r.Redactor, providers.CompletionRequest{
		Messages:        crossCheckMessages(redact.ReplaceWhole(q, r.words)),
		MaxOutputTokens: crossOutputTokens, MaxBilledAttempts: billedAttempts})
	if !ok {
		r.score.CrossCheck = CrossCheckNotSent
		return refusalQuestion, nil, nil
	}
	ok, _, err := r.reserve(ctx, fam, 2, req)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		r.score.CrossCheck = CrossCheckBudget
		return refusalBudget, nil, nil
	}
	c, err := r.send(ctx, r.Models[fam], req, false)
	if err != nil {
		return "", nil, fmt.Errorf("the cross-check call failed: %w", err)
	}
	r.score.CrossCheck = CrossCheckAnswered
	if clean, kinds := redactorClean(r.Redactor, c.Content, nil); !clean {
		return "", kinds, nil
	}
	return crossCheckResult(c.Content), nil, nil
}

// send is the only call of a model: it takes a request whose every message passed checkRequest as it is sent,
// makes the call and counts the usage it reports.
func (r *run) send(ctx context.Context, m Model, cr checkedRequest, judge bool) (providers.Completion, error) {
	c, err := m.Provider.Complete(ctx, cr.req)
	if judge {
		r.score.Usage.JudgeInput += c.Usage.InputTokens
		r.score.Usage.JudgeOutput += c.Usage.OutputTokens
		if c.Model != "" {
			if f, ok := modelFamily(c.Model); ok && f == r.score.Judge.Family {
				r.score.Judge.Model = c.Model
			}
		}
	} else {
		r.score.Usage.CrossInput += c.Usage.InputTokens
		r.score.Usage.CrossOutput += c.Usage.OutputTokens
	}
	return c, err
}

// answer turns the judge's last reply into the score: a reply that breaks the contract or the schema is
// invalid and not retried; any other failure is an error for a retry.
func (r *run) answer(c providers.Completion, err error) (Score, error) {
	if providers.IsMalformedOutput(err) {
		return r.end(StatusInvalid, "the judge's reply broke the request's contract")
	}
	if err != nil {
		return Score{}, fmt.Errorf("the judge call failed: %w", err)
	}
	a, ok := parseAnswer(r.Redactor, c.Content)
	if !ok {
		return r.end(StatusInvalid, "the judge's answer does not match the answer schema")
	}
	r.score.Answer = &a
	r.score.Status = StatusScored
	return r.score, nil
}
