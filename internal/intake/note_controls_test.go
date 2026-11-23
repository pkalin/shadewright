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

package intake

import (
	"fmt"
	"testing"
)

// noteControls returns every code point the note is stored without, in order: Unicode category Cc but newline
// and tab (C0, DEL and C1, U+0085 among them). It is written here, not taken from the code under test.
func noteControls() []rune {
	var out []rune
	for r := rune(0); r <= 0x9f; r++ {
		if (r < 0x20 && r != '\n' && r != '\t') || r >= 0x7f {
			out = append(out, r)
		}
	}
	return out
}

// The judge's note, generated (#852): every removable control at the start, in the middle and at the end of a
// plain note, in its JSON escape, is removed and the rest stored; a secret the redactor finds only before the
// control is removed ("x" CR "Bearer" and a token: the control is the word boundary), and a secret key the removed
// control joins ("pass" ESC "word="), drop the note. The answers are known by construction: the stored note is the
// note as built without the control, never the code's own cleaning of it.
func TestJudgeNoteControlsGenerated(t *testing.T) {
	r := newFixture(t).scorer.Redactor
	answer := func(note string) string {
		return `{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful","note":"` + note + `"}`
	}
	esc := func(c rune) string { return fmt.Sprintf(`\u%04x`, c) }
	check := func(name, note, stored string, dropped bool) {
		t.Helper()
		a, ok := parseAnswer(r, answer(note))
		if !ok {
			t.Fatalf("%s: answer refused", name)
		}
		if a.Note != stored || a.NoteDropped != dropped {
			t.Errorf("%s: note %q stored as %q (dropped %v), want %q (dropped %v)", name, note, a.Note, a.NoteDropped,
				stored, dropped)
		}
	}
	cases := 0
	const left, right = "the limit is", " too low"
	for _, c := range noteControls() {
		e := esc(c)
		check(fmt.Sprintf("%U at the start", c), e+left+right, left+right, false)
		check(fmt.Sprintf("%U in the middle", c), left+e+right, left+right, false)
		check(fmt.Sprintf("%U at the end", c), left+right+e, left+right, false)
		check(fmt.Sprintf("%U before Bearer", c), "x"+e+"Bearer Qz7secretValue1", "", true)
		check(fmt.Sprintf("%U inside a secret key", c), "pass"+e+"word=Hunter2Secret9x", "", true)
		cases += 5
	}
	t.Logf("%d notes, %d controls", cases, len(noteControls()))
}
