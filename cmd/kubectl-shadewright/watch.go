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
	"time"

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
// that fails writes its error to stderr and the next one runs; only a failed write to stdout ends the watch
// with an error. warning, when not nil, carries the CRD version warning, printed once after the first refresh.
// It starts no goroutine of its own.
func watchStatus(ctx context.Context, c client.Reader, opts Options, w watchOptions, clk clock,
	warning <-chan string, stdout, stderr io.Writer) error {
	for first := true; ; first = false {
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
		if err := writeRefresh(stdout, stderr, w, report, err, now, first); err != nil {
			return err
		}
		if warning != nil {
			select {
			case msg := <-warning:
				if msg != "" {
					_, _ = fmt.Fprintln(stderr, msg)
				}
			case <-ctx.Done():
				return nil
			}
			warning = nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-clk.After(w.interval):
		}
	}
}

// writeRefresh writes one refresh: in text, a header with the time and the report (after clearing the screen
// when redrawing, else after a blank line from the previous one); in JSON, the report as one line, so the
// output is newline-delimited JSON. A failed collect writes its error, with the time, to stderr only, so stdout
// stays valid JSON. It returns only the error of a write to stdout.
func writeRefresh(stdout, stderr io.Writer, w watchOptions, report *Report, collectErr error, now time.Time,
	first bool) error {
	stamp := now.Format(time.RFC3339)
	if w.output == outputJSON {
		if collectErr != nil {
			_, _ = fmt.Fprintf(stderr, "Error at %s: %s\n", stamp, clean(collectErr.Error(), maxMessage))
			return nil
		}
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
	case !first:
		header = "\n" + header
	}
	if _, err := io.WriteString(stdout, header); err != nil {
		return err
	}
	if collectErr != nil {
		_, _ = fmt.Fprintf(stderr, "Error at %s: %s\n", stamp, clean(collectErr.Error(), maxMessage))
		return nil
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
