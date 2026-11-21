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
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
)

// Run IDs of the attempt tests.
const (
	runA = "run-a"
	runB = "run-b"
)

// scoreAt runs the fixture's scorer as run at the time now.
func scoreAt(f *fixture, src Source, run string, now time.Time) (Score, error) {
	f.scorer.Run = run
	f.scorer.Now = func() time.Time { return now }
	return f.scorer.Score(context.Background(), src)
}

// A record has two attempts a UTC day: a third run is refused before any reservation or model call, with the
// record and the count; the next UTC day it goes through.
func TestThirdAttemptOfADayIsRefusedWithoutACall(t *testing.T) {
	rec := recordOf(t, rngOf(91, 92), claudeID)
	f := newFixture(t)
	day := time.Date(2026, 11, 21, 0, 0, 1, 0, time.UTC)
	// Two runs that fail after the judge's call (no reply scripted): two paid attempts.
	for i, run := range []string{runA, runB} {
		if _, err := scoreAt(f, sourceOf(t, rec), run, day.Add(time.Duration(i)*time.Hour)); err == nil {
			t.Fatalf("%s: the failing judge call was not an error", run)
		}
	}
	calls, reserved := f.modelCalls(), len(f.ledger.reserved)
	if calls != 2 {
		t.Fatalf("%d model calls after two attempts, want 2", calls)
	}
	_, err := scoreAt(f, sourceOf(t, rec), "run-c", day.Add(23*time.Hour+59*time.Minute))
	var ae *AttemptsError
	if !errors.As(err, &ae) || ae.Attempts != 2 || ae.Record != rec.ID || ae.Day != "20261121" || ae.Raced {
		t.Fatalf("a third attempt the same day: %v", err)
	}
	if !strings.Contains(err.Error(), rec.ID) || !strings.Contains(err.Error(), "2 attempts") {
		t.Fatalf("the refusal does not name the record and the count: %v", err)
	}
	if f.modelCalls() != calls || len(f.ledger.reserved) != reserved {
		t.Fatalf("the refused run made %d calls and %d reservations", f.modelCalls()-calls, len(f.ledger.reserved)-reserved)
	}
	// The next UTC day the record is tried again.
	f.openai.replies = append(f.openai.replies, providers.Completion{}, providers.Completion{}, answerReply(gptID))
	s, err := scoreAt(f, sourceOf(t, rec), "run-d", day.Add(24*time.Hour))
	if err != nil || s.Status != StatusScored || f.modelCalls() != calls+1 {
		t.Fatalf("the next day: %+v, %v, %d calls", s, err, f.modelCalls()-calls)
	}
	// A refused run of another record does not count: each record has its own attempts.
	other := recordOf(t, rngOf(93, 94), claudeID)
	f.openai.replies = append(f.openai.replies, answerReply(gptID))
	if s, err := scoreAt(f, sourceOf(t, other), "run-e", day); err != nil || s.Status != StatusScored {
		t.Fatalf("another record the same day: %+v, %v", s, err)
	}
}

// A run counts once, whatever its calls: a run with the cross-check reserved three calls and is one attempt.
func TestAttemptsCountRunsNotCalls(t *testing.T) {
	rs := []Reservation{
		{Day: "20261121", Record: testRecordID, Run: "a", Call: 1},
		{Day: "20261121", Record: testRecordID, Run: "a", Call: 2, Family: FamilyAnthropic},
		{Day: "20261121", Record: testRecordID, Run: "a", Call: 3},
		{Day: "20261120", Record: testRecordID, Run: "b", Call: 1},
		{Day: "20261121", Record: strings.Replace(testRecordID, "0f8e", "1f8e", 1), Run: "c", Call: 1},
		{Run: "none", Call: 1},
	}
	if n := Attempts(rs, "20261121", testRecordID); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
	if n := Attempts(rs, "20261121", ""); n != 0 {
		t.Fatalf("reservations with no record counted as %d attempts of no record", n)
	}
}

