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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The bounds of --interval.
const (
	defaultInterval = 5 * time.Second
	minInterval     = time.Second
)

// clearScreen moves the cursor home and clears the terminal, so each refresh redraws the status in place. It
// is a fixed sequence written by the plugin; text from the cluster never carries one (clean removes them).
const clearScreen = "\x1b[H\x1b[2J"

// clock is the time source of a watch, so tests can drive the refreshes without waiting.
type clock interface {
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// interruptContext returns a context cancelled by the first Ctrl-C or SIGTERM, which ends the watch. That
// first signal also restores the default handling before the context is done, so a second one ends the
// process even when a call does not return. stop releases the signals.
func interruptContext(parent context.Context) (context.Context, context.CancelFunc) {
	sctx, stopSignals := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(parent)
	go func() {
		<-sctx.Done()
		stopSignals()
		cancel()
	}()
	return ctx, func() {
		stopSignals()
		cancel()
	}
}

// watchOptions are the settings of `status --watch`.
type watchOptions struct {
	// interval is the time from the end of one refresh to the start of the next.
	interval time.Duration
	// timeout limits the reads of each refresh; 0 means no limit.
	timeout time.Duration
	// output is text or json.
	output string
	// redraw clears the screen before each text refresh; set only when stdout is a terminal, so a redirected
	// output keeps every refresh without escape sequences.
	redraw bool
}

// watchStatus collects and writes the status again and again, waiting interval between refreshes, until ctx
// is done (Ctrl-C), and then returns nil. It polls with the same get and list reads as a single status: a
// Kubernetes watch would need the watch verb, which the plugin's documented role does not grant. Each refresh
// is a whole new read, so there is no watch stream to drop or resume and no event to lose or repeat. A refresh
// that fails writes its error to stderr, nothing to stdout, and the next one runs. A refresh refused as
// Forbidden or Unauthorized ends the watch with that error, since waiting does not grant access, and so does a
// failed write to stdout. warning, when not nil, carries the CRD version warning, printed after the first
// refresh and, when redrawing, again after each refresh that cleared the screen. It starts no goroutine of its
// own.
func watchStatus(ctx context.Context, c client.Reader, opts Options, w watchOptions, clk clock,
	warning <-chan string, stdout, stderr io.Writer) error {
	warnMsg := ""
	// printed is whether a refresh was written to stdout, so the next one is set apart from it.
	printed := false
	for {
		now := clk.Now()
		rctx, cancel := ctx, context.CancelFunc(func() {})
		if w.timeout > 0 {
			rctx, cancel = context.WithTimeout(ctx, w.timeout)
		}
		report, err := collect(rctx, c, opts, now)
		cancel()
		if ctx.Err() != nil {
			// Interrupted during the reads: their error is only the cancellation, so print nothing.
			return nil
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return err
		}
		if werr := writeRefresh(stdout, stderr, w, report, err, now, printed); werr != nil {
			return werr
		}
		printed = printed || err == nil
		if warning != nil {
			select {
			case warnMsg = <-warning:
			case <-ctx.Done():
				return nil
			}
			warning = nil
			if warnMsg != "" {
				_, _ = fmt.Fprintln(stderr, warnMsg)
			}
		} else if w.redraw && err == nil && warnMsg != "" {
			// This refresh cleared the screen, and the warning with it.
			_, _ = fmt.Fprintln(stderr, warnMsg)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-clk.After(w.interval):
		}
	}
}

// writeRefresh writes one refresh: in text, a header with the time and the report (after clearing the screen
// when redrawing, else after a blank line when an earlier refresh was printed); in JSON, the report as one
// line, so the output is newline-delimited JSON. A failed collect writes its error, with the time, to stderr
// only, so stdout stays valid JSON and, in text, the last good status stays on the screen. It returns only the
// error of a write to stdout.
func writeRefresh(stdout, stderr io.Writer, w watchOptions, report *Report, collectErr error, now time.Time,
	printed bool) error {
	stamp := now.Format(time.RFC3339)
	if collectErr != nil {
		_, _ = fmt.Fprintf(stderr, "Error at %s: %s\n", stamp, clean(collectErr.Error(), maxMessage))
		return nil
	}
	if w.output == outputJSON {
		b, err := json.Marshal(report)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", b)
		return err
	}
	header := fmt.Sprintf("Every %s, last read %s (Ctrl-C to stop)\n\n", w.interval, stamp)
	switch {
	case w.redraw:
		header = clearScreen + header
	case printed:
		header = "\n" + header
	}
	if _, err := io.WriteString(stdout, header); err != nil {
		return err
	}
	return renderText(stdout, report, now)
}

// isTerminal reports whether w is a character device, such as a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
