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

package eval

import (
	"slices"
	"strings"
	"testing"
	"unicode"
)

// termIn reports whether a rubric term is in text. It is the one matcher of the package: the diagnosis score
// (scoreDiagnosis), the subject terms and the behind-reads check (factReached) all use it (#848).
//
// The boundary rule: case is ignored, and a term matches only where no word character is glued to an end of
// the term that is itself a word character. A word character is a letter, a digit or an underscore. An end
// that is a digit is also glued to a decimal point followed by a digit, so a number is not taken from a longer
// number. An end that is not a word character (a space, a colon, a comma) is a delimiter of its own and takes
// no check. So "256" is in "requests 256 CPUs", `"cpu": "256"` and "256." at the end of a sentence, and not in
// "256Mi", "sha256:", "1256", "2560" or "0.256"; "48Mi" is in "limit: 48Mi," and not in "148Mi"; "cache" is in
// "cache.yaml" and not in "caches".
func termIn(term, text string) bool {
	t, l := []rune(strings.ToLower(term)), []rune(strings.ToLower(text))
	if len(t) == 0 {
		return false
	}
	for i := 0; i+len(t) <= len(l); i++ {
		if !slices.Equal(l[i:i+len(t)], t) {
			continue
		}
		if wordRune(t[0]) && gluedBefore(l, i, unicode.IsDigit(t[0])) {
			continue
		}
		if end := i + len(t); wordRune(t[len(t)-1]) && gluedAfter(l, end, unicode.IsDigit(t[len(t)-1])) {
			continue
		}
		return true
	}
	return false
}

// wordRune reports whether r is a word character for termIn: a letter, a digit or an underscore.
func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// gluedBefore reports whether the match starting at l[i] follows a word character, or, for a match that
// starts with a digit, a decimal point after a digit ("0.5").
func gluedBefore(l []rune, i int, digit bool) bool {
	if i == 0 {
		return false
	}
	if wordRune(l[i-1]) {
		return true
	}
	return digit && l[i-1] == '.' && i >= 2 && unicode.IsDigit(l[i-2])
}

// gluedAfter reports whether the match ending before l[end] is followed by a word character, or, for a match
// that ends with a digit, by a decimal point and a digit ("5.5").
func gluedAfter(l []rune, end int, digit bool) bool {
	if end >= len(l) {
		return false
	}
	if wordRune(l[end]) {
		return true
	}
	return digit && l[end] == '.' && end+1 < len(l) && unicode.IsDigit(l[end+1])
}

// rubricTerms returns every term of every rubric, the escalation scenarios' included, once each.
func rubricTerms(t *testing.T) []string {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	all, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	esc, err := loadEscalationScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	var terms []string
	for _, s := range append(all, esc...) {
		for _, f := range s.rubric.Facts {
			terms = append(terms, f.All...)
			terms = append(terms, f.Any...)
			terms = append(terms, f.Subject...)
			for _, g := range f.AnyOf {
				terms = append(terms, g...)
			}
		}
	}
	slices.Sort(terms)
	terms = slices.Compact(terms)
	if len(terms) == 0 {
		t.Fatal("no rubric terms")
	}
	return terms
}

// Every rubric term matches wherever a diagnosis can write it, with known answers by construction: alone, at
// the start and the end of the text, after and before words, punctuation, quotes and backticks, in any case.
func TestTermInRubricTermsMatchTheirForms(t *testing.T) {
	before := []string{"", "the value ", "(", "`", `"`, "'", "\n", ": ", "=", "/"}
	after := []string{"", " here", ")", "`", `"`, "'", ".", ",", ";", ":", "\n", "/"}
	for _, term := range rubricTerms(t) {
		for _, form := range []string{term, strings.ToLower(term), strings.ToUpper(term)} {
			for _, b := range before {
				for _, a := range after {
					if text := b + form + a; !termIn(term, text) {
						t.Errorf("termIn(%q, %q) = false, want true", term, text)
					}
				}
			}
		}
	}
}

// No rubric term matches where a word character is glued to an end of it that is a word character, nor a
// number glued to a decimal part: answers known by construction ("256Mi", "sha256", "1256", "0.5", "5.5").
func TestTermInRubricTermsRefuseGluedForms(t *testing.T) {
	glue := []string{"x", "7", "_", "Mi"}
	for _, term := range rubricTerms(t) {
		r := []rune(term)
		var texts []string
		if wordRune(r[0]) {
			for _, g := range glue {
				texts = append(texts, "a "+g+term+" b")
			}
			if unicode.IsDigit(r[0]) {
				texts = append(texts, "a 1."+term+" b")
			}
		}
		if wordRune(r[len(r)-1]) {
			for _, g := range glue {
				texts = append(texts, "a "+term+g+" b")
			}
			if unicode.IsDigit(r[len(r)-1]) {
				texts = append(texts, "a "+term+".5 b")
			}
		}
		for _, text := range texts {
			if termIn(term, text) {
				t.Errorf("termIn(%q, %q) = true, want false", term, text)
			}
		}
	}
}

