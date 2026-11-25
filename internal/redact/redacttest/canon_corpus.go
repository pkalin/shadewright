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

package redacttest

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// CanonCase is one input of the canonical view corpus generated in canonical_gen.go, with the pieces of
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
		e := CanonCase{Form: c.Form, Name: c.Name, Hash: canonHash(c), In: c.In, Args: c.Args, Labels: c.Labels}
		masked := []byte(c.In)
		for _, r := range c.Secrets {
			s := c.In[r[0]:r[1]]
			e.Secrets = append(e.Secrets, s)
			for i := r[0]; i < r[1]; i++ {
				masked[i] = 0
			}
		}
		e.Masked = string(masked)
		for _, a := range c.Args {
			for _, s := range e.Secrets {
				a = strings.ReplaceAll(a, s, strings.Repeat("\x00", len(s)))
			}
			e.MaskedArgs = append(e.MaskedArgs, a)
		}
		for _, l := range c.Labels {
			a := l[0] + "=" + l[1]
			for _, s := range e.Secrets {
				a = strings.ReplaceAll(a, s, strings.Repeat("\x00", len(s)))
			}
			e.MaskedLabels = append(e.MaskedLabels, a)
		}
		for _, r := range c.Keep {
			e.Keep = append(e.Keep, c.In[r[0]:r[1]])
		}
		out = append(out, e)
	}
	return out, sampled
}

// CanonIndex returns what a leak table may name of each input of the full corpus, by hash.
func CanonIndex() map[string]CanonInput { return canonIndexOf(canonCorpus()) }

// canonSampleDivisor is the share of the corpus checked in short mode: one input in 32, chosen with a fixed seed.
// Under the race detector it is one in 2*RaceSampleEvery (decision of 2026-11-21 on #873). The full corpus runs in
// the pass without -race (make test).
const canonSampleDivisor = 32

// canonSample returns cases, or under the race detector and in short mode a seeded sample of them, and
// whether it is a sample.
func canonSample(cases []CanonSource) ([]CanonSource, bool) {
	if !RaceDetector && !testing.Short() {
		return cases, false
	}
	divisor := canonSampleDivisor
	if RaceDetector {
		divisor = 2 * RaceSampleEvery
	}
	rng := rand.New(rand.NewPCG(845, 8)) //nolint:gosec // a fixed seed keeps the sample the same on every run
	var out []CanonSource
	for _, c := range cases {
		if rng.IntN(divisor) == 0 {
			out = append(out, c)
		}
	}
	return out, true
}

// CanonInput is what a leak table may name of an input of the full corpus: its name and the number of its
// secret and kept pieces.
type CanonInput struct {
	Name          string
	Secrets, Keep int
}

// canonIndexOf returns the CanonInput of each case, by hash.
func canonIndexOf(cases []CanonSource) map[string]CanonInput {
	idx := make(map[string]CanonInput, len(cases))
	for _, c := range cases {
		idx[canonHash(c)] = CanonInput{Name: c.Name, Secrets: len(c.Secrets), Keep: len(c.Keep)}
	}
	return idx
}
