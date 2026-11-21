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

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/intake"
)

// dryRun is the flag of the built-in fake models.
const (
	dryRun     = "--dry-run"
	recordFlag = "--record"
	spendFlag  = "--spend-dir"
)

var fakeToken = "ghp_" + strings.Repeat("A1b2", 9)

// writeRecord writes a hand-built record, named as the analyzer names it, and returns its path.
func writeRecord(t *testing.T, dir, summary string) string {
	t.Helper()
	fp := "sha256:" + strings.Repeat("ab12", 16)
	uid := "0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5b"
	started := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	rec := analysis.Record{
		SchemaVersion: analysis.RecordSchemaVersion, ID: uid + "-" + started.Format("20060102t150405z"),
		Incident: analysis.RecordIncident{Name: remediationv1alpha1.IncidentName(fp, 1), UID: uid, Fingerprint: fp,
			Reason: "CrashLoopBackOff", SubjectKind: "Deployment", SubjectName: "api"},
		StartedAt: started, FinishedAt: started.Add(time.Minute), Reason: "NoAction", Calls: 2,
		Models: []string{"claude-opus-5-5"}, PromptVersion: "analysis-12", Summary: summary,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, rec.ID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDryRunScoresARecord(t *testing.T) {
	dir := t.TempDir()
	rec := writeRecord(t, dir, "The probe path /healthz returns 404 since the last rollout.")
	out := filepath.Join(dir, "score.json")
	var stdout, stderr bytes.Buffer
	args := []string{recordFlag, rec, spendFlag, filepath.Join(dir, "spend"), "--out", out, dryRun}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var s intake.Score
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Status != intake.StatusScored || s.Judge == nil || s.Judge.Family != intake.FamilyOpenAI || s.Reserved <= 0 {
		t.Fatalf("score = %s", data)
	}
	// A second run finds the score and calls nothing.
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "exists already") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestQuarantinedRecordLeavesNoTextInTheOutput(t *testing.T) {
	dir := t.TempDir()
	rec := writeRecord(t, dir, "Set token="+fakeToken+" to fix it.")
	var stdout, stderr bytes.Buffer
	code := run([]string{recordFlag, rec, spendFlag, filepath.Join(dir, "spend"), dryRun}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"quarantined"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), fakeToken) {
		t.Fatal("the record's secret reached the output or the log")
	}
	if _, err := os.Stat(filepath.Join(dir, "spend")); err == nil {
		t.Fatal("a quarantined record made a reservation")
	}
}

func TestFlagErrors(t *testing.T) {
	dir := t.TempDir()
	rec := writeRecord(t, dir, "A summary.")
	for name, args := range map[string][]string{
		"no record":     {spendFlag, dir, dryRun},
		"no spend dir":  {recordFlag, rec, dryRun},
		"no models":     {recordFlag, rec, spendFlag, dir},
		"a zero cap":    {recordFlag, rec, spendFlag, dir, dryRun, "--max-run-tokens", "0"},
		"an extra arg":  {recordFlag, rec, spendFlag, dir, dryRun, "x"},
		"unknown flags": {"--bogus"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != exitConfig {
				t.Fatalf("exit %d, want %d: %s", code, exitConfig, stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(dir, "0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5c-20261120t090000z.json")
	if code := run([]string{recordFlag, missing, spendFlag, dir, dryRun}, &stdout, &stderr); code != exitRetry {
		t.Fatalf("a missing record: exit %d, want %d", code, exitRetry)
	}
}

// Finding (d) of 2026-11-21: the intake reserves each call once at its maximum, so its adapters must not retry
// a call by themselves (a 200 whose body read fails was sent again under the same reservation).
func TestAdaptersMakeOneAttempt(t *testing.T) {
	dir := t.TempDir()
	o := options{anthropicModel: "claude-opus-5-5", anthropicKF: filepath.Join(dir, "a"), openaiModel: "gpt-5.1",
		openaiKF: filepath.Join(dir, "o"), openaiEndpoint: "https://api.openai.com/v1"}
	a, oa := adapterConfigs(o)
	if a.MaxAttempts != 1 || oa.MaxAttempts != 1 {
		t.Fatalf("attempts: anthropic %d, openai %d; want 1 each", a.MaxAttempts, oa.MaxAttempts)
	}
	if _, err := buildModels(o); err != nil {
		t.Fatalf("the adapters refuse the configuration: %v", err)
	}
}

// A budget outcome logs a warning with the family and the month on every run that meets the cap.
func TestBudgetOutcomeLogsAWarning(t *testing.T) {
	dir := t.TempDir()
	rec := writeRecord(t, dir, "The probe path /healthz returns 404 since the last rollout.")
	var stdout, stderr bytes.Buffer
	args := []string{recordFlag, rec, spendFlag, filepath.Join(dir, "spend"), "--max-month-tokens", "10", dryRun}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var s intake.Score
	if err := json.Unmarshal(stdout.Bytes(), &s); err != nil || s.Status != intake.StatusBudget || s.Month == "" {
		t.Fatalf("stdout = %s (%v)", stdout.String(), err)
	}
	var warned bool
	for line := range strings.SplitSeq(strings.TrimSpace(stderr.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if entry["level"] == "WARN" && entry["family"] == string(intake.FamilyOpenAI) &&
			entry["month"] == s.Month {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning with the family and the month: %s", stderr.String())
	}
}

// A record with two attempts on the UTC day is refused with exit code 3, a line naming the record and the
// count, no score and no reservation; --extra-attempt naming it lets one run through, naming another record
// is a configuration error.
func TestThirdAttemptExitsWithItsOwnCode(t *testing.T) {
	dir := t.TempDir()
	rec := writeRecord(t, dir, "The probe path /healthz returns 404 since the last rollout.")
	id := strings.TrimSuffix(filepath.Base(rec), ".json")
	now := time.Now().UTC()
	spend := filepath.Join(dir, "spend")
	fam := filepath.Join(spend, "spend", now.Format("2006-01"), string(intake.FamilyOpenAI))
	if err := os.MkdirAll(fam, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"local-a", "local-b"} {
		name := now.Format("20060102") + "." + id + "." + run + ".1.100.json"
		if err := os.WriteFile(filepath.Join(fam, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "score.json")
	args := []string{recordFlag, rec, spendFlag, spend, "--out", out, dryRun}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitAttempts {
		t.Fatalf("exit %d, want %d: %s", code, exitAttempts, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"record":"`+id+`"`) || !strings.Contains(stderr.String(), `"attempts":2`) {
		t.Fatalf("the line does not name the record and the count: %s", stderr.String())
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused run wrote a score")
	}
	if entries, err := os.ReadDir(fam); err != nil || len(entries) != 2 {
		t.Fatalf("a refused run reserved: %d names, %v", len(entries), err)
	}
	other := strings.Replace(id, id[:8], "00000000", 1)
	stderr.Reset()
	if code := run(append(args, "--extra-attempt", other), &stdout, &stderr); code != exitConfig {
		t.Fatalf("an extra attempt for another record: exit %d: %s", code, stderr.String())
	}
	stderr.Reset()
	if code := run(append(args, "--extra-attempt", id), &stdout, &stderr); code != 0 {
		t.Fatalf("the extra attempt: exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the extra attempt wrote no score: %v", err)
	}
}

// A score whose write fails leaves no file at --out, so the next run scores the record instead of skipping it;
// an existing file is never replaced.
func TestWriteOutLeavesNoPartialScore(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "score.json")
	saved := writeScore
	t.Cleanup(func() { writeScore = saved })
	writeScore = func(f *os.File, data []byte) error {
		_, _ = f.Write(data[:len(data)/2])
		return errors.New("disk full")
	}
	if err := writeOut(out, []byte(`{"status":"scored"}`), nil); err == nil {
		t.Fatal("a failed write was reported as written")
	}
	if _, err := os.Lstat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after a failed write the score path: %v", err)
	}
	writeScore = saved
	if err := writeOut(out, []byte("first\n"), nil); err != nil {
		t.Fatal(err)
	}
	if err := writeOut(out, []byte("second\n"), nil); err == nil {
		t.Fatal("an existing score was replaced")
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != "first\n" {
		t.Fatalf("score = %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the directory holds %v, %v; want the score alone", entries, err)
	}
}
