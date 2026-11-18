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

	"github.com/pkalin/shadewright/internal/redact"
)

// This test counts work with [redact.Work], which exists only in a build with the tag redactwork; make test
// runs this package with it.

// Excerpt redacts all it is given, which is at most MaxReadBytes, and only then cuts (#840). Its work
// ([redact.Work]) is bounded by excerptWorkPerByte steps per byte of that input, on the largest input and on
// the hostile shapes the block rules read, and grows linearly: eight times the input costs at most about
// eight times the work. The work is counted, not timed, so a busy machine cannot fail it (#825).
func TestExcerptWorkIsBoundedOnTheLargestInput(t *testing.T) {
	if testing.Short() {
		t.Skip("large inputs")
	}
	r, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	work := func(unit string, size int) (int64, int) {
		data := []byte(strings.Repeat(unit, size/len(unit)))
		return redact.Work(func() { Excerpt(r, data, false, 64<<10) }), len(data)
	}
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
		small, smallSize := work(unit, MaxReadBytes/8)
		large, largeSize := work(unit, MaxReadBytes)
		if large > excerptWorkPerByte*int64(largeSize) {
			t.Errorf("%q: %d steps for %d bytes, more than %d per byte", unit, large, largeSize, excerptWorkPerByte)
		}
		if float64(large)/float64(largeSize) > 1.125*float64(max(small, 1))/float64(smallSize)+0.5 {
			t.Errorf("%q: %d steps for %d bytes, %d for %d: more than linear", unit, small, smallSize, large, largeSize)
		}
	}
}

// excerptWorkPerByte bounds the work of Excerpt per byte of its input: the rules of package redact over all of
// it and the first pass of the reading of the release before #845 over all of it (#845, thirteenth decision of
// 2026-11-18), each at most about 710 steps per byte on the most costly shape (about 1,430 together on
// `TOKEN="a",b,`), and the second pass of that reading over the bytes kept.
const excerptWorkPerByte = 1536
