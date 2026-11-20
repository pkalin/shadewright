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

import "strings"

// CanonCase is one input of the canonical view corpus of #845 (canonical_gen_test.go), with the pieces of
// secret the generator placed in it and the pieces of text that must survive. Args, for a key and its value in
// two strings of a sequence, holds the strings as a container's arguments hold them; In is then one rendering
// of the sequence. Labels, for a metric series, holds its labels as query_metrics reads them. Masked is In, and
// MaskedArgs are Args and MaskedLabels the labels as query_metrics shows them ("name=value"), with every byte
// of a secret piece replaced by NUL.
type CanonCase struct {
	Form         string
	Name         string
	Hash         string
	In           string
	Args         []string
	Labels       [][2]string
	Secrets      []string
	Keep         []string
	Masked       string
	MaskedArgs   []string
	MaskedLabels []string
}

// CanonCorpus returns the canonical view corpus, or under the race detector and in short mode a seeded
// sample of it (canonSample), and whether it is a sample.
func CanonCorpus() ([]CanonCase, bool) {
	cases, sampled := canonSample(canonCorpus())
	out := make([]CanonCase, 0, len(cases))
	for _, c := range cases {
		e := CanonCase{Form: c.form, Name: c.name, Hash: canonHash(c), In: c.in, Args: c.args, Labels: c.labels}
		masked := []byte(c.in)
		for _, r := range c.secrets {
			s := c.in[r[0]:r[1]]
			e.Secrets = append(e.Secrets, s)
			for i := r[0]; i < r[1]; i++ {
				masked[i] = 0
			}
		}
		e.Masked = string(masked)
		for _, a := range c.args {
			for _, s := range e.Secrets {
				a = strings.ReplaceAll(a, s, strings.Repeat("\x00", len(s)))
			}
			e.MaskedArgs = append(e.MaskedArgs, a)
		}
		for _, l := range c.labels {
			a := l[0] + "=" + l[1]
			for _, s := range e.Secrets {
				a = strings.ReplaceAll(a, s, strings.Repeat("\x00", len(s)))
			}
			e.MaskedLabels = append(e.MaskedLabels, a)
		}
		for _, r := range c.keep {
			e.Keep = append(e.Keep, c.in[r[0]:r[1]])
		}
		out = append(out, e)
	}
	return out, sampled
}

// CanonIndex returns what a leak table may name of each input of the full corpus, by hash.
func CanonIndex() map[string]CanonInput { return canonIndexOf(canonCorpus()) }

// CanonPartialMin is canonPartialMin: the length of a secret piece's prefix and suffix that counts as a leak of
// the piece at an entry point.
const CanonPartialMin = canonPartialMin
