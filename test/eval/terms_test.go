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
// Case is ignored. The boundary rule depends on the side and on how the term ends (decision of 2026-11-17 on
// #848), and an end of the term that is not a letter or a digit (a space, a colon, a brace) takes no check:
//
//   - Left: strict. No letter or digit is glued before the term, nor, before a term that starts with a digit,
//     a decimal point after a digit ("0.256"). A lower-case letter followed by an upper-case start of the
//     term is a boundary ("itemCache" holds "cache"), and underscores glue only when a letter or digit comes
//     before them ("DB_POOL_SIZE" is not in "MY_DB_POOL_SIZE", and is in "_DB_POOL_SIZE_").
//   - Right, a term that ends in a digit: strict. No letter or digit follows, nor a decimal point and a digit
//     ("256.5"), except a plural "s" that ends the word ("503s"). Underscores glue only when a letter or
//     digit follows them.
//   - Right, a term that ends in a letter: open to letters, so the word may go on ("restart" is in
//     "restarted", "96Mi" in "96MiB", "cpu" in "CPUs"), as the substring match was on that side. A digit
//     glues ("app=web" is not in "app=web2"), and underscores glue only when a letter or digit follows them.
//
// So "256" is in "requests 256 CPUs", `"cpu": "256"`, "256-core", "256:" and "256." at the end of a sentence,
// and not in "256Mi", "sha256:", "v256", "2560", "0.256" or "256.5".
func termIn(term, text string) bool {
	t, orig := []rune(term), []rune(text)
	if len(t) == 0 {
		return false
	}
	// Each rune is lowered on its own, so the lowered text keeps the indexes of the original one, whose case
	// the camel-case boundary reads.
	l := make([]rune, len(orig))
	for i, r := range orig {
		l[i] = unicode.ToLower(r)
	}
	for i := range t {
		t[i] = unicode.ToLower(t[i])
	}
	first, last := []rune(term)[0], t[len(t)-1]
	for i := 0; i+len(t) <= len(l); i++ {
		if !slices.Equal(l[i:i+len(t)], t) {
			continue
		}
		if alnum(first) && gluedBefore(orig, i) {
			continue
		}
		if end := i + len(t); alnum(last) && gluedAfter(l, end, unicode.IsDigit(last)) {
			continue
		}
		return true
	}
	return false
}

// alnum reports whether r is a letter or a digit.
func alnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// gluedBefore reports whether the match that starts at text[i], whose first rune is a letter or a digit, is glued
// to what comes before it: a letter or a digit (unless a lower-case letter before an upper-case start), a decimal
// point after a digit before a digit start, or underscores after a letter or a digit.
func gluedBefore(text []rune, i int) bool {
	if i == 0 {
		return false
	}
	prev, start := text[i-1], text[i]
	switch {
	case prev == '_':
		j := i - 1
		for j > 0 && text[j-1] == '_' {
			j--
		}
		return j > 0 && alnum(text[j-1])
	case unicode.IsLower(prev) && unicode.IsUpper(start):
		return false
	case alnum(prev):
		return true
	}
	return unicode.IsDigit(start) && prev == '.' && i >= 2 && unicode.IsDigit(text[i-2])
}

// gluedAfter reports whether the match that ends before l[end], whose last rune is a letter or a digit, is glued
// to what follows it. After a digit: a letter or a digit other than a plural "s" that ends the word, or a
// decimal point and a digit. After a letter: a digit. After either: underscores followed by a letter or a digit.
func gluedAfter(l []rune, end int, digit bool) bool {
	if end >= len(l) {
		return false
	}
	next := l[end]
	switch {
	case next == '_':
		return underscoresGlue(l, end)
	case unicode.IsDigit(next):
		return true
	case !digit:
		return false
	case unicode.IsLetter(next):
		return next != 's' || end+1 < len(l) && (alnum(l[end+1]) || l[end+1] == '_' && underscoresGlue(l, end+1))
	}
	return next == '.' && end+1 < len(l) && unicode.IsDigit(l[end+1])
}