// termIn matches a term on word boundaries without case, so a value glued to a unit or another word is not taken
// for it: hand-picked cases, the near misses of #848 among them. Each term is in every text of in, and in no text
// of out.
func TestTermIn(t *testing.T) {
	for _, c := range []struct {
		term    string
		in, out []string
	}{
		{"256",
			[]string{"the web container requests 256 CPUs", `"cpu": "256"`, "cpu request 256", "256 at the start",
				"(`256`)", "a request of 256."},
			[]string{"a limit of 256Mi", "sha256:00", "image@sha256:256abc", "2560 bytes", "1256 bytes",
				"a share of 0.256", "256.5 CPUs", "256_total"}},
		{"256m", []string{"cpu: 256m"}, []string{"cpu: 256Mi"}},
		{"10.0.0.1",
			[]string{"dial tcp 10.0.0.1:5432: connection refused"},
			[]string{"dial tcp 10.0.0.12:5432", "dial tcp 110.0.0.1:5432"}},
		{"app=web", []string{"selector app=web,tier=front"}, []string{"selector app=webhook"}},
		{"podinfo:6.17.99",
			[]string{"image ghcr.io/stefanprodan/podinfo:6.17.99 not found"},
			[]string{"image podinfo:6.17.990", "image podinfo:6.17.99.1"}},
		{"6.5", []string{"6.5 per replica"}, []string{"16.5 per replica", "6.55 per replica"}},
		{"exit code 1", []string{"Exit Code 1."}, []string{"exited with exit code 137"}},
		{"0 restarts", []string{"0 restarts so far"}, []string{"10 restarts"}},
		{"restart", []string{"restart count 3"}, []string{"restarts: 3"}},
		{"cache", []string{"the key cache.yaml"}, []string{"the caches"}},
		{"DB_POOL_SIZE", []string{"DB_POOL_SIZE=0"}, []string{"DB_POOL_SIZE_MAX=4"}},
		{"liveness", []string{"the liveness probe"}, []string{"livenessProbe failed"}},
		{"not a memory issue", []string{"It is NOT a memory issue."}, []string{"not a memory issues list"}},
		{"/healthz",
			[]string{"GET /healthz returned 404", `"path": "/healthz"`},
			[]string{"/healthzcheck"}},
		{"Insufficient cpu", []string{"3 insufficient CPU."}, nil},
		{"", nil, []string{"anything"}},
	} {
		for _, text := range c.in {
			if !termIn(c.term, text) {
				t.Errorf("termIn(%q, %q) = false, want true", c.term, text)
			}
		}
		for _, text := range c.out {
			if termIn(c.term, text) {
				t.Errorf("termIn(%q, %q) = true, want false", c.term, text)
			}
		}
	}
}

// The rubric facts whose terms were written for substring matching still pass a correct diagnosis in each form
// of the word it may use, now that terms match on word boundaries (#848).
func TestRubricFactsAcceptWordForms(t *testing.T) {
	_, esc := mustEscalationScenarios(t)
	const backlog = "the backlog, the current external metric value (about 13 requests per second) against its " +
		"target (5 per replica)"
	byName := map[string]*scenario{scnOOM: mustLoad(t, scnOOM), scnKedaCeiling: mustLoad(t, scnKedaCeiling)}
	for _, s := range esc {
		byName[s.name] = s
	}
	for _, c := range []struct {
		scenario, fact string
		texts          []string
	}{
		{scnOOM, "the restart count of the web container", []string{
			"The web container restarted 4 times.", "It restarts after each kill.", "restartCount 4",
			"The container is restarting.", "Restart count 4.",
		}},
		{"escalation/probe-path", "the container restarts after the failed probes", []string{
			"The kubelet restarted the container.", "It restarts after three failures.", "Restarting failed container.",
		}},
		{"escalation/probe-path", "the liveness probe fails with HTTP status 404", []string{
			"The liveness probe gets 404.", "livenessProbe: HTTP probe failed with statuscode: 404",
		}},
		{scnKedaCeiling, backlog, []string{
			"The load is 13 requests per second against a target of 5 per replica.",
			"The load is 13 req/s against a target of 5 per replica.",
		}},
	} {
		s := byName[c.scenario]
		if s == nil {
			t.Fatalf("no scenario %s", c.scenario)
		}
		var r rubric
		for _, f := range s.rubric.Facts {
			if f.Fact == c.fact {
				r.Facts = append(r.Facts, f)
			}
		}
		if len(r.Facts) != 1 {
			t.Fatalf("%s has %d facts %q, want 1", c.scenario, len(r.Facts), c.fact)
		}
		for _, text := range c.texts {
			if got := scoreDiagnosis(r, text, subjectText(s.incident)); !got.Pass {
				t.Errorf("%s: %q fails the fact %q: %+v", c.scenario, text, c.fact, got.Facts)
			}
		}
	}
}
