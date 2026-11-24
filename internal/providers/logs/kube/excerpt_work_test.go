//go:build redactwork

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

package kube

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

// This test counts work with [redact.Work], which exists only in a build with the tag redactwork; make test
// runs this package with it.

// Excerpt redacts all it is given, which is at most MaxReadBytes, and only then cuts (#840). Its work
// ([redact.Work]) is bounded on the largest input and on the hostile shapes the block rules read, and grows
// linearly: eight times the input costs at most about eight times the work. Since the fourteenth decision of
// 2026-11-18 (#845) the bound is on what Excerpt adds to the reading of the release before, whose passes are
// counted on their own ([redact.WorkOfReleased]): at most excerptOwnWorkPerByte per byte and linear, and Excerpt
// at most 1.15 times that reading plus that budget; the reading stays within excerptWorkPerByte and linear on
// these shapes. The work is counted, not timed, so a busy machine cannot fail it (#825).
func TestExcerptWorkIsBoundedOnTheLargestInput(t *testing.T) {
	if testing.Short() {
		t.Skip("large inputs")
	}
	r, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	// work returns the work of Excerpt on unit repeated to size bytes, and the part of it the reading of the
	// release before took.
	work := func(unit string, size int) (total, release int64, n int) {
		data := []byte(strings.Repeat(unit, size/len(unit)))
		total, release = redact.WorkOfReleased(func() { Excerpt(r, data, false, 64<<10) })
		return total, release, len(data)
	}
	per := func(w int64, n int) float64 { return float64(w) / float64(n) }
	grows := func(first, later float64) bool { return later > 1.125*first+0.5 }
	for _, unit := range []string{
		"password:\n  password:\n",
		"[--password, a\n  b ",
		`,\"token\":\"\\\\`,
		"x token: | pass: | \n  a\n",
		"msg: password: >- x\n",
		"credentials:\n  a: b\n  ref:\n    c: d\n",
		"Back-off restarting failed container\n",
		// The largest work per byte: the extent rule reads a window of each value after a quoted one.
		`TOKEN="a",b,`,
	} {
		st, sr, sn := work(unit, MaxReadBytes/8)
		lt, lr, ln := work(unit, MaxReadBytes)
		t.Logf("%q at %d bytes: %.1f steps per byte, the release's reading %.1f, the rest %.1f", unit, ln,
			per(lt, ln), per(lr, ln), per(lt-lr, ln))
		if lt-lr > excerptOwnWorkPerByte*int64(ln) {
			t.Errorf("%q: Excerpt adds %d steps to the release's reading on %d bytes, more than %d per byte", unit,
				lt-lr, ln, excerptOwnWorkPerByte)
		}
		if float64(lt) > 1.15*float64(lr)+float64(excerptOwnWorkPerByte*int64(ln)) {
			t.Errorf("%q: %d steps on %d bytes, more than 1.15 times the release's reading (%d) and %d per byte",
				unit, lt, ln, lr, excerptOwnWorkPerByte)
		}
		if grows(per(st-sr, sn), per(lt-lr, ln)) {
			t.Errorf("%q: Excerpt adds %.1f steps per byte at %d bytes, %.1f at %d: more than linear", unit,
				per(st-sr, sn), sn, per(lt-lr, ln), ln)
		}
		// The release's reading is linear on every shape since #873: it stays bounded.
		if lr > excerptWorkPerByte*int64(ln) || grows(per(sr, sn), per(lr, ln)) {
			t.Errorf("%q: the release's reading alone costs %.1f steps per byte at %d bytes, %.1f at %d", unit,
				per(sr, sn), sn, per(lr, ln), ln)
		}
	}
}

// excerptWorkPerByte bounds the work of the reading of the release before per byte of the input of Excerpt on
// these shapes: its extent rule, over all of it and again over the bytes kept, which cost about 710 steps per
// byte on the most costly one (`TOKEN="a",b,`) until #873 made the reading after each comma of a value read only
// the key name there (28 since).
const excerptWorkPerByte = 1024

// excerptOwnWorkPerByte bounds what Excerpt adds per byte to the reading of the release before: the rules of
// package redact over all of the input (about 715 per byte on `TOKEN="a",b,`), the trace of that reading and
// the cut.
const excerptOwnWorkPerByte = 768
