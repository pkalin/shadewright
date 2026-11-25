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

package redactadapter

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr/funcr"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

const memoryLimitChild = "SHADEWRIGHT_REDACTION_MEMORY_LIMIT_CHILD"

func runWithMemoryLimit(t *testing.T, testName, limit string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+testName+"$") // #nosec G204 -- reruns this test binary
	cmd.Env = append(os.Environ(), "GOMEMLIMIT="+limit, memoryLimitChild+"="+limit)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, out)
	}
}

func TestFallbackHeapLog(t *testing.T) {
	setting := os.Getenv(memoryLimitChild)
	if setting == "" {
		for _, limit := range []string{"off", "128MiB"} {
			t.Run(limit, func(t *testing.T) { runWithMemoryLimit(t, "TestFallbackHeapLog", limit) })
		}
		return
	}

	var lines []string
	logf.SetLogger(funcr.New(func(prefix, args string) {
		if prefix == "redact" {
			lines = append(lines, args)
		}
	}, funcr.Options{}))
	plain, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, redactText := range []func(string) string{plain.String, redact.String, plain.String} {
		if got := redactText("ordinary text"); got != "ordinary text" {
			t.Errorf("redaction returned %q", got)
		}
	}
	if setting == "off" {
		const message = "no Go memory limit (GOMEMLIMIT) is set: the redactions running at once share a fixed heap limit"
		if len(lines) != 1 || !strings.Contains(lines[0], message) || !strings.Contains(lines[0], `"limitBytes"=67108864`) {
			t.Errorf("want one fallback log for 64 MiB, got %q", lines)
		}
	} else if len(lines) != 0 {
		t.Errorf("fallback log with a Go memory limit: %q", lines)
	}
}

func TestDecisionCounterDeltas(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		run    func() bool
		reason string
	}{
		{"work", func() bool {
			_, ok := r.WithWorkBudget(0).StringChecked("ordinary text")
			return ok
		}, redact.ReasonWork},
		{"heap", func() bool {
			_, ok := r.PiecesWithin(make([]redact.Piece, redact.FreePieces+1), -1)
			return ok
		}, redact.ReasonHeap},
		{"size", func() bool {
			return r.StringShort(strings.Repeat("a", redact.MaxShortEntry+1)) != redact.Withheld
		}, redact.ReasonSize},
	} {
		t.Run(c.name, func(t *testing.T) {
			counter := withheldTotal.WithLabelValues(c.reason)
			before := promtest.ToFloat64(counter)
			waits := promtest.ToFloat64(heapWaitsTotal)
			if c.run() {
				t.Error("entry was released")
			}
			if d := promtest.ToFloat64(counter) - before; d != 1 {
				t.Errorf("withheld counter increased by %v, want 1", d)
			}
			if d := promtest.ToFloat64(heapWaitsTotal) - waits; d != 0 {
				t.Errorf("wait counter increased by %v, want 0", d)
			}
		})
	}
}

func TestWaitCounterDeltas(t *testing.T) {
	if os.Getenv(memoryLimitChild) == "" {
		runWithMemoryLimit(t, t.Name(), "64MiB")
		return
	}

	for _, c := range []struct {
		name        string
		cancel      bool
		wantRefused float64
	}{
		{"successful", false, 0},
		{"cancelled", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, err := New()
				if err != nil {
					t.Fatal(err)
				}
				// A full-weight entry holds the limiter while the second entry tries to acquire it.
				held := make(chan struct{})
				release := make(chan struct{})
				done := make(chan struct{})
				go func() {
					defer close(done)
					ps := []redact.Piece{{Text: strings.Repeat("a", 320<<10)}}
					r.PiecesFitting(ps, func([]string) bool {
						close(held)
						<-release
						return true
					})
				}()
				defer func() { close(release); <-done }()
				select {
				case <-held:
				case <-done:
					t.Fatal("the first entry did not acquire its share")
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				refused := withheldTotal.WithLabelValues(redact.ReasonWait)
				before := promtest.ToFloat64(refused)
				waits := promtest.ToFloat64(heapWaitsTotal)
				result := make(chan bool, 1)
				go func() {
					_, ok := r.WithContext(ctx).StringChecked("ordinary text")
					result <- ok
				}()
				synctest.Wait()
				if c.cancel {
					cancel()
				} else {
					release <- struct{}{}
				}
				if ok := <-result; ok == c.cancel {
					t.Errorf("decided %v, want %v", ok, !c.cancel)
				}
				if d := promtest.ToFloat64(refused) - before; d != c.wantRefused {
					t.Errorf("wait refusals increased by %v, want %v", d, c.wantRefused)
				}
				if d := promtest.ToFloat64(heapWaitsTotal) - waits; d != 1 {
					t.Errorf("wait counter increased by %v, want 1", d)
				}
			})
		})
	}
}
