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

package redact

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Two long runs over the same bytes can end at different places: a chunk of short windows followed by a start
// character that opens a long window ends before that character, and the same chunk at the end of a text takes
// the character as its last window. The matches of the first are taken for the second, so they must hold the one
// that starts at that character (#873, review of 2026-11-21).
func TestWindowedSameBytesOtherEnd(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(`@x*`)
	w := newWindowed(nil, re)
	if w.whole {
		t.Fatalf("%s is read whole", re)
	}
	chunk := strings.Repeat("@xx", 300)
	pad := strings.Repeat(" ", windowMinText)
	texts := []string{
		" " + chunk + "@" + strings.Repeat("x", 2*windowMinRun) + pad, // the chunk ends before the long window's @
		pad + " " + chunk + "@", // the same bytes end the text: @ is in the chunk
	}
	ids := &windowIDs{}
	for _, s := range texts {
		if got, want := w.findAllWith(nil, s, ids), re.FindAllStringSubmatchIndex(s, -1); !reflect.DeepEqual(got, want) {
			t.Errorf("on %d bytes after the texts before: windows found %d matches, the whole text %d", len(s), len(got), len(want))
		}
	}
}