// The flag raises the limit for one run of the record it names; naming another record ends the run before
// any reservation.
func TestExtraAttemptRaisesTheLimitForTheRecordItNames(t *testing.T) {
	rec := recordOf(t, rngOf(95, 96), claudeID)
	f := newFixture(t)
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	for _, run := range []string{runA, runB} {
		if _, err := scoreAt(f, sourceOf(t, rec), run, now); err == nil {
			t.Fatal("the failing judge call was not an error")
		}
	}
	calls, reserved := f.modelCalls(), len(f.ledger.reserved)
	f.scorer.ExtraAttemptFor = recordOf(t, rngOf(195, 196), claudeID).ID
	if _, err := scoreAt(f, sourceOf(t, rec), "run-c", now); !errors.Is(err, ErrExtraAttemptOtherRecord) ||
		f.modelCalls() != calls || len(f.ledger.reserved) != reserved {
		t.Fatalf("an extra attempt for another record: %v, %d calls", err, f.modelCalls()-calls)
	}
	f.scorer.ExtraAttemptFor = "not-a-record"
	if err := f.scorer.Validate(); err == nil {
		t.Fatal("an extra attempt that names no record ID passed the configuration check")
	}
	f.scorer.ExtraAttemptFor = rec.ID
	f.openai.replies = append(f.openai.replies, providers.Completion{}, providers.Completion{}, answerReply(gptID))
	if s, err := scoreAt(f, sourceOf(t, rec), "run-c", now); err != nil || s.Status != StatusScored {
		t.Fatalf("the extra attempt: %+v, %v", s, err)
	}
	// One run only: the next run without the flag is refused with the three attempts.
	f.scorer.ExtraAttemptFor = ""
	var ae *AttemptsError
	if _, err := scoreAt(f, sourceOf(t, rec), "run-d", now); !errors.As(err, &ae) || ae.Attempts != 3 {
		t.Fatalf("the run after the extra attempt: %v", err)
	}
}

// barrierLedger holds the first reservation of every run until both runs reach it, once before the name is
// created and once after: the interleaving in which every run passed the first count before any other
// reserved, and listed again after every other reserved.
type barrierLedger struct {
	Ledger
	before, after *barrier
}

type barrier struct {
	mu      sync.Mutex
	n       int
	release chan struct{}
}

// newBarrier holds two runs: the racing tests race two.
func newBarrier() *barrier { return &barrier{n: 2, release: make(chan struct{})} }

func (b *barrier) wait(ctx context.Context) error {
	b.mu.Lock()
	b.n--
	if b.n == 0 {
		close(b.release)
	}
	b.mu.Unlock()
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *barrierLedger) Reserve(ctx context.Context, r Reservation) error {
	if r.Call != 1 {
		return b.Ledger.Reserve(ctx, r)
	}
	if err := b.before.wait(ctx); err != nil {
		return err
	}
	if err := b.Ledger.Reserve(ctx, r); err != nil {
		return err
	}
	return b.after.wait(ctx)
}

// Two runs racing for a record's second attempt cannot both make calls. In the worst interleaving, both count
// one attempt, both reserve, and both see three: both end without a call (the record loses its day).
func TestRacingRunsCannotBothTakeTheLastAttempt(t *testing.T) {
	rec := recordOf(t, rngOf(97, 98), claudeID)
	f := newFixture(t)
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	dir := DirLedger{Root: t.TempDir()}
	f.scorer.Ledger = dir
	if _, err := scoreAt(f, sourceOf(t, rec), runA, now); err == nil {
		t.Fatal("the failing judge call was not an error")
	}
	calls := f.modelCalls()
	bl := &barrierLedger{Ledger: dir, before: newBarrier(), after: newBarrier()}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, run := range []string{runB, "run-c"} {
		s := *f.scorer
		s.Ledger, s.Run, s.Now = bl, run, func() time.Time { return now }
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, errs[i] = s.Score(ctx, sourceOf(t, rec))
		})
	}
	wg.Wait()
	if got := f.modelCalls() - calls; got != 0 {
		t.Fatalf("racing runs made %d model calls, want none: %v", got, errs)
	}
	for _, err := range errs {
		var ae *AttemptsError
		if !errors.As(err, &ae) || !ae.Raced || ae.Attempts != 3 {
			t.Fatalf("a racing run: %v", err)
		}
	}
}

