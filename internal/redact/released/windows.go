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

package released

import (
	"strings"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// maxBase64Window is the most characters a window of base64Windows holds: more than the 44 of the longest
// slash-split secret it is meant for, and a bound that keeps the number of windows per part constant.
const maxBase64Window = 96

// windowPart is one slash-separated part of a run that holds a character besides padding, with what
// base64Windows adds up over a window of parts. An empty part (no character besides padding: "" between two
// slashes, or "=" alone at the end) is not kept: each part counts the empty parts right before it (gap). An empty
// part never starts or ends a window and adds nothing to one but its place, so the windows, the stretches and
// their ranges are those of a list that keeps every part; a run of slashes alone keeps no part at all, where a
// part for each slash held about 56 bytes for each byte of the run (#1087). Offsets take 32 bits: a run is part
// of an entry of at most 4 MiB.
//
// A part is held in 24 bytes: its characters and the empty parts before it take a byte each, at most
// maxPartCount. A part of more characters never joins a window (maxBase64Window), and more empty parts end any
// window (a window holds at most maxBase64Window/4 parts), so every comparison reads the same as with the counts
// in full: a window, a stretch (its flagged parts are in a window, the parts it takes in short) and a join
// (maxBase64Key) compare these counts with bounds under maxPartCount only.
type windowPart struct {
	score      float64 // sum of wordScore over the pairs
	start, end int32   // byte range in the run, padding included
	chars      uint8   // characters, padding excluded, at most maxPartCount
	gap        uint8   // empty parts right before this one, at most maxPartCount
	flags      uint8   // the part* flags
}

// maxPartCount is the most a count of a windowPart holds: more reads the same, see windowPart.
const maxPartCount = 255

// Flags of a windowPart.
const (
	partUpper     uint8 = 1 << iota // holds an upper-case letter, as evidence
	partLower                       // holds a lower-case letter, as evidence
	partBase64                      // may be part of standard base64: no "-" or "_", and "=" only as final padding
	partWord                        // a path word (pathWord)
	partLowerWord                   // a path word that starts with a lower-case letter
	partNoPairs                     // an acronym or a library name: its pairs count as no evidence
)

// has reports whether p has flag f.
func (p windowPart) has(f uint8) bool { return p.flags&f != 0 }

// pairs returns the pairs of neighbouring characters of p that count as evidence.
func (p windowPart) pairs() int {
	if p.has(partNoPairs) {
		return 0
	}
	return max(0, int(p.chars)-1)
}

// Marks of a part in base64Windows: redacted, and redacted with the empty parts after it up to the next part.
const (
	markRedact uint8 = 1 << iota
	markJoinNext
)

// base64Windows finds standard base64 cut short by slashes in a run that randomRun keeps (#525), such as an
// AWS secret access key after a path or in a CLI flag, and calls add with each range of the run to redact.
// A run without a slash is one part and goes through the same windows: a run of 32 or more characters that
// randomRun keeps (letters and "+" without a digit, say) is redacted when it scores as base64.
//
// It judges every window of adjacent parts that holds from minBase64Window to maxBase64Window characters of
// evidence, apart from the parts around it, so path words next to a key cannot dilute it: the window counts
// when it mixes cases, its parts average at least four characters, and its pairs of neighbouring characters
// score at most maxBase64Score on average (wordScore). A window never starts or ends at an empty part, and one
// that starts or ends at a lower-case path word must score at most -maxBase64Score ("/opt/" before library
// names scores as words, "cijt/" before a key as base64). Parts with "-" or "_" (Kubernetes names, UUIDs)
// never join a window, and parts of upper-case letters, maybe with digits (acronyms and library names such
// as "HTTPS" or "CUDA12"), join one without counting as evidence either way.
//
// Of the parts that some window covers, path words (pathWord) are kept and the others are flagged;
// takeInKeyParts then joins the pieces of a key and adds at most one short part on each side of it. Each
// stretch of adjacent redacted parts is one range, so two keys with a path between them give two ranges and
// the path stays.
// Windows are bounded by maxBase64Window, so the cost is linear in the run.
//
// Known gaps when no secret key name stands before the key, kept for safety and simplicity over perfect paths
// (TestSlashSplitBase64KnownLeaks; after a key name the value goes whatever it reads as, #585: after
// "client_secret=" up to the next whitespace, after "--client-secret" one token or a quoted value on one
// line): a key whose windows all score as words stays whole; a short lower-case end of a key that no window
// covers ("/wed7") stays, as a path word would; a key of upper-case parts with one short mixed part
// ("ABCDEF/GHIJKL/.../yz2+ab3+cd4+") has too little evidence and stays; and a lower-case path word at either
// end of a key ("cijt/...") is kept with the path around it.
func base64Windows(wm *meter.Meter, run string, add func(start, end int)) {
	parts := windowParts(wm, run)
	covered := coveredParts(wm, parts)
	if covered == nil {
		return
	}
	for i, p := range parts {
		tick(wm, 1)
		covered[i] = covered[i] && !p.has(partWord)
	}
	addStretches(wm, parts, takeInKeyParts(wm, parts, covered), add)
}

// windowParts returns the parts of run that are not empty (windowPart), each with the empty parts before it
// counted, in a slice sized once.
func windowParts(wm *meter.Meter, run string) []windowPart {
	// The parts counted to size the slice: the loop that builds them counts the steps of this one too, as the one
	// loop over the parts did before (a step a part).
	parts := make([]windowPart, 0, countParts(run))
	var gap uint8
	eachPart(wm, run, func(start, end int) {
		if emptyPart(run, start, end) {
			if gap < maxPartCount {
				gap++
			}
			return
		}
		p := newWindowPart(wm, run, start, end)
		p.gap, gap = gap, 0
		parts = append(parts, p)
	})
	return parts
}

// countParts returns the parts of run that are not empty. It counts no step: the loop that builds them counts
// one a part.
func countParts(run string) int {
	n := 0
	for pos := 0; pos <= len(run); {
		end := pos + strings.IndexByte(run[pos:], '/')
		if end < pos {
			end = len(run)
		}
		if !emptyPart(run, pos, end) {
			n++
		}
		pos = end + 1
	}
	return n
}

// eachPart calls f with the byte range of each slash-separated part of run, in order, a step a part.
func eachPart(wm *meter.Meter, run string, f func(start, end int)) {
	for pos := 0; pos <= len(run); {
		tick(wm, 1)
		end := pos + strings.IndexByte(run[pos:], '/')
		if end < pos {
			end = len(run)
		}
		f(pos, end)
		pos = end + 1
	}
}

// emptyPart reports whether the part run[start:end] holds no character besides padding: nothing, or, at the end
// of the run, "=" alone.
func emptyPart(run string, start, end int) bool {
	if end == len(run) {
		return strings.TrimRight(run[start:end], "=") == ""
	}
	return start == end
}

// coveredParts reports, for each part, whether a window that qualifies as base64 covers it (see
// base64Windows), or returns nil when no window qualifies. The empty parts between two parts count in the
// number of parts of a window (n) as the parts they are.
func coveredParts(wm *meter.Meter, parts []windowPart) []bool {
	// diff counts, as a difference array, the qualifying windows over each part.
	var diff []int32
	for lo := range parts {
		tick(wm, 1)
		var chars, pairs, n int
		var score float64
		var upper, lower bool
		for j := lo; j < len(parts) && parts[j].has(partBase64); j++ {
			tick(wm, 1)
			p := parts[j]
			if j > lo {
				// The empty parts before p join the window one by one, each ending it when the window has too
				// many parts; they add no characters.
				n += int(p.gap)
				if 4*n > maxBase64Window {
					break
				}
			}
			n++
			chars, pairs, score = chars+int(p.chars), pairs+p.pairs(), score+p.score
			upper, lower = upper || p.has(partUpper), lower || p.has(partLower)
			if chars > maxBase64Window || 4*n > maxBase64Window {
				break
			}
			limit := maxBase64Score
			if parts[lo].has(partLowerWord) || p.has(partLowerWord) {
				limit = -maxBase64Score // a window at a lower-case path word must read clearly as base64
			}
			if chars >= minBase64Window && upper && lower && chars >= 4*n && score <= limit*float64(pairs) {
				if diff == nil {
					diff = make([]int32, len(parts)+1)
				}
				diff[lo]++
				diff[j+1]--
			}
		}
	}
	if diff == nil {
		return nil
	}
	covered := make([]bool, len(parts))
	var n int32
	for i := range parts {
		tick(wm, 1)
		n += diff[i]
		covered[i] = n > 0
	}
	return covered
}

// maxTakeInPart is the longest part next to a key that takeInKeyParts takes in with it.
const maxTakeInPart = 5

// maxBase64Key is the most characters, slashes and padding aside, of the slash-split secrets base64Windows
// is meant for: the 44 of an older Entra client secret. Two stretches and the one part between them that
// hold no more than that are one key, whatever the part reads as (takeInKeyParts).
const maxBase64Key = 44

// keyStretch is a stretch of parts that base64Windows redacts: from the first to the last non-empty part,
// with the characters of its parts.
type keyStretch struct{ first, last, chars int }

// takeInKeyParts decides, from the parts that windows flagged (redact on entry: covered and no path word),
// which parts to redact, and writes that back into redact. Flagged parts with only empty parts ("//")
// between them form one stretch. A part with "-" or "_" (a Kubernetes name) is never added to a stretch, a
// lower-case path word ("etc", "api", "keys") or a long part only to join two pieces of one key:
//
//   - One part alone between two stretches joins them when the two stretches and the part hold at most
//     maxBase64Key characters, whatever the part reads as ("ghquje" or "Vpzfixaifdo" inside a key). Beyond
//     that, it joins them only when it is short (shortPart: at most maxTakeInPart characters) and no
//     lower-case path word, or when either stretch is shorter than minBase64Stretch (a piece of the same key
//     that would be kept on its own). A lower-case path word between two stretches of at least that length
//     ("/api/" between two keys) stays, and the keys are redacted each on its own.
//   - Then each stretch takes in at most one more part on each side: the next non-empty part, when it is
//     short, no lower-case path word and not between two stretches, such as "wed7" at the end of ".../wed7"
//     or a short capitalised word. A part taken in takes in nothing further, so the path after a key loses
//     one short part at most.
//
// Each step looks at a part and its neighbours once, so the cost is linear in the number of parts. Since the
// empty parts are not kept (windowPart), the next non-empty part is the next part kept, and the marks it returns
// (markRedact, markJoinNext) say which parts are redacted and which empty parts with them.
func takeInKeyParts(wm *meter.Meter, parts []windowPart, flagged []bool) []uint8 {
	var stretches []keyStretch
	for i := range parts {
		tick(wm, 1)
		if !flagged[i] {
			continue
		}
		if n := len(stretches); n > 0 && stretches[n-1].last+1 == i {
			stretches[n-1].last, stretches[n-1].chars = i, stretches[n-1].chars+int(parts[i].chars)
			continue
		}
		stretches = append(stretches, keyStretch{first: i, last: i, chars: int(parts[i].chars)})
	}
	marks := make([]uint8, len(parts))
	if len(stretches) == 0 {
		return marks
	}
	// Join stretches across one short part.
	merged := []keyStretch{stretches[0]}
	for _, s := range stretches[1:] {
		tick(wm, 1)
		prev := &merged[len(merged)-1]
		c := prev.last + 1
		joined := prev.chars + int(parts[c].chars) + s.chars
		if c != s.first-1 || !parts[c].has(partBase64) || joined > maxBase64Key &&
			(!shortPart(wm, parts[c]) || parts[c].has(partLowerWord) && prev.chars >= minBase64Stretch && s.chars >= minBase64Stretch) {
			merged = append(merged, s)
			continue
		}
		prev.last, prev.chars = s.last, joined
	}
	// next returns the part after i in direction step (1 or -1), or -1.
	next := func(i, step int) int {
		if i += step; i >= 0 && i < len(parts) {
			return i
		}
		return -1
	}
	for k, s := range merged {
		tick(wm, 1)
		first, last := s.first, s.last
		// takeIn returns the part next to the stretch edge at i in direction step when it may be taken in.
		takeIn := func(i, step int) int {
			c := next(i, step)
			if c < 0 || parts[c].has(partLowerWord) || !shortPart(wm, parts[c]) {
				return i
			}
			beyond := next(c, step)
			if beyond >= 0 && (step < 0 && k > 0 && beyond == merged[k-1].last ||
				step > 0 && k+1 < len(merged) && beyond == merged[k+1].first) {
				return i // between two stretches
			}
			return c
		}
		first, last = takeIn(first, -1), takeIn(last, 1)
		for i := first; i <= last; i++ {
			tick(wm, 1)
			marks[i] |= markRedact
			if i < last {
				marks[i] |= markJoinNext
			}
		}
	}
	return marks
}

// shortPart reports whether p is short enough to be redacted with a key next to it (takeInKeyParts): at most
// maxTakeInPart characters of standard base64, so no "-" or "_" (and no ".", which ends a run).
func shortPart(_ *meter.Meter, p windowPart) bool {
	return p.has(partBase64) && p.end-p.start <= maxTakeInPart
}

// addStretches calls add with the range of each stretch of adjacent redacted parts, without empty parts at
// its ends, that holds at least minBase64Stretch characters. Two parts kept are adjacent when no empty part
// stands between them, or when the empty parts between them are redacted with them (markJoinNext).
func addStretches(wm *meter.Meter, parts []windowPart, marks []uint8, add func(start, end int)) {
	for i := 0; i < len(parts); {
		tick(wm, 1)
		if marks[i]&markRedact == 0 {
			i++
			continue
		}
		j := i
		for j+1 < len(parts) && marks[j+1]&markRedact != 0 && (parts[j+1].gap == 0 || marks[j]&markJoinNext != 0) {
			tick(wm, 1)
			j++
		}
		chars := 0
		for k := i; k <= j; k++ {
			tick(wm, 1)
			chars += int(parts[k].chars)
		}
		// A short stretch among path words that a long window reached over is a word, not a key.
		if chars >= minBase64Stretch {
			add(int(parts[i].start), int(parts[j].end))
		}
		i = j + 1
	}
}

// minBase64Stretch is the fewest characters a stretch of adjacent parts that base64Windows redacts holds: a
// key gives one long stretch, while a part that is no path word among words a window covers ("orders" in
// "/namespaces/orders/pods/") gives a short one, which is kept.
const minBase64Stretch = minBase64Window / 2

// minPathWordScore is the score per pair (wordScore) from which a part of four or more lower-case letters
// reads as a path word ("keys" 0.91, "download" 0.84, "credentials" 0.86) in pathWord, and
// minShortPathWordScore the same for three letters ("etc", "app" 0.49, "api" 1.29), which have one
// alternation of vowel and consonant at most. Random letters reach them less often, the longer they are.
const (
	minPathWordScore      = 0.8
	minShortPathWordScore = 0.4
)

// pathWord reports whether part, next to or at the end of a base64 window, is a path word rather than a
// piece of the key: at least three lower-case letters that read as a word (minPathWordScore), maybe after
// one capital and maybe followed by digits ("etc", "credentials", "python3", "Documents"), or a version
// ("v1"). A path word that a window covers is not flagged as key; a window that starts or ends at a
// lower-case one must score clearly as base64, and a lower-case one is redacted with a key only when it joins
// two pieces of one (takeInKeyParts).
func pathWord(wm *meter.Meter, part string) bool {
	capital := part != "" && part[0] >= 'A' && part[0] <= 'Z'
	if capital {
		part = part[1:]
	}
	letters := 0
	for letters < len(part) && part[letters] >= 'a' && part[letters] <= 'z' {
		tick(wm, 1)
		letters++
	}
	for i := letters; i < len(part); i++ {
		tick(wm, 1)
		if part[i] < '0' || part[i] > '9' {
			return false
		}
	}
	if !capital && part[:letters] == "v" && letters < len(part) {
		return true
	}
	if letters < 3 {
		return false
	}
	var score float64
	for i := 1; i < letters; i++ {
		tick(wm, 1)
		score += wordScore[wordClass(wm, part[i-1])][wordClass(wm, part[i])]
	}
	if letters == 3 {
		return score >= minShortPathWordScore*float64(letters-1)
	}
	return score >= minPathWordScore*float64(letters-1)
}

// newWindowPart returns the part run[start:end] for base64Windows. A part of upper-case letters, maybe with
// digits, is an acronym or a library name ("REST", "CUDA12", "NCCL2") as often as a piece of a key, so it
// counts as no evidence: its characters fill a window, but it adds no pairs, no score and no case.
// The part is not empty (emptyPart).
func newWindowPart(wm *meter.Meter, run string, start, end int) windowPart {
	p := windowPart{start: int32(start), end: int32(end), flags: partBase64} // #nosec G115 -- a run is part of an entry of at most 4 MiB
	text := run[start:end]
	if end == len(run) {
		text = strings.TrimRight(text, "=")
	}
	acronym := true
	var upper, lower bool
	for i := range len(text) {
		tick(wm, 1)
		c := text[i]
		switch {
		case c == '-' || c == '_' || c == '=':
			p.flags &^= partBase64
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		}
		acronym = acronym && (c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
		if i > 0 {
			p.score += wordScore[wordClass(wm, text[i-1])][wordClass(wm, c)]
		}
	}
	p.chars = uint8(min(len(text), maxPartCount)) // #nosec G115 -- at most maxPartCount
	if pathWord(wm, run[start:end]) {
		p.flags |= partWord
		if run[start] >= 'a' && run[start] <= 'z' {
			p.flags |= partLowerWord
		}
	}
	if acronym && upper {
		p.score = 0
		p.flags |= partNoPairs
		return p
	}
	if upper {
		p.flags |= partUpper
	}
	if lower {
		p.flags |= partLower
	}
	return p
}
