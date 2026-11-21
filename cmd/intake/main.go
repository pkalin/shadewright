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

// Command intake is the scoring intake of ADR-0047: it scores one analysis record from a file and writes a
// score JSON. It checks the record's name, size and schema, runs the redactor over it again and quarantines it
// on any hit before any model call, then asks one judge of the other model family than the record's, with at
// most one cross-check, under token caps counted in a local spend directory. It never runs inside a cluster
// and holds no cluster or Git credential.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pkalin/shadewright/internal/intake"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/anthropic"
	"github.com/pkalin/shadewright/internal/providers/model/openaicompatible"
	"github.com/pkalin/shadewright/internal/redact"
)

// Exit codes: 0 an outcome was written (scored, unjudged, invalid, quarantined or budget), 1 a failed step to
// retry, 2 a usage or configuration error, 3 the record had its attempts of the UTC day and nothing was written
// or called (try it on a later day, or once more with --extra-attempt).
const (
	exitRetry    = 1
	exitConfig   = 2
	exitAttempts = 3
)

// Defaults of the token caps (ADR-0047 section 6). They are below the consoles' monthly limits, so the code
// stops first.
const (
	defaultMaxCallTokens  = 40_000
	defaultMaxRunTokens   = 100_000
	defaultMaxMonthTokens = 4_000_000
	// callTimeout bounds one model call (one attempt); three fit in intake.RunTimeout.
	callTimeout = 2 * time.Minute
)

