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
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// fakeClock starts at testNow and moves by the requested duration on each After, which fires at once, so the
// refreshes run back to back. After the refreshes-th refresh it cancels the watch (as Ctrl-C does) and returns
// a channel that never fires, so the watch must notice the cancellation to end.
type fakeClock struct {
	now       time.Time
	refreshes int
	waits     []time.Duration
	cancel    context.CancelFunc
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	if len(c.waits) >= c.refreshes {
		c.cancel()
		return nil
	}
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// watchClient is a fake client with an Incident, the settings and the budget, and funcs around its calls.
func watchClient(t *testing.T, funcs interceptor.Funcs) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(testSettings(), testIncident("incident-a", "a", testSubject, "OOM"), budgetMap("2026-11-06", "4")).
		WithInterceptorFuncs(funcs).Build()
}

// runWatch runs watchStatus for refreshes refreshes and returns stdout, stderr and the clock.
func runWatch(t *testing.T, c client.Reader, w watchOptions, refreshes int, warning <-chan string) (string, string,
	*fakeClock) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clk := &fakeClock{now: testNow, refreshes: refreshes, cancel: cancel}
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- watchStatus(ctx, c, Options{Namespace: testNS}, w, clk, warning, &stdout, &stderr)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchStatus: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watchStatus did not end after the cancellation")
	}
	return stdout.String(), stderr.String(), clk
}

// TestWatchTextTwoRefreshes: two refreshes with a fake clock redraw the screen twice, each with its own time,
// and wait the interval between them; the cancellation then ends the watch without an error.
func TestWatchTextTwoRefreshes(t *testing.T) {
	w := watchOptions{interval: 5 * time.Second, timeout: 30 * time.Second, output: outputText, redraw: true}
	out, errOut, clk := runWatch(t, watchClient(t, interceptor.Funcs{}), w, 2, nil)
	if n := strings.Count(out, clearScreen); n != 2 {
		t.Errorf("screen cleared %d times, want 2:\n%q", n, out)
	}
	for _, stamp := range []string{"2026-11-06T12:00:00Z", "2026-11-06T12:00:05Z"} {
		if !strings.Contains(out, "Every 5s, last read "+stamp) {
			t.Errorf("no refresh at %s:\n%s", stamp, out)
		}
	}
	if n := strings.Count(out, "Incident incident-a"); n != 2 {
		t.Errorf("Incident shown %d times, want 2", n)
	}
	if len(clk.waits) != 2 || clk.waits[0] != 5*time.Second {
		t.Errorf("waits = %v, want two of 5s", clk.waits)
	}
	if errOut != "" {
		t.Errorf("stderr = %q", errOut)
	}
}

// TestWatchTextNotTerminal: without a terminal, nothing clears the screen and the refreshes follow each other
// after a blank line.
func TestWatchTextNotTerminal(t *testing.T) {
	w := watchOptions{interval: 5 * time.Second, output: outputText}
	out, _, _ := runWatch(t, watchClient(t, interceptor.Funcs{}), w, 2, nil)
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape sequence in redirected output:\n%q", out)
	}
	if !strings.HasPrefix(out, "Every 5s") || !strings.Contains(out, "\n\nEvery 5s, last read 2026-11-06T12:00:05Z") {
		t.Errorf("refreshes not separated by a blank line:\n%s", out)
	}
}

// TestWatchJSONLines: with -o json each refresh is one line holding one JSON document.
func TestWatchJSONLines(t *testing.T) {
	w := watchOptions{interval: 2 * time.Second, output: outputJSON}
	out, _, _ := runWatch(t, watchClient(t, interceptor.Funcs{}), w, 3, nil)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3:\n%s", len(lines), out)
	}
	for i, l := range lines {
		var r Report
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("line %d is not one JSON document: %v", i, err)
		}
		if len(r.Namespaces) != 1 || len(r.Namespaces[0].Incidents) != 1 {
			t.Errorf("line %d: %+v", i, r)
		}
	}
}