// underscoresGlue reports whether the underscores that start at l[i] are followed by a letter or a digit.
func underscoresGlue(l []rune, i int) bool {
	for i < len(l) && l[i] == '_' {
		i++
	}
	return i < len(l) && alnum(l[i])
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
	before := []string{"", "the value ", "(", "`", `"`, "'", "\n", ": ", "=", "/", "-"}
	after := []string{"", " here", ")", "`", `"`, "'", ".", ",", ";", ":", "\n", "/", "-"}
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

// termForms are the forms a diagnosis may write a term in, by construction (#848): the plural, a unit suffix
// after a term that ends in a letter ("96MiB"), camel case after a lower-case word for a term that starts with a
// letter ("itemCache", "noOOMKills"), Markdown emphasis and code marks.
func termForms(term string) []string {
	r := []rune(term)
	first, last := r[0], r[len(r)-1]
	forms := []string{"*" + term + "*", "_" + term + "_", "**" + term + "**", "__" + term + "__", "`" + term + "`",
		"_" + strings.ToUpper(term) + "_"}
	if alnum(last) {
		forms = append(forms, term+"s", "*"+term+"s*", "`"+term+"s`")
	}
	if unicode.IsLetter(last) {
		forms = append(forms, term+"B", term+"es", term+"ed", term+"Probe", "_"+term+"Count_")
		if unicode.IsLetter(first) {
			forms = append(forms, "no"+strings.ToUpper(term)+"Kills")
		}
	}
	if unicode.IsLetter(first) {
		camel := string(unicode.ToUpper(first)) + string(r[1:])
		forms = append(forms, "item"+camel, "**item"+camel+"**")
	}
	return forms
}

// The probe over every term of every rubric under testdata: each form of termForms, in a sentence, matches.
func TestTermInRubricTermsMatchWordForms(t *testing.T) {
	for _, term := range rubricTerms(t) {
		for _, form := range termForms(term) {
			if text := "The diagnosis names " + form + " here."; !termIn(term, text) {
				t.Errorf("termIn(%q, %q) = false, want true", term, text)
			}
		}
	}
}

// No rubric term matches where it is glued, with answers known by construction: on the left a letter, a digit or
// underscores after a letter or digit, and a decimal point after a digit before a digit start; on the right of a
// term that ends in a digit, a letter or a digit (an "s" that does not end the word included), underscores before
// a letter or digit, or a decimal part; on the right of a term that ends in a letter, a digit or underscores
// before a letter or digit.
func TestTermInRubricTermsRefuseGluedForms(t *testing.T) {
	for _, term := range rubricTerms(t) {
		r := []rune(term)
		lower := strings.ToLower(term)
		var texts []string
		if alnum(r[0]) {
			// Lower case on both sides, so no camel-case boundary.
			for _, g := range []string{"x", "7", "x_", "7__", "mi"} {
				texts = append(texts, "a "+g+lower+" b")
			}
			if unicode.IsDigit(r[0]) {
				texts = append(texts, "a 1."+term+" b")
			}
		}
		switch last := r[len(r)-1]; {
		case unicode.IsDigit(last):
			for _, g := range []string{"x", "7", "Mi", "B", "st", "s7", "s_x", "_x", "__7", ".5"} {
				texts = append(texts, "a "+term+g+" b")
			}
		case unicode.IsLetter(last):
			for _, g := range []string{"7", "_x", "__7"} {
				texts = append(texts, "a "+term+g+" b")
			}
		}
		for _, text := range texts {
			if termIn(term, text) {
				t.Errorf("termIn(%q, %q) = true, want false", term, text)
			}
		}
	}
}

// Each rule of termIn with its true forms and its near misses, hand-picked (#848): each term is in every text of
// in, and in no text of out.
func TestTermIn(t *testing.T) {
	const (
		cache, mi96, mi48    = "cache", "96Mi", "48Mi"
		openUnit, openPhrase = "right letter open, unit", "right letter open, phrase"
		openRight            = "right letter open"
	)
	for _, c := range []struct {
		rule, term string
		in, out    []string
	}{
		{"left strict", cache,
			[]string{"the key cache.yaml", "(cache)", "a cache"},
			[]string{"itemcache", "ITEMCACHE", "7cache", "dcache"}},
		{"left camel case", cache,
			[]string{"the itemCache map", "itemCaches"},
			[]string{"ItemcACHE"}},
		{"left camel case, upper-case term", "OOMKilled",
			[]string{"noOOMKilled", "lastState.terminated.reason: OOMKilled"},
			[]string{"NOOOMKilled", "xoomkilled"}},
		{"left underscore", "DB_POOL_SIZE",
			[]string{"DB_POOL_SIZE=0", "_DB_POOL_SIZE_", "__DB_POOL_SIZE__", "env _DB_POOL_SIZE"},
			[]string{"MY_DB_POOL_SIZE", "MY__DB_POOL_SIZE", "7_DB_POOL_SIZE", "DB_POOL_SIZE_MAX=4"}},
		{"left decimal", "256",
			[]string{"a request of 256.", "requests 256 CPUs", "cpu=256"},
			[]string{"a share of 0.256", "v256", "sha256:00", "1256 bytes", "x_256"}},
		{"right digit strict", "256",
			[]string{`"cpu": "256"`, "cpu request 256", "256 at the start", "(`256`)", "_256_", "**256**",
				"256-core", "256:", "256s of them"},
			[]string{"a limit of 256Mi", "2560 bytes", "256.5 CPUs", "256_total", "256B", "256sec", "256s7",
				"256s_x", "image@sha256:256abc"}},
		{"right digit plural", "503",
			[]string{"returns 503s", "the 503s.", "*503s*"},
			[]string{"5030", "503x", "503ms"}},
		{"right digit plural", "404",
			[]string{"404s from /healthz"},
			[]string{"4040", "404.1"}},
		// A term that ends in a letter is open on the right, so a unit term takes a longer unit too: write the
		// full unit (mi48, not "48M").
		{"right letter open, unit term", "256m", []string{"cpu: 256m", "cpu: 256Mi"}, []string{"cpu: 256m0", "1256m"}},
		{"right digit, address", "10.0.0.1",
			[]string{"dial tcp 10.0.0.1:5432: connection refused"},
			[]string{"dial tcp 10.0.0.12:5432", "dial tcp 110.0.0.1:5432", "10.0.0.1.5"}},
		{"right digit, image tag", "podinfo:6.17.99",
			[]string{"image ghcr.io/stefanprodan/podinfo:6.17.99 not found"},
			[]string{"image podinfo:6.17.990", "image podinfo:6.17.99.1"}},
		{"right digit, decimal term", "6.5", []string{"6.5 per replica"}, []string{"16.5 per replica", "6.55 per replica"}},
		{openUnit, mi96,
			[]string{"a limit of 96MiB", mi96, "96Mi,"},
			[]string{"196Mi", "96Mi5", "96Mi_x"}},
		{openUnit, "64Mi", []string{"64MiB"}, []string{"164Mi"}},
		{openUnit, mi48, []string{"48MiB", "limit: 48Mi,"}, []string{"148Mi"}},
		{"right letter open, word forms", "restart",
			[]string{"restarts: 3", "restarted", "restarting", "restartCount 4", "_restart_"},
			[]string{"restart7", "restart_count"}},
		{openPhrase, "no oom", []string{"no OOMKills", "no OOM"}, []string{"no oom7"}},
		{openPhrase, "insufficient cpu", []string{"3 Insufficient CPUs."}, nil},
		{openPhrase, "readiness probe", []string{"readiness probes failed"}, nil},
		{"right letter open, hyphen", "dry-run", []string{"two dry-runs failed"}, []string{"xdry-run"}},
		{openRight, "liveness", []string{"the liveness probe", "livenessProbe failed"}, nil},
		{openRight, "app=web", []string{"selector app=web,tier=front", "app=webhook"},
			[]string{"app=web2"}},
		{openRight, "/healthz",
			[]string{"GET /healthz returned 404", `"path": "/healthz"`, "/healthzcheck"},
			[]string{"/healthz2"}},
		{"emphasis", "OOMKilled", []string{"_OOMKilled_", "**OOMKilled**", "*OOMKilled*", "`OOMKilled`"}, nil},
		{"multi-word", "exit code 1", []string{"Exit Code 1."}, []string{"exited with exit code 137"}},
		{"multi-word", "0 restarts", []string{"0 restarts so far"}, []string{"10 restarts"}},
		{"empty term", "", nil, []string{"anything"}},
	} {
		for _, text := range c.in {
			if !termIn(c.term, text) {
				t.Errorf("%s: termIn(%q, %q) = false, want true", c.rule, c.term, text)
			}
		}
		for _, text := range c.out {
			if termIn(c.term, text) {
				t.Errorf("%s: termIn(%q, %q) = true, want false", c.rule, c.term, text)
			}
		}
	}
}

// The rubric facts whose base terms were written for substring matching ("restart", "liveness", " 13 req") still
// pass a correct diagnosis in each form of the word it may use, now that terms match on word boundaries (#848).
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