type options struct {
	record, out, spendDir       string
	anthropicModel, anthropicKF string
	openaiModel, openaiKF       string
	openaiEndpoint              string
	maxCall, maxRun, maxMonth   int64
	dryRun                      bool
	extraAttempt                string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	fl := flag.NewFlagSet("intake", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var o options
	fl.StringVar(&o.record, "record", "",
		"Path of the record file, named <record ID>.json as the analyzer writes it. Required.")
	fl.StringVar(&o.out, "out", "-", "Path of the score JSON, created only if absent (a score that exists ends the run "+
		"without a model call), or - for standard output.")
	fl.StringVar(&o.spendDir, "spend-dir", "", "Directory of the spend reservations, kept across runs: "+
		"spend/<month>/<family>/<day>.<record ID>.<run>.<call>.<tokens>.json. Required.")
	fl.StringVar(&o.anthropicModel, "anthropic-model", "", "Model ID of the Anthropic family (judge of OpenAI records, "+
		"cross-check of Anthropic ones).")
	fl.StringVar(&o.anthropicKF, "anthropic-key-file", "",
		"File holding the Anthropic API key of whoever runs the command, read at each call.")
	fl.StringVar(&o.openaiModel, "openai-model", "", "Model ID of the OpenAI family (judge of Anthropic records, "+
		"cross-check of OpenAI ones).")
	fl.StringVar(&o.openaiKF, "openai-key-file", "", "File holding the OpenAI API key of whoever runs the "+
		"command, read at each call.")
	fl.StringVar(&o.openaiEndpoint, "openai-endpoint", "https://api.openai.com/v1", "HTTPS API base of the OpenAI family.")
	fl.Int64Var(&o.maxCall, "max-call-tokens", defaultMaxCallTokens,
		"Token cap of one call: estimated input and output limit.")
	fl.Int64Var(&o.maxRun, "max-run-tokens", defaultMaxRunTokens,
		"Token cap of one run, every call reserved at its maximum.")
	fl.Int64Var(&o.maxMonth, "max-month-tokens", defaultMaxMonthTokens, "Token cap of a month for each family, summed "+
		"from the spend directory.")
	fl.BoolVar(&o.dryRun, "dry-run", false, "Use a built-in fake model of each family that answers a fixed "+
		"score: no key, no network. Reservations are still counted.")
	fl.StringVar(&o.extraAttempt, "extra-attempt", "", "Record ID of the record this run scores even when it had "+
		"its attempts of the UTC day; for one run by hand. The run ends with exit code 2 when the record is another.")
	if err := fl.Parse(args); err != nil {
		return exitConfig
	}
	red, err := redact.New()
	if err != nil {
		log.Error("Failed to build the redactor", "error", err.Error())
		return exitConfig
	}
	if o.record == "" || o.spendDir == "" || fl.NArg() > 0 {
		log.Error("The flags --record and --spend-dir are required, and no argument is taken")
		return exitConfig
	}
	models, err := buildModels(o)
	if err != nil {
		log.Error("Failed to configure the models", "error", red.String(err.Error()))
		return exitConfig
	}
	runID, err := newRunID()
	if err != nil {
		log.Error("Failed to make a run ID", "error", err.Error())
		return exitRetry
	}
	scorer := &intake.Scorer{Redactor: red, Models: models, Ledger: intake.DirLedger{Root: o.spendDir},
		Limits: intake.Limits{MaxCallTokens: o.maxCall, MaxRunTokens: o.maxRun, MaxMonthTokens: o.maxMonth}, Run: runID,
		ExtraAttemptFor: o.extraAttempt}
	if err := scorer.Validate(); err != nil {
		log.Error("The configuration is invalid", "error", err.Error())
		return exitConfig
	}
	if o.out != "-" {
		if _, err := os.Lstat(o.out); err == nil {
			log.Info("Skipped the record: its score exists already", "out", o.out)
			return 0
		} else if !errors.Is(err, fs.ErrNotExist) {
			log.Error("Failed to check for an existing score", "error", red.String(err.Error()))
			return exitRetry
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	score, err := scorer.Score(ctx, &intake.FileSource{Path: o.record})
	var attempts *intake.AttemptsError
	switch {
	case errors.As(err, &attempts):
		log.Warn("Refused the record: it had its attempts of the UTC day; try a later day or --extra-attempt",
			"run", runID, "record", attempts.Record, "day", attempts.Day, "attempts", attempts.Attempts,
			"limit", intake.MaxAttemptsPerDay, "raced", attempts.Raced)
		return exitAttempts
	case errors.Is(err, intake.ErrExtraAttemptOtherRecord):
		log.Error("The flag --extra-attempt names another record than --record", "run", runID)
		return exitConfig
	}
	if err != nil {
		// The intake's errors hold no record text; the redactor runs over them all the same.
		log.Error("Failed to score the record; try again", "run", runID, "error", red.String(err.Error()))
		return exitRetry
	}
	data, err := json.MarshalIndent(score, "", "  ")
	if err != nil {
		log.Error("Failed to encode the score", "error", err.Error())
		return exitRetry
	}
	data = append(data, '\n')
	if err := writeOut(o.out, data, stdout); err != nil {
		log.Error("Failed to write the score", "error", red.String(err.Error()))
		return exitRetry
	}
	if score.Status == intake.StatusBudget {
		// ADR-0047 section 6: a cap reached is visible on every run that meets it, with the family and the month.
		fam := ""
		if score.Judge != nil {
			fam = string(score.Judge.Family)
		}
		log.Warn("Reached a token cap; the record was not scored", "run", runID, "record", score.RecordID,
			"family", fam, "month", score.Month, "reason", score.Reason)
	}
	log.Info("Scored the record", "run", runID, "record", score.RecordID, "status", string(score.Status),
		"check", score.Check, "reserved", score.Reserved)
	return 0
}

// buildModels returns a model of each family: the adapters, or the fakes of --dry-run.
func buildModels(o options) (map[intake.Family]intake.Model, error) {
	if o.dryRun {
		return map[intake.Family]intake.Model{
			intake.FamilyAnthropic: {Provider: fakeJudge{model: "claude-dry-run"}, Name: "claude-dry-run"},
			intake.FamilyOpenAI:    {Provider: fakeJudge{model: "gpt-dry-run"}, Name: "gpt-dry-run"},
		}, nil
	}
	if o.anthropicModel == "" || o.anthropicKF == "" || o.openaiModel == "" || o.openaiKF == "" {
		return nil, errors.New("both families need a model and a key file (or --dry-run)")
	}
	ac, oc := adapterConfigs(o)
	a, err := anthropic.New(ac)
	if err != nil {
		return nil, err
	}
	oa, err := openaicompatible.New(oc)
	if err != nil {
		return nil, err
	}
	return map[intake.Family]intake.Model{
		intake.FamilyAnthropic: {Provider: a, Name: o.anthropicModel},
		intake.FamilyOpenAI:    {Provider: oa, Name: o.openaiModel},
	}, nil
}

// adapterConfigs returns the configuration of each family's adapter. Each makes one attempt per call: the
// intake reserves every call once at its maximum (ADR-0047 section 6), and an automatic retry would be a second
// billed attempt under that one reservation. A failed call ends the run with exit code 1 for the next run.
func adapterConfigs(o options) (anthropic.Config, openaicompatible.Config) {
	return anthropic.Config{Model: o.anthropicModel, KeyFile: o.anthropicKF, OtherKeyFiles: []string{o.openaiKF},
			Timeout: callTimeout, MaxAttempts: 1},
		openaicompatible.Config{Endpoint: o.openaiEndpoint, Deployment: o.openaiModel, KeyFile: o.openaiKF,
			OtherKeyFiles: []string{o.anthropicKF}, Timeout: callTimeout, MaxAttempts: 1}
}

// writeOut writes the score to standard output or creates the file; a file that exists is never replaced.
func writeOut(path string, data []byte, stdout io.Writer) error {
	if path == "-" {
		_, err := stdout.Write(data)
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- the operator's --out flag
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// newRunID returns a random run ID for the reservation names.
func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "local-" + hex.EncodeToString(b), nil
}

// fakeJudge answers every call with a fixed valid score, for --dry-run.
type fakeJudge struct{ model string }

func (f fakeJudge) Complete(context.Context, providers.CompletionRequest) (providers.Completion, error) {
	return providers.Completion{Model: f.model, Content: fmt.Sprintf(
		`{"specific":%d,"consistent":%d,"causeSupported":%d,"missingData":%d,"verdict":"partial","note":"dry run"}`,
		3, 3, 3, 3)}, nil
}