// TestWatchFailedRefreshKeepsGoing: a refresh whose list fails prints its error on stderr, cleaned and with its
// time, and the next refresh runs; stdout holds only the good refreshes, so the JSON stays parseable.
func TestWatchFailedRefreshKeepsGoing(t *testing.T) {
	calls := 0
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := l.(*remediationv1alpha1.IncidentList); ok {
			calls++
			if calls == 1 {
				return apierrors.NewServiceUnavailable("etcd \x1b]0;pwned\x07down")
			}
		}
		return c.List(ctx, l, opts...)
	}}
	for _, output := range []string{outputJSON, outputText} {
		calls = 0
		w := watchOptions{interval: time.Second, output: output}
		out, errOut, _ := runWatch(t, watchClient(t, funcs), w, 2, nil)
		if !strings.HasPrefix(errOut, "Error at 2026-11-06T12:00:00Z: listing Incidents: etcd down") ||
			strings.Contains(errOut, "pwned") || strings.Count(errOut, "\n") != 1 {
			t.Errorf("%s: stderr = %q", output, errOut)
		}
		if n := strings.Count(out, "incident-a"); n == 0 || calls != 2 {
			t.Errorf("%s: the refresh after the error did not run (calls %d):\n%s", output, calls, out)
		}
		if output == outputJSON && strings.Count(out, "\n") != 1 {
			t.Errorf("json: %d lines, want 1 (the good refresh)", strings.Count(out, "\n"))
		}
	}
}

// TestWatchEachRefreshHasItsTimeout: each refresh gets the --request-timeout from its own start, not one
// limit for the whole watch, and a refresh that runs out of time is an error like any other.
func TestWatchEachRefreshHasItsTimeout(t *testing.T) {
	var deadlines []time.Duration
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := l.(*remediationv1alpha1.IncidentList); ok {
			d, ok := ctx.Deadline()
			if !ok {
				t.Error("a refresh has no deadline")
			}
			deadlines = append(deadlines, time.Until(d))
			if len(deadlines) == 1 {
				return context.DeadlineExceeded
			}
		}
		return c.List(ctx, l, opts...)
	}}
	w := watchOptions{interval: time.Second, timeout: time.Minute, output: outputJSON}
	out, errOut, _ := runWatch(t, watchClient(t, funcs), w, 2, nil)
	if len(deadlines) != 2 {
		t.Fatalf("%d refreshes, want 2", len(deadlines))
	}
	for _, d := range deadlines {
		if d <= 50*time.Second || d > time.Minute {
			t.Errorf("deadline in %v, want about 1m", d)
		}
	}
	if !strings.Contains(errOut, "context deadline exceeded") || strings.Count(out, "\n") != 1 {
		t.Errorf("stdout %q, stderr %q", out, errOut)
	}
}

// TestWatchCancelDuringRefresh: Ctrl-C during the reads ends the watch with nil and prints neither the
// cancellation as an error nor a half refresh.
func TestWatchCancelDuringRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	funcs := interceptor.Funcs{List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList,
		_ ...client.ListOption) error {
		cancel()
		return ctx.Err()
	}}
	clk := &fakeClock{now: testNow, refreshes: 100, cancel: cancel}
	var stdout, stderr bytes.Buffer
	err := watchStatus(ctx, watchClient(t, funcs), Options{Namespace: testNS},
		watchOptions{interval: time.Second, output: outputText}, clk, nil, &stdout, &stderr)
	if err != nil || stdout.Len() != 0 || stderr.Len() != 0 || len(clk.waits) != 0 {
		t.Errorf("err %v, stdout %q, stderr %q, waits %v", err, stdout.String(), stderr.String(), clk.waits)
	}
}

// cancelWriter cancels the watch on its first write: Ctrl-C right after a refresh was printed.
type cancelWriter struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	w.cancel()
	return w.Buffer.Write(p)
}

