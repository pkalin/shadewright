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
	"flag"
	"io"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/controller"
)

// TestExecutorFlagsConfigureTheReconciler: --reviewer-hints and --verify-outcomes are off unless given, and the
// flags reach the ReviewerHints, PullRequestPollInterval, NodeGrace and VerifyOutcomes of the reconciler
// newReconciler builds, the one main runs.
func TestExecutorFlagsConfigureTheReconciler(t *testing.T) {
	cases := map[string]struct {
		args      []string
		wantHints bool
		wantPoll  time.Duration
		wantGrace time.Duration
		wantOut   bool
	}{
		"defaults": {nil, false, controller.DefaultPullRequestPollInterval, time.Minute, false},
		"hints on": {[]string{"--reviewer-hints"}, true, controller.DefaultPullRequestPollInterval, time.Minute, false},
		"hints off": {[]string{"--reviewer-hints=false"}, false, controller.DefaultPullRequestPollInterval,
			time.Minute, false},
		"poll interval only": {[]string{"--pull-request-poll-interval=2m"}, false, 2 * time.Minute, time.Minute, false},
		"node grace": {[]string{"--node-grace=90s"}, false, controller.DefaultPullRequestPollInterval,
			90 * time.Second, false},
		"outcomes on": {[]string{"--verify-outcomes"}, false, controller.DefaultPullRequestPollInterval,
			time.Minute, true},
		"outcomes off": {[]string{"--verify-outcomes=false"}, false, controller.DefaultPullRequestPollInterval,
			time.Minute, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fs := flag.NewFlagSet("executor", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			ef := bindExecutorFlags(fs)
			if err := fs.Parse(c.args); err != nil {
				t.Fatalf("parse %v: %v", c.args, err)
			}
			r := ef.newReconciler(nil, nil, "shadewright-system", nil, nil, nil)
			if r.Namespace != "shadewright-system" {
				t.Errorf("Namespace = %q, want shadewright-system", r.Namespace)
			}
			if r.ReviewerHints != c.wantHints {
				t.Errorf("ReviewerHints = %v, want %v", r.ReviewerHints, c.wantHints)
			}
			if r.PullRequestPollInterval != c.wantPoll {
				t.Errorf("PullRequestPollInterval = %v, want %v", r.PullRequestPollInterval, c.wantPoll)
			}
			if r.NodeGrace != c.wantGrace {
				t.Errorf("NodeGrace = %v, want %v", r.NodeGrace, c.wantGrace)
			}
			if r.VerifyOutcomes != c.wantOut {
				t.Errorf("VerifyOutcomes = %v, want %v", r.VerifyOutcomes, c.wantOut)
			}
		})
	}
}
