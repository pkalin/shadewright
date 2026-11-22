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
	"math/rand/v2"
	"strings"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// This file builds the corpus of the condition of #852 (decision of 2026-11-22): text without control characters
// is decided at every entry point exactly as on origin/main. It compiles on origin/main as it is, so that the
// table of main's outputs (controlFreeMainTable in controlfree_entry_test.go) is written there from the same
// inputs.

// ControlFreeCase is one input of the control-free corpus: a text, and for a sequence or a metric series its
// strings or labels.
type ControlFreeCase struct {
	Name   string
	In     string
	Args   []string
	Labels [][2]string
}

// controlFreeGenerated is the number of inputs drawn from the grammar of the differential fuzz targets
// (redacttest.Decode), and controlFreeSeed the seed they are drawn with.
const (
	controlFreeGenerated = 1500
	controlFreeSeed      = 852
	// controlFreeGeneratedBytes is the length a generated input is cut to.
	controlFreeGeneratedBytes = 512
	// controlFreeBlockEvery keeps one input in that many of the block values of EntryCorpus (about 9,800); of its
	// block forms (about 3,150) every second is kept.
	controlFreeBlockEvery = 4
	// controlFreeCanonEvery keeps one input in that many of the canonical view corpus (about 26,000 inputs, in
	// generator order), so that the test over 21 entry points stays near a minute of processor time.
	controlFreeCanonEvery = 8
)

// isControlRune reports whether r is a control character as #852 removes it: Unicode category Cc but newline
// and tab. It is written here, not taken from the package, so that the corpus is the same on origin/main.
func isControlRune(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)
}

// stripControls returns s with every control character removed, again until none is left (removing one can join
// two invalid bytes into one: "\xc2" NUL "\x85" is NEL). Invalid bytes are kept.
func stripControls(s string) string {
	for {
		var b strings.Builder
		removed := false
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			if (r != utf8.RuneError || size != 1) && isControlRune(r) {
				removed = true
			} else {
				b.WriteString(s[i : i+size])
			}
			i += size
		}
		if !removed {
			return s
		}
		s = b.String()
	}
}

// ControlFreeCorpus returns the corpus of the condition: the ordinary messages of #840, the block corpora
// (EntryCorpus: one block value in controlFreeBlockEvery, every second block form) and one input in controlFreeCanonEvery of the canonical view corpus of #845 (with its sequences
// and metric series), each
// input without its control characters (stripControls; most hold none), then controlFreeGenerated inputs of the
// fuzz grammar, drawn with a fixed seed and cut to controlFreeGeneratedBytes, without theirs. An input that is already in the corpus is not added
// again. Every input is free of control characters by construction.
func ControlFreeCorpus() []ControlFreeCase {
	var out []ControlFreeCase
	seen := map[string]bool{}
	add := func(c ControlFreeCase) {
		c.In = stripControls(c.In)
		for i := range c.Args {
			c.Args[i] = stripControls(c.Args[i])
		}
		for i := range c.Labels {
			c.Labels[i] = [2]string{stripControls(c.Labels[i][0]), stripControls(c.Labels[i][1])}
		}
		key := c.In + "\x00" + strings.Join(c.Args, "\x00")
		for _, l := range c.Labels {
			key += "\x00" + l[0] + "=" + l[1]
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, c)
	}
	for _, in := range ordinaryCorpus() {
		add(ControlFreeCase{Name: "ordinary", In: in})
	}
	for i, c := range EntryCorpus() {
		if i%controlFreeBlockEvery != 0 && (strings.HasPrefix(c.Name, "block value: ") || i%2 != 0) {
			continue
		}
		add(ControlFreeCase{Name: c.Name, In: c.In})
	}
	for i, c := range canonCorpus() {
		if i%controlFreeCanonEvery != 0 {
			continue
		}
		add(ControlFreeCase{Name: "canonical: " + c.name, In: c.in, Args: append([]string(nil), c.args...),
			Labels: append([][2]string(nil), c.labels...)})
	}
	rng := rand.New(rand.NewPCG(controlFreeSeed, controlFreeSeed)) // #nosec G404 -- a seeded corpus, no secret
	for i := range controlFreeGenerated {
		data := make([]byte, 8+rng.IntN(56))
		for j := range data {
			data[j] = byte(rng.UintN(256)) // #nosec G115 -- below 256
		}
		// An odd mode byte reads the rest as the grammar; every fourth input is raw bytes.
		if i%4 != 0 {
			data[0] |= 1
		} else {
			data[0] &^= 3
		}
		in := redacttest.Decode(data, 1).Joined("")
		// A repeat of the grammar fills a field to its limit; the first controlFreeGeneratedBytes are enough.
		add(ControlFreeCase{Name: "generated", In: in[:min(len(in), controlFreeGeneratedBytes)]})
	}
	return out
}
