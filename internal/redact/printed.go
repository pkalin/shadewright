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
	"strings"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// The printed view. A terminal prints neither a control, nor the rest of an escape sequence, nor an invisible
// character: "pass" ESC "[31m" "word=V" shows as "password=V" in red, and "--password" CR U+200B " V" as
// "--password V". The controls view leaves out the escape character alone and keeps "[31m", which still splits the
// key, and it keeps an invisible character, which the reading of the text as it is can take as a flag's whole
// value. So the shown short entry point (ReplaceCheckedShortShown) reads a text that holds a control once more as
// it is printed: without its controls, without its invisible characters (isInvisible) and without the whole of each
// escape sequence: a CSI sequence (ESC "[" or the C1 CSI U+009B, then parameter bytes 0x30 to 0x3F, intermediate
// bytes 0x20 to 0x2F and one final byte 0x40 to 0x7E), or ESC and one byte 0x40 to 0x5F. A CSI without its final
// byte is not a sequence: only its controls are left out. Its matches are added as those of the controls view are
// (addControlsView): counted on the bytes of the text, never taking away a byte another reading hides. A text
// without a control has no printed view (the folded view reads its invisible characters). The work is linear in the length of the text: each byte is read once.

// escapeSeqLen returns the length of the escape sequence that starts at s[i], or 0 when none does.
func escapeSeqLen(wm *meter.Meter, s string, i int) int {
	switch {
	case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[', s[i] == 0xc2 && i+1 < len(s) && s[i+1] == 0x9b:
	case s[i] == 0x1b && i+1 < len(s) && s[i+1] >= 0x40 && s[i+1] <= 0x5f:
		return 2
	default:
		return 0
	}
	j := i + 2 // after ESC "[" or the two bytes of U+009B
	for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
		tick(wm, 1)
		j++
	}
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
		tick(wm, 1)
		j++
	}
	if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
		return j + 1 - i
	}
	return 0
}

// printedViewOf returns the printed view of s, and false when s holds no control. It is read even when it leaves out
// nothing but controls, and is then the controls view read again: whether a text has the view depends only on
// whether it holds a control, so the work of a shape grows with its length alone. The work is linear in the length of s; it allocates the
// view's text and one gap for each run removed.
func printedViewOf(wm *meter.Meter, s string) (controlsView, bool) {
	if !hasControl(wm, s) {
		return controlsView{}, false
	}
	var b strings.Builder
	b.Grow(len(s))
	var v controlsView
	removable := func(i int) int {
		if n := escapeSeqLen(wm, s, i); n > 0 {
			return n
		}
		if n := controlLen(wm, s, i); n > 0 {
			return n
		}
		return invisibleLen(wm, s, i)
	}
	for i := 0; i < len(s); {
		tick(wm, 1)
		n := removable(i)
		if n == 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		for i += n; i < len(s); i += n {
			tick(wm, 1)
			if n = removable(i); n == 0 {
				break
			}
		}
		v.gaps = append(v.gaps, controlsGap{at: int32(b.Len()), removed: int32(i - b.Len())}) // #nosec G115 -- an entry is at most MaxEntry bytes
	}
	v.text = b.String()
	return v, true
}

// addPrintedView adds to l the matches of the printed view of s that l does not already hold whole, as
// addControlsView adds those of the controls view. The matches l holds are never changed or dropped.
func (r *Redactor) addPrintedView(wm *meter.Meter, s string, l *matchList) {
	v, ok := printedViewOf(wm, s)
	if !ok {
		return
	}
	l.merge(wm)
	r.eachControlsMatch(wm, v, func(start, end, rule int) {
		if !l.covers(wm, start, end) {
			l.add(wm, start, end, rule)
		}
	})
}