// Two runs of two records, racing for the last room under the judge family's monthly cap, cannot both call the
// judge. In the worst interleaving both list the month under the cap, both reserve, and both read the sum again
// with both reservations: both stop with the budget status before the call, and their reservations stay.
func TestRacingRunsCannotBothPassTheMonthlyCap(t *testing.T) {
	recs := []analysis.Record{recordOf(t, rngOf(103, 104), claudeID), recordOf(t, rngOf(105, 106), claudeID)}
	ta, tb := firstCallTokens(t, recs[0]), firstCallTokens(t, recs[1])
	f := newFixture(t)
	judge := &countingModel{}
	f.scorer.Models[FamilyOpenAI] = Model{Provider: judge, Name: gptID}
	// Room for either call alone, not for both.
	f.scorer.Limits.MaxMonthTokens = ta + tb - 1
	dir := DirLedger{Root: t.TempDir()}
	bl := &barrierLedger{Ledger: dir, before: newBarrier(), after: newBarrier()}
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	scores, errs := make([]Score, 2), make([]error, 2)
	var wg sync.WaitGroup
	for i, run := range []string{runA, runB} {
		s := *f.scorer
		s.Ledger, s.Run, s.Now = bl, run, func() time.Time { return now }
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			scores[i], errs[i] = s.Score(ctx, sourceOf(t, recs[i]))
		})
	}
	wg.Wait()
	rs, err := dir.List(context.Background(), now.Format("2006-01"), FamilyOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	if n := judge.n.Load(); n != 0 {
		t.Fatalf("racing runs made %d judge calls with %d tokens reserved under a cap of %d, want none: %v",
			n, Spent(rs), f.scorer.Limits.MaxMonthTokens, errs)
	}
	for i := range scores {
		if errs[i] != nil || scores[i].Status != StatusBudget || !strings.Contains(scores[i].Reason, "monthly token cap") {
			t.Fatalf("racing run %d: %+v, %v", i, scores[i], errs[i])
		}
	}
	if Spent(rs) != ta+tb {
		t.Fatalf("the stopped runs' reservations sum to %d, want both kept (%d)", Spent(rs), ta+tb)
	}
}

// countingModel counts calls and fails each, so every run that calls is one paid attempt.
type countingModel struct{ n atomic.Int64 }

func (c *countingModel) Complete(context.Context, providers.CompletionRequest) (providers.Completion, error) {
	c.n.Add(1)
	return providers.Completion{}, errors.New("counting model: failed on purpose")
}

// Many runs of one record at once, on the directory ledger: at most two make a model call in a day.
func TestConcurrentRunsOfARecordMakeAtMostTwoAttempts(t *testing.T) {
	rec := recordOf(t, rngOf(99, 100), claudeID)
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	for round := range 8 {
		f := newFixture(t)
		judge := &countingModel{}
		f.scorer.Models[FamilyOpenAI] = Model{Provider: judge, Name: gptID}
		f.scorer.Ledger = DirLedger{Root: t.TempDir()}
		var wg sync.WaitGroup
		for i := range 6 {
			s := *f.scorer
			s.Run, s.Now = "run-"+strconv.Itoa(i), func() time.Time { return now }
			wg.Go(func() { _, _ = s.Score(context.Background(), sourceOf(t, rec)) })
		}
		wg.Wait()
		if n := judge.n.Load(); n > MaxAttemptsPerDay {
			t.Fatalf("round %d: %d runs called the judge, want at most %d", round, n, MaxAttemptsPerDay)
		}
	}
}

