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

// EntryCase is one generated input of the block corpora, as a caller reads it, with the secrets the
// generator placed in it. The entry point test (package redact_test) runs it through every function that
// produces text for the model, a status, an Event or a log line.
type EntryCase struct {
	Name    string
	In      string
	Secrets []string
}

// EntryCorpus returns every input of both block corpora that the redactor's own tests require to be
// redacted. A value that only a source flattened is given with its lines, as the callers read it before they
// flatten it themselves (blockPipeline); the one-line forms that say "block" are given flattened.
func EntryCorpus() []EntryCase {
	var out []EntryCase
	for _, c := range blockCorpus(blockSecretKeys, true) {
		if !c.indicator && (c.afterText || c.bareQuoted) {
			continue
		}
		in := c.in
		if !c.indicator && c.lined != c.in {
			in = c.lined
		}
		e := EntryCase{Name: "block value: " + c.name, In: in}
		for _, r := range c.secrets {
			e.Secrets = append(e.Secrets, c.in[r[0]:r[1]])
		}
		out = append(out, e)
	}
	for _, c := range formCorpus() {
		e := EntryCase{Name: "block form: " + c.name, In: c.in}
		for _, r := range c.secrets {
			e.Secrets = append(e.Secrets, c.in[r[0]:r[1]])
		}
		out = append(out, e)
	}
	return out
}
