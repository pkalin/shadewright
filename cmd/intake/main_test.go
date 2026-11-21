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
	missing := filepath.Join(dir, "0a-20261120t090000z.json")
	if code := run([]string{recordFlag, missing, spendFlag, dir, dryRun}, &stdout, &stderr); code != exitRetry {
		t.Fatalf("a missing record: exit %d, want %d", code, exitRetry)
	}
}