// TestWatchCancelBeforeWarning: a cancellation while the CRD version check has not answered ends the watch
// instead of waiting for it.
func TestWatchCancelBeforeWarning(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clk := &fakeClock{now: testNow, refreshes: 100, cancel: cancel}
	stdout := &cancelWriter{cancel: cancel}
	var stderr bytes.Buffer
	never := make(chan string)
	done := make(chan error, 1)
	go func() {
		done <- watchStatus(ctx, watchClient(t, interceptor.Funcs{}), Options{Namespace: testNS},
			watchOptions{interval: time.Second, output: outputJSON}, clk, never, stdout, &stderr)
	}()
	select {
	case err := <-done:
		if err != nil || len(clk.waits) != 0 || stdout.Len() == 0 {
			t.Errorf("err %v, waits %v, stdout %q", err, clk.waits, stdout.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watch waited for the warning after the cancellation")
	}
}

// TestWatchPrintsWarningOnce: the CRD version warning is printed once, after the first refresh.
func TestWatchPrintsWarningOnce(t *testing.T) {
	warning := make(chan string, 1)
	warning <- "Warning: the CRDs are from another release"
	w := watchOptions{interval: time.Second, output: outputJSON}
	_, errOut, _ := runWatch(t, watchClient(t, interceptor.Funcs{}), w, 3, warning)
	if strings.Count(errOut, "Warning:") != 1 {
		t.Errorf("stderr = %q, want the warning once", errOut)
	}
}

// incidentsResource is the resource of Incidents in a refusal.
const incidentsResource = "incidents"

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestWatchStdoutFailureEnds: a write to stdout that fails (a closed pipe) ends the watch with that error
// instead of polling the API server for nobody.
func TestWatchStdoutFailureEnds(t *testing.T) {
	for _, output := range []string{outputText, outputJSON} {
		ctx, cancel := context.WithCancel(t.Context())
		clk := &fakeClock{now: testNow, refreshes: 2, cancel: cancel}
		var stderr bytes.Buffer
		err := watchStatus(ctx, watchClient(t, interceptor.Funcs{}), Options{Namespace: testNS},
			watchOptions{interval: time.Second, output: output}, clk, nil, failingWriter{}, &stderr)
		if err == nil || !strings.Contains(err.Error(), "broken pipe") || len(clk.waits) != 0 {
			t.Errorf("%s: err %v, waits %v", output, err, clk.waits)
		}
		cancel()
	}
}

// TestWatchForbiddenListKeepsGoing: a list refused by RBAC is a failed refresh like any other.
func TestWatchForbiddenListKeepsGoing(t *testing.T) {
	funcs := interceptor.Funcs{List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList,
		_ ...client.ListOption) error {
		gr := schema.GroupResource{Group: remediationv1alpha1.GroupVersion.Group, Resource: incidentsResource}
		return apierrors.NewForbidden(gr, "", errors.New("no list"))
	}}
	w := watchOptions{interval: time.Second, output: outputText}
	_, errOut, clk := runWatch(t, watchClient(t, funcs), w, 3, nil)
	if strings.Count(errOut, "Error at") != 3 || len(clk.waits) != 3 {
		t.Errorf("stderr %q, waits %v", errOut, clk.waits)
	}
}

// flagInterval is the --interval flag.
const flagInterval = "--interval"

func TestParseStatusWatch(t *testing.T) {
	f, err := parseStatus([]string{"-w"})
	if err != nil || !f.watch || f.interval != defaultInterval {
		t.Errorf("-w: %+v, %v", f, err)
	}
	f, err = parseStatus([]string{"--watch", flagInterval, "2", "-o", "json"})
	if err != nil || !f.watch || f.interval != 2*time.Second || f.output != outputJSON {
		t.Errorf("--interval 2: %+v, %v", f, err)
	}
	f, err = parseStatus([]string{"--watch", "--interval=1m30s"})
	if err != nil || f.interval != 90*time.Second {
		t.Errorf("--interval=1m30s: %+v, %v", f, err)
	}
	f, err = parseStatus(nil)
	if err != nil || f.watch {
		t.Errorf("no flag: %+v, %v", f, err)
	}
	for _, args := range [][]string{
		{flagInterval, "5s"}, {"-w", flagInterval, "500ms"}, {"-w", flagInterval, "0"}, {"-w", flagInterval, "-1s"},
		{"-w", flagInterval, "soon"},
	} {
		if _, err := parseStatus(args); err == nil {
			t.Errorf("parseStatus(%q) succeeded", args)
		}
	}
	for _, args := range [][]string{{"-w"}, {flagInterval, "5s"}} {
		if _, err := parseExplain(append([]string{"rr-1"}, args...)); err == nil {
			t.Errorf("explain %q succeeded", args)
		}
		if _, err := parseDoctor(args); err == nil {
			t.Errorf("doctor %q succeeded", args)
		}
	}
}
