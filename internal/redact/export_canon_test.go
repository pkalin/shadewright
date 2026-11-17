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

// CanonCase is one input of the canonical view corpus of #845 (canonical_gen_test.go), with the pieces of
// secret the generator placed in it. Args, for a key and its value in two strings of a sequence, holds the
// strings as a container's arguments hold them; In is then one rendering of the sequence.
type CanonCase struct {
	Form    string
	Name    string
	Hash    string
	In      string
	Args    []string
	Secrets []string
}

// CanonCorpus returns the canonical view corpus, or under the race detector and in short mode a seeded
// sample of it (canonSample), and whether it is a sample.
func CanonCorpus() ([]CanonCase, bool) {
	cases, sampled := canonSample(canonCorpus())
	out := make([]CanonCase, 0, len(cases))
	for _, c := range cases {
		e := CanonCase{Form: c.form, Name: c.name, Hash: canonHash(c), In: c.in, Args: c.args}
		for _, r := range c.secrets {
			e.Secrets = append(e.Secrets, c.in[r[0]:r[1]])
		}
		out = append(out, e)
	}
	return out, sampled
}