// The month's sum is the sum of the token counts in the names: the record and the day in a name change no sum.
func TestMonthSumIsTheTokensOfTheNames(t *testing.T) {
	ctx := context.Background()
	rng := rngOf(101, 102)
	l := DirLedger{Root: t.TempDir()}
	var want int64
	for i := range 200 {
		tokens := int64(rng.IntN(60_000))
		if i%17 == 0 {
			tokens = maxTokens
		}
		want += tokens
		day := strconv.Itoa(20261101 + rng.IntN(30))
		if err := l.Reserve(ctx, Reservation{Month: testMonth, Day: day, Record: uidOf(rng) + "-20261120t093000z",
			Family: FamilyOpenAI, Run: "run-" + strconv.Itoa(i), Call: 1 + rng.IntN(3), Tokens: tokens}); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := l.List(ctx, testMonth, FamilyOpenAI)
	if err != nil || Spent(rs) != want || len(rs) != 200 {
		t.Fatalf("sum %d of %d names, %v; want %d", Spent(rs), len(rs), err, want)
	}
}

// A name with no day or record ("<run>.<call>.<tokens>.json") is not a reservation: List refuses the
// directory with an error that names the file, as for any other name it cannot read, and never sums less.
func TestListRefusesANameWithNoDayOrRecord(t *testing.T) {
	ctx := context.Background()
	l := DirLedger{Root: t.TempDir()}
	if err := l.Reserve(ctx, Reservation{Month: testMonth, Day: "20261121", Record: testRecordID,
		Family: FamilyOpenAI, Run: "run-a", Call: 1, Tokens: 100}); err != nil {
		t.Fatal(err)
	}
	const name = "run-b.1.5000.json"
	if err := os.WriteFile(filepath.Join(l.Root, "spend", testMonth, string(FamilyOpenAI), name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := l.List(ctx, testMonth, FamilyOpenAI)
	if err == nil || !strings.Contains(err.Error(), `"`+name+`"`) || rs != nil {
		t.Fatalf("list = %v, %v; want an error naming %s", rs, err, name)
	}
}

// keyAlphabet is what the generated record strings are made of: the ID's own characters, the separators of
// a path and a name, upper case, white space, a NUL and letters outside ASCII.
var keyAlphabet = []rune("0123456789abcdefzt-./\\:_ABCFTZ \t\x00éİ")

// No string reaches a reservation's name as its record unless it is a record ID in the shape of the name
// check; one that does reads back as the same record, and the name is one path element.
func TestOnlyACheckedRecordIDReachesAKey(t *testing.T) {
	rng := rngOf(103, 104)
	base := Reservation{Month: testMonth, Day: "20261121", Family: FamilyOpenAI, Run: testRun, Call: 1, Tokens: 10}
	l := DirLedger{Root: t.TempDir()}
	accepted, refused := 0, 0
	for range 5000 {
		id := []rune(uidOf(rng) + "-20261120t093000z")
		// One to three edits: replace, insert or delete a character.
		for range 1 + rng.IntN(3) {
			p := rng.IntN(len(id) + 1)
			c := keyAlphabet[rng.IntN(len(keyAlphabet))]
			switch op := rng.IntN(4); {
			case op == 0 && p < len(id):
				id[p] = c
			case op == 1:
				id = append(id[:p], append([]rune{c}, id[p:]...)...)
			case op == 2 && p < len(id):
				id = append(id[:p], id[p+1:]...)
			}
		}
		r := base
		r.Record = string(id)
		_, shaped := recordIDFromName(r.Record + ".json")
		shaped = shaped && blobNamePattern.MatchString(r.Record+".json")
		name, err := ReservationName(r)
		if (err == nil) != shaped {
			t.Fatalf("record %q: name %q, %v; the name check says %v", r.Record, name, err, shaped)
		}
		if err != nil {
			refused++
			if err := l.Reserve(context.Background(), r); err == nil {
				t.Fatalf("record %q was reserved", r.Record)
			}
			continue
		}
		accepted++
		if filepath.Base(name) != name || strings.ContainsAny(name, "/\\") {
			t.Fatalf("record %q: name %q is not one path element", r.Record, name)
		}
		got, ok := parseReservation(name, testMonth, FamilyOpenAI)
		if !ok || got != r {
			t.Fatalf("record %q: name %q reads back as %+v", r.Record, name, got)
		}
	}
	if accepted == 0 || refused == 0 {
		t.Fatalf("the corpus has %d accepted and %d refused records; it needs both", accepted, refused)
	}
	// Nothing refused reached the directory.
	if _, err := os.Stat(filepath.Join(l.Root, "spend")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused record left the spend directory: %v", err)
	}
}
