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

// Package redact removes secrets, tokens, connection strings and personal data from text before it is
// stored or sent to a model (design 10.3, ADR-0015). Callers run every piece of cluster data through one
// [Redactor] at the boundary; the package does not decide where that boundary is.
//
// Each match is replaced by a marker that names its kind, such as "[REDACTED:jwt]", so a reader still knows
// what was there. The output is never longer than the input: when a marker does not fit in the matched
// text, the first shorter one that fits is used: "[jwt]", then "[REDACTED]", then "***", then one "*" per
// byte (only for matches of one or two bytes). Matching uses RE2 (package regexp), so the time
// taken is linear in the input and no input can make the redactor backtrack or allocate without bound.
//
// Redaction is best effort and errs on the side of removing too much: an IP address or an email in a log
// line is removed even when it is harmless, and so are the user name of any URL ("ssh://git@host"), long
// runs that mix letters and digits, such as Git commit hashes and container IDs, 32 characters or more of
// mixed-case base64 that slashes cut short, or that has no slash and no digit (also a base64 blob that is not
// a secret, and at most one short part of up to five characters on each side of it in a path), and every
// scalar value under a secret-named YAML key ("credentials:\n  user: admin" loses "admin"), the block under a
// secret key and a block scalar indicator wherever the key stands ("msg: password: |", also inside a JSON or
// logfmt string, with no blank or a Unicode space at the colon, after "---" lines or less indented than the
// key's line), and the rest of the line after such an indicator when a block was flattened onto it, inside a
// flow collection up to its closing bracket (#840),
// and, read on one canonical view of the text (view.go, #845): the block under each secret key and indicator
// of a line that holds several, with every Unicode space, control and format character and invalid byte as
// indentation and LF, CRLF and a lone CR as line ends, a flow value up to the bracket that closes it after any
// brackets inside the secret, a block that a comment line at column 0 interrupts, a value in a string of a
// sequence after the string that holds its key ("password:", "--password=", also with blank strings between,
// the colon in a string of its own, or a flag after other words; decided once for a list, for a sequence
// written in a text and for a tool result laid out as one text, seqValues and Pieces in layout.go), an
// environment entry whose value comes before its name, in flow and in block form, and a "- key:" naming a
// secret with its "values" (a selector); and, read on the folded view of the text (fold.go), a key name with an invisible character
// inside it or before its colon, a key followed by a colon look-alike of Unicode's confusables list, an explicit
// YAML key ("? password" over ": x", also "password" alone below "?" or "? |"), and a key followed by a
// separator of other characters ("userPassword:: x", "password => x", "token\x00: x"),
// the value after a secret-named command-line flag ("--client-secret x", "-password x", "--sas x", also across line ends, in
// argument lists), however it reads or starts with, another flag included ("--use-token --verbose" loses
// "--verbose", "--secret db-creds" loses "db-creds"), and the value after a key
// name of two or more words, joined by "_", "-" or in camelCase, and a blank ("aws_secret_access_key x",
// "ClientSecret x"), only when the whole value is an opaque token: 16 or more characters, every one of them
// from "A-Z a-z 0-9 / + = _ -", and at least one digit or one of "/+=" ("client_secret is missing" keeps "is",
// and a password such as "hunter2" after a blank stays, a known leak). After a flag or such a name the value
// is the text between its quotes on one line, or else one token that ends at whitespace, the line end, or a
// quote character (a plain or backslash-escaped quote), and at nothing else, not at ",", "]" or "}"
// (keyNameValue). The separator between a flag and its value is whitespace including real line ends, a
// backslash directly followed by a real line end, a YAML list dash only at the start of a line after
// indentation, a comma only directly after the flag name or its closing quote, and that quote, plain or
// backslash-escaped; nothing else, so literal "\n" pairs, a comma after whitespace and a bare "-" start the
// value. A
// match that starts inside a value already taken is skipped, except a secret-named flag, which is judged on
// its own. Values that look like references rather than secrets, such
// as "secretName: db-creds", are kept, because the name of a missing Secret is what a diagnosis needs, and so
// is the hex of an image digest.
//
// Where a rule would also match prose, it asks the value to look like a credential: the word after "Basic"
// or "Bearer", and the value of the short keys "pass", "auth", "sig" and "sas" before a colon, on their own
// ("pass: 12 fail: 0") or after one of a few prose words and a hyphen ("pre-auth: ok", "two-pass: true"), inline or
// in a block on the following lines, count only with a value of at least 8 bytes that mixes ASCII letters
// with digits or symbols (blanks too when the value is quoted, as in a passphrase; 16 bytes of anything after
// Basic or Bearer, or base64 of "user:password" after Basic). So "Bearer abc12" and "auth: hunter2" pass
// through, while any other hyphenated key ("db-pass", "proxy-auth") always counts. Any byte that is not an
// ASCII letter counts as a symbol, so "auth: déjàvuxx" and "auth: unauthorized," are redacted, and so is
// "basic internationalization": over-redaction kept by decision (#194), because these cannot be told apart
// from a password or token.
//
// Known gaps: JSON escaped more than three times, a Kubernetes environment entry whose "value" comes more
// than 16 lines after its "name", a base64 secret cut short by slashes with no secret key name
// before it whose parts read as words or acronyms (whole or in part, see base64Windows), a secret after a
// one-word key name and a blank without a dash ("set secret x", which only the long-token rule judges,
// because "kubectl get secret foo" has the same shape), after a single-letter flag ("-p x"), after a key or
// flag name written with Unicode escapes, or in a here-document or here-string, the part of a value after a
// key name past the bound of the extent rule (maxValueExtent, extent.go), a quoted piece glued to an unquoted
// value that does not close on its line, a backslash and a letter or digit glued to a quoted piece after a
// flag, a field after a comma glued to a quoted piece after a key name that is no flag, the part of an
// unquoted value after a quote inside it, the words after the first of a block sequence item after a quoted
// flag item or of an item after a flag item that opens with a backslash or "$(", a quoted scalar after a
// verbatim tag in an array, a value after a compound name and a blank that is no opaque token, a value on
// the lines below a secret key after other text on its line with nothing after its colon, the lines after the
// first of a scalar over several lines under a bare short key, a value after a secret key and blanks alone
// with no colon, "=" or arrow ("userPassword x", read as prose like "password reset failed"), a value after a
// separator with more than maxSepRun marks, controls or invisible characters (blanks do not count), after a
// key and a separator of "|", "~", ">", "-" or a dash, or after a key, a line break and then the separator
// (#859 looks for such values by their shape), and a secret in a format no rule knows (TestKeyNameKnownLeaks).
// It removes too much in a few known places: the line after a value that a lone CR ends, the text after "->",
// "=>" or "==" behind a key word, the name of a secret key that stands as another secret key's value, and a
// URL in angle brackets with a port before a link with an "@".
//
// Every rule reads the text as it was given and reports spans of it; the spans of all rules are merged and
// replaced once ([Redactor.String]), so no rule reads another's output and no pass comes before another. The
// rules of #840 and before are kept as they were, and the view rules only add spans to theirs, so nothing
// they removed comes back. Callers redact text once, as it was read, before they replace a character, join a
// line, escape or cut it, and never redact what they made of it (#845). A caller that holds a sequence of
// strings (a container's arguments, the name and value of a metric label) redacts it with [Redactor.Strings],
// which takes the decision String takes on the strings joined by line breaks and so also sees a key in one
// string and its value in a later one. The folded view is a second reading of the same text, not of a rule's
// output: its matches count on the bytes they were folded from. So is the controls view (controls.go, #852): every
// text is also read without its control characters, and what that reading finds is added, so a key or a token a
// control splits is hidden; the reading of the text as it is runs first and unchanged, so it only adds. The short
// entry points of the collector (shortentry.go) do not read it.
package redact

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"iter"
	"math"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/released"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// Kinds of redacted data, used in markers such as "[REDACTED:jwt]".
const (
	KindPrivateKey       = "private-key"
	KindJWT              = "jwt"
	KindToken            = "token"
	KindConnectionString = "connection-string"
	KindCredential       = "credential"
	KindEmail            = "email"
	KindIP               = "ip"
)

// Pattern is an extra rule supplied by the operator, for data the built-in rules do not know about.
type Pattern struct {
	// Kind names the data in the marker. Lower-case letters, digits and hyphens, starting with a letter,
	// at most 32 characters.
	Kind string
	// Expr is an RE2 regular expression. If it has capture groups, only the text of the groups that
	// matched is redacted; otherwise the whole match is.
	Expr string
}

// spanFunc is the span function of a rule (rule.span).
type spanFunc func(wm *meter.Meter, s string, m []int, prevEnd int) (start, end int, ok bool)

// spansFunc is the spans function of a rule (rule.spans).
type spansFunc func(wm *meter.Meter, s string, m []int, prevEnd int, add func(start, end int)) int

// rule is one compiled pattern. Spans of the listed groups are redacted, or the whole match when groups is
// empty. span, when set, replaces groups: it receives the submatches and the end of the previous span it returned for this rule, and
// returns the byte range to redact, or ok false to leave the match alone.
type rule struct {
	kind   string
	re     *regexp.Regexp
	groups []int
	span   spanFunc
	// spans, when set, replaces span for a rule that redacts several ranges per match: it calls add for
	// each and returns the end of what it read, which later matches of the rule before it skip.
	spans spansFunc
	// spansFor, when set, replaces spans with the function it returns, made anew for each pass over a text.
	spansFor func() spansFunc
	// spanFor, when set, replaces span in the same way.
	spanFor func() spanFunc
	// scan, when set, replaces re and the functions above: a rule that reads the whole text on the canonical
	// view (view.go) and calls add for each range to redact.
	scan func(wm *meter.Meter, s string, add func(start, end int))
	// win is re with its alphabet, read by windows (windows.go).
	win *windowed
}

// each calls f with each match of the rule's expression in s, in order, as FindAllStringSubmatchIndex(s, -1) finds
// them, read by windows when the rule has them (New) and runs holds the long windows of s (longRuns of the
// redactor's wins). f takes each match as it is found, so the matches of the whole text are not held at once
// (#1087).
func (ru *rule) each(wm *meter.Meter, s string, runs [][]longRun, ids *windowIDs, f func(m []int)) {
	all := func(m []int) bool { f(m); return true }
	if ru.win == nil || runs == nil {
		if ru.win != nil {
			eachCharged(wm, ru.win.expr, s, all)
			return
		}
		for _, m := range ru.re.FindAllStringSubmatchIndex(counted(wm, s), -1) {
			f(m)
		}
		return
	}
	ru.win.eachIn(wm, s, runs[ru.win.idx], ids, all)
}

// Redactor replaces sensitive data in text. It is safe for concurrent use.
type Redactor struct {
	rules []rule
	// before is the redactor of the release before, with the same extra patterns: the reading kept in package
	// released (before.go). nil only for a redactor made of some rules alone, in a test.
	before *released.Redactor
	// wins are the expressions of the rules with their alphabets, read by windows (windows.go).
	wins []*windowed
	// lowered and workBudget: the work budget of every entry is at most workBudget steps (WithWorkBudget, tests
	// only). False on every redactor New returns.
	lowered    bool
	workBudget int64
	// ctx ends the wait of this redactor's entries for the heap limit of the redactions running at once
	// (WithContext); nil on every redactor New returns.
	ctx context.Context
}

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// New returns a redactor with the built-in rules plus the given extra patterns. It fails if a pattern has
// an invalid kind or expression.
func New(extra ...Pattern) (*Redactor, error) {
	rules := builtinRules(nil)
	for i, p := range extra {
		tick(nil, 1)
		if !kindPattern.MatchString(p.Kind) {
			return nil, fmt.Errorf("extra pattern %d: kind %q must match %s", i, p.Kind, kindPattern)
		}
		re, err := regexp.Compile(p.Expr)
		if err != nil {
			return nil, fmt.Errorf("extra pattern %d (%s): %w", i, p.Kind, err)
		}
		if _, err := meter.CompileExpr(re); err != nil {
			return nil, fmt.Errorf("extra pattern %d (%s): %w", i, p.Kind, err)
		}
		r := rule{kind: p.Kind, re: re}
		for g := 1; g <= re.NumSubexp(); g++ {
			tick(nil, 1)
			r.groups = append(r.groups, g)
		}
		rules = append(rules, r)
	}
	ps := make([]released.Pattern, len(extra))
	for i, p := range extra {
		tick(nil, 1)
		ps[i] = released.Pattern{Kind: p.Kind, Expr: p.Expr}
	}
	before, err := released.New(ps...)
	if err != nil {
		return nil, fmt.Errorf("the reading of the release before: %w", err)
	}
	r := &Redactor{rules: rules, before: before}
	for i := range rules {
		tick(nil, 1)
		if rules[i].re != nil {
			rules[i].win = newWindowed(nil, rules[i].re)
			rules[i].win.idx = len(r.wins)
			r.wins = append(r.wins, rules[i].win)
		}
	}
	return r, nil
}

var defaultRedactor = func() *Redactor {
	r, err := New()
	if err != nil {
		panic(err) // the built-in rules compile, as the tests show
	}
	return r
}()

// String redacts s with the built-in rules.
func String(s string) string {
	return defaultRedactor.String(s)
}

// StringChecked is String with a flag: ok is false when the entry is withheld (budget.go), and the text is then
// Withheld.
func StringChecked(s string) (out string, ok bool) {
	return defaultRedactor.StringChecked(s)
}

// StringKeeping is String, except that a match of a rule that lies wholly inside whole occurrences of strings
// in keep stays as it is. keep holds values the caller has verified are not secrets (an object name it read back
// from the API server, for example), never text from a model, a user or the cluster; s may be any text, a
// template the caller renders or a model's answer. An occurrence counts only when the bytes on both sides are not
// token characters (ASCII letters, digits and "+/_-=."), so a kept string inside a longer run is redacted with
// that run. The rules run over the whole of s, so a match that holds a kept string and anything else (a
// password's value that starts with a kept name, for example) is redacted whole, kept string included. Empty
// strings in keep are ignored.
func StringKeeping(s string, keep ...string) string {
	return defaultRedactor.StringKeeping(s, keep...)
}

// StringKeepingChecked is the package's StringKeepingChecked with the built-in rules.
func StringKeepingChecked(s string, keep ...string) (out string, ok bool) {
	return defaultRedactor.StringKeepingChecked(s, keep...)
}

// Clean is the package's Clean with the built-in rules.
func Clean(s string, keep ...string) bool {
	return defaultRedactor.Clean(s, keep...)
}

// StringKeeping is the package's StringKeeping with r's rules.
func (r *Redactor) StringKeeping(s string, keep ...string) string {
	out, _ := r.StringKeepingChecked(s, keep...)
	return out
}

// StringKeepingChecked is StringKeeping with a flag: ok is false when it withholds s (budget.go), and out is then
// Withheld. A caller that shows a withheld value apart (the body of a pull request) reads the flag, never the text.
func (r *Redactor) StringKeepingChecked(s string, keep ...string) (out string, ok bool) {
	return meteredText(r, len(s), func(wm *meter.Meter) string { return r.stringKeeping(wm, s, keep) })
}

// stringKeeping is StringKeeping on the budget wm.
func (r *Redactor) stringKeeping(wm *meter.Meter, s string, keep []string) string {
	spans := r.spans(wm, s)
	if len(spans) == 0 {
		return s
	}
	kept := wholeOccurrences(wm, s, keep)
	if len(kept) == 0 {
		return render(wm, s, spans)
	}
	out := spans[:0]
	for _, sp := range spans {
		tick(wm, 1)
		if !covered(wm, sp, kept) {
			out = append(out, sp)
		}
	}
	return render(wm, s, out)
}

// Clean reports whether s holds nothing the rules would redact, besides matches that lie wholly inside whole
// occurrences of strings in keep (as [StringKeeping] keeps them) or inside markers this redactor writes. It is
// the check of text that was redacted already: redaction is not idempotent, as a marker in the place of a value
// can be taken for a value again ("token=[REDACTED:token]"), so comparing s with its redaction would refuse
// such text.
//
// An entry it withholds (package budget.go) is not clean: Clean returns false.
func (r *Redactor) Clean(s string, keep ...string) bool {
	clean, ok := metered(r, len(s), func(wm *meter.Meter) bool { return r.clean(wm, s, keep) })
	return ok && clean
}

// ErrWait is the error of [Redactor.CleanChecked] when the check did not run: its wait for the limit on the heap
// of the redactions running at once ended first (heaplimit.go). The same text may be checked again later.
var ErrWait = errors.New("redaction did not run: its wait for the heap limit ended")

// CleanChecked is Clean that tells a check that did not run from one that ran: it returns ErrWait, and false,
// when the wait for the heap limit ended before the check could run. Any other withheld entry (longer than
// MaxEntry, or over its work budget) is not clean, with no error, as in Clean: checking it again decides the same.
func (r *Redactor) CleanChecked(s string, keep ...string) (bool, error) {
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background() // the wait ends after maxHeapWait
	}
	clean, why := meteredWhy(ctx, len(s), r.budgetOf(len(s)), func(wm *meter.Meter) bool { return r.clean(wm, s, keep) })
	if why == ReasonWait {
		return false, ErrWait
	}
	return why == "" && clean, nil
}

// clean is Clean on the budget wm.
func (r *Redactor) clean(wm *meter.Meter, s string, keep []string) bool {
	spans := r.spans(wm, s)
	if len(spans) == 0 {
		return true
	}
	ok := mergeRanges(wm, append(wholeOccurrences(wm, s, keep), r.markers(wm, s)...))
	for _, sp := range spans {
		tick(wm, 1)
		if !covered(wm, sp, ok) {
			return false
		}
	}
	return true
}

// markerPattern matches the shapes of the markers [marker] writes; markers keeps only the exact ones.
var markerPattern = regexp.MustCompile(`\[REDACTED(?::[a-z][a-z0-9-]{0,31})?\]|\[[a-z][a-z0-9-]{0,31}\]|\*+`)

// MarkerShapes returns the byte ranges of s that have the shape of a marker a redactor writes, of any kind,
// in order and not overlapping. It is a superset of the markers ("[info]" and a run of "*" are included):
// a caller that must not cut inside a marker (the context builder's cut of a value, #796) loses nothing by
// also keeping such text whole.
func MarkerShapes(s string) [][]int { return markerPattern.FindAllStringIndex(s, -1) }

// markers returns the byte ranges of the markers of r's rules in s, in order and not overlapping: a long or
// short form naming a kind of one of r's rules, "[REDACTED]", the withheld marker [Withheld], or a run of at
// most three "*" (the longest [marker] writes). A longer run of "*" or a marker naming another kind is not one this redactor wrote, so
// Clean does not take it as clean.
func (r *Redactor) markers(wm *meter.Meter, s string) []span {
	var out []span
	for _, m := range markerPattern.FindAllStringIndex(s, -1) {
		tick(wm, 1)
		if r.isMarker(wm, s[m[0]:m[1]]) {
			out = append(out, span{start: m[0], end: m[1]})
		}
	}
	return out
}

// isMarker reports whether text, a match of markerPattern, is a marker [marker] writes for one of r's rules.
func (r *Redactor) isMarker(wm *meter.Meter, text string) bool {
	switch {
	case text[0] == '*':
		return len(text) <= len("***")
	case text == "[REDACTED]", text == Withheld:
		// Withheld is the marker of a whole entry the budget withheld (budget.go), which a caller that keeps the
		// text form writes in place of a value: no rule has its kind, so only this exact long form is taken.
		return true
	case strings.HasPrefix(text, "[REDACTED:"):
		return r.hasKind(wm, text[len("[REDACTED:"):len(text)-1])
	default:
		return r.hasKind(wm, text[1:len(text)-1])
	}
}

// hasKind reports whether one of r's rules has the kind.
func (r *Redactor) hasKind(wm *meter.Meter, kind string) bool {
	for _, ru := range r.rules {
		tick(wm, 1)
		if ru.kind == kind {
			return true
		}
	}
	return false
}

// mergeRanges sorts ranges by start and joins the ones that overlap or touch.
func mergeRanges(wm *meter.Meter, rs []span) []span {
	sortFunc(wm, rs, func(a, b span) int { return a.start - b.start })
	out := rs[:0]
	for _, x := range rs {
		tick(wm, 1)
		if n := len(out); n > 0 && x.start <= out[n-1].end {
			out[n-1].end = max(out[n-1].end, x.end)
			continue
		}
		out = append(out, x)
	}
	return out
}

// ReplaceWhole returns s with each whole occurrence of a key of words replaced by its word. An occurrence is
// whole as [Redactor.Clean] and [StringKeeping] take a kept value: no letter, digit or one of "+/_-=." on
// either side. Occurrences are found from the left, the longest key first where two start at the same offset,
// and do not overlap, so the text replaced is exactly the text Clean would keep for the same keys.
func ReplaceWhole(s string, words map[string]string) string {
	keys := make([]string, 0, len(words))
	for k := range words {
		tick(nil, 1)
		keys = append(keys, k)
	}
	occ := wholeOccurrences(nil, s, keys)
	if len(occ) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	pos := 0
	for _, o := range occ {
		tick(nil, 1)
		b.WriteString(s[pos:o.start])
		b.WriteString(words[s[o.start:o.end]])
		pos = o.end
	}
	b.WriteString(s[pos:])
	return b.String()
}

// wholeOccurrences returns the byte ranges of the whole occurrences in s of the non-empty strings of keep, in
// order and not overlapping (the longest one where two start at the same offset).
func wholeOccurrences(wm *meter.Meter, s string, keep []string) []span {
	var occ []span
	for pos := 0; pos < len(s); {
		tick(wm, 1)
		at, n := firstWhole(wm, s[pos:], keep)
		if at < 0 {
			break
		}
		occ = append(occ, span{start: pos + at, end: pos + at + n})
		pos += at + n
	}
	return occ
}

// covered reports whether every byte of sp lies inside the sorted, disjoint ranges of occ.
func covered(wm *meter.Meter, sp span, occ []span) bool {
	pos := sp.start
	for _, o := range occ {
		tick(wm, 1)
		if o.end <= pos {
			continue
		}
		if o.start > pos {
			return false
		}
		pos = o.end
		if pos >= sp.end {
			return true
		}
	}
	return false
}

// firstWhole returns the offset and length of the first whole occurrence in s of a non-empty string of keep
// (the longest one on a tie), or -1.
func firstWhole(wm *meter.Meter, s string, keep []string) (int, int) {
	best, bestLen := -1, 0
	for _, k := range keep {
		tick(wm, 1)
		if k == "" {
			continue
		}
		for from := 0; from <= len(s)-len(k); {
			tick(wm, 1)
			i := index(wm, s[from:], k)
			if i < 0 {
				break
			}
			i += from
			if (i == 0 || !isTokenByte(wm, s[i-1])) && (i+len(k) == len(s) || !isTokenByte(wm, s[i+len(k)])) {
				if best < 0 || i < best || (i == best && len(k) > bestLen) {
					best, bestLen = i, len(k)
				}
				break
			}
			from = i + 1
		}
	}
	return best, bestLen
}

// isTokenByte reports whether c can be part of a token run or an object name.
func isTokenByte(wm *meter.Meter, c byte) bool {
	return isAlnum(wm, c) || strings.IndexByte("+/_-=.", c) >= 0
}

// span is a byte range of the input to replace, with the kind of the rule that found it.
type span struct {
	start, end int
	kind       string
	rule       int
	// named is, for a range mergeSpans merged, the end of the match that names it, before the range grew: merged
	// again with more matches on the same text, the range is named as if all were merged at once (#1087). 0 for a
	// match not merged yet, which names by its own end; a caller that moves a span to other bytes clears it.
	named int32
}

// nameEnd is the end mergeSpans orders sp by on a tie of start and rule: the end of the match that names it.
func (sp span) nameEnd() int {
	if sp.named > 0 {
		return int(sp.named)
	}
	return sp.end
}

// String returns s with every match of every rule replaced by a marker. Overlapping matches are merged
// into one, named after the match that starts first (the first rule, on a tie).
// An entry it withholds (budget.go) is Withheld, as text; [Redactor.StringChecked] says so with a flag.
func (r *Redactor) String(s string) string {
	out, _ := r.StringChecked(s)
	return out
}

// StringChecked is String with a flag: ok is false when the entry is withheld, and the text is then Withheld.
func (r *Redactor) StringChecked(s string) (out string, ok bool) {
	return meteredText(r, len(s), func(wm *meter.Meter) string { return render(wm, s, r.spans(wm, s)) })
}

// spans returns the matches of every rule in s, sorted, with overlapping matches merged into one named after
// the match that starts first (the first rule, on a tie), and what String of the release before hid in s
// (releasedString), as one more reading. The rules read s, and
// also its folded view when it has one (foldView: a key with an invisible character inside it, a colon
// look-alike, an explicit key, a separator of other characters), whose matches count on the bytes of s they were
// folded from. A merged range takes the invisible characters right after it.
//
// When s holds a control character, the matches of its controls view that these readings do not already hold are
// added (addControlsView, controls.go, #852).
func (r *Redactor) spans(wm *meter.Meter, s string) []span {
	l := r.mainMatches(wm, s)
	r.addControlsView(wm, s, l)
	return settle(wm, s, l.spans(wm))
}

// spansAsMain is spans without the controls view: the readings of origin/main alone, which the short entry points
// take, so that a fingerprint, a symptom and a module's evidence value are decided as they were before #852
// (decision of 2026-11-22, ADR-0039: a fingerprint is a function of the event).
func (r *Redactor) spansAsMain(wm *meter.Meter, s string) []span {
	return settle(wm, s, r.mainMatches(wm, s).spans(wm))
}

// mainMatches returns the matches of the rules in s and in its folded view, and what String of the release before
// hid in s, in one list.
func (r *Redactor) mainMatches(wm *meter.Meter, s string) *matchList {
	l := r.newMatchList()
	r.eachRuleMatch(wm, s, &windowIDs{}, func(start, end, rule int) { l.add(wm, start, end, rule) })
	r.releasedStringInto(wm, s, false, nil, l)
	return l
}

// ownSpans is spans without the reading of the release before: the rules of this package alone, as Pieces reads
// each piece (it applies the reading of the release before as that release's tools did, releasedPieces).
func (r *Redactor) ownSpans(wm *meter.Meter, s string) []span {
	return r.ownSpansWith(wm, s, &windowIDs{})
}

// ownSpansWith is ownSpans with the long windows read before in ids.
func (r *Redactor) ownSpansWith(wm *meter.Meter, s string, ids *windowIDs) []span {
	return settle(wm, s, r.ruleMatchesWith(wm, s, ids))
}

// settle merges matches found on s (mergeSpans) and gives each merged range the invisible characters right
// after it.
func settle(wm *meter.Meter, s string, matches []span) []span {
	merged := mergeSpans(wm, matches)
	for i := range merged {
		tick(wm, 1)
		// Invisible characters right after a match belong to it (#845, fourth round): the folded view drops
		// them, and kept apart from the replacement they would be the one visible trace of where it ended. The
		// merged ranges are disjoint, so each byte is read at most once here.
		limit := len(s)
		if i+1 < len(merged) {
			limit = merged[i+1].start
		}
		for merged[i].end < limit {
			tick(wm, 1)
			n := invisibleLen(wm, s, merged[i].end)
			if n == 0 {
				break
			}
			merged[i].end += n
		}
	}
	return merged
}

// ruleMatches returns the matches of every rule in s and in its folded view (mapped to the bytes of s), each with
// the kind of the rule that found it, sorted and merged as mergeSpans merges them (matchList).
func (r *Redactor) ruleMatches(wm *meter.Meter, s string) []span {
	return r.ruleMatchesWith(wm, s, &windowIDs{})
}

// ruleMatchesWith is ruleMatches with the long windows read before in ids (windows.go): the reads of one decision
// share them, so a value read alone and again in its answer is read once by each expression.
//
// When s holds a control character ([Control]), the matches of the same reading of s without its controls (its
// controls view, controls.go, #852) are added, each counted on the bytes of s it was read from: the reading of s
// as it is runs first and unchanged, so the controls view only adds to what is hidden (addControlsView).
func (r *Redactor) ruleMatchesWith(wm *meter.Meter, s string, ids *windowIDs) []span {
	l := r.newMatchList()
	r.eachRuleMatch(wm, s, ids, func(start, end, rule int) { l.add(wm, start, end, rule) })
	r.addControlsView(wm, s, l)
	return l.spans(wm)
}

// eachRuleMatch calls add with each match of every rule in s and in its folded view (mapped to the bytes of s as
// it is found), with the index of its rule, in no order.
func (r *Redactor) eachRuleMatch(wm *meter.Meter, s string, ids *windowIDs, add func(start, end, rule int)) {
	r.ruleSpans(wm, s, false, ids, add)
	if f, ok := foldView(wm, s); ok {
		r.ruleSpans(wm, f.text, true, ids, func(start, end, rule int) {
			tick(wm, 1)
			add(f.from(wm, start), f.to(wm, end-1), rule)
		})
	}
}

// mergeSpans sorts spans and merges the overlapping ones into one named after the span that starts first (the
// first rule, on a tie).
func mergeSpans(wm *meter.Meter, spans []span) []span {
	if len(spans) == 0 {
		return nil
	}
	sortFunc(wm, spans, func(a, b span) int {
		if a.start != b.start {
			return a.start - b.start
		}
		if a.rule != b.rule {
			return a.rule - b.rule
		}
		return b.nameEnd() - a.nameEnd()
	})

	merged := spans[:0]
	for i := 0; i < len(spans); {
		tick(wm, 1)
		cur := spans[i]
		cur.named = int32(cur.nameEnd()) // #nosec G115 -- an entry is at most MaxEntry bytes, an answer six times that
		for i++; i < len(spans) && spans[i].start < cur.end; i++ {
			tick(wm, 1)
			cur.end = max(cur.end, spans[i].end)
		}
		merged = append(merged, cur)
	}
	return merged
}

// ruleSpans calls emit with each match of every rule in s, with the index of its rule, in no order; folded says s
// is a folded view.
func (r *Redactor) ruleSpans(wm *meter.Meter, s string, folded bool, ids *windowIDs, emit func(start, end, rule int)) {
	var runs [][]longRun
	if len(s) >= windowMinText {
		runs = longRuns(wm, s, r.wins)
	}
	ids.newText(wm)
	for i, ru := range r.rules {
		tick(wm, 1)
		if folded && ru.kind == KindIP {
			// The folded view reads keys and separators; an address is read as written ("：：" is no "::").
			continue
		}
		prevEnd := 0
		add := func(start, end int) {
			if end > start {
				emit(start, end, i)
			}
		}
		if ru.scan != nil {
			ru.scan(wm, s, add)
			continue
		}
		spanFn, oneSpan := ru.spans, ru.span
		if ru.spansFor != nil {
			spanFn = ru.spansFor()
		}
		if ru.spanFor != nil {
			oneSpan = ru.spanFor()
		}
		ru.each(wm, s, runs, ids, func(m []int) {
			tick(wm, 1)
			if spanFn != nil {
				prevEnd = spanFn(wm, s, m, prevEnd, add)
				return
			}
			if oneSpan != nil {
				if start, end, ok := oneSpan(wm, s, m, prevEnd); ok && end > start {
					emit(start, end, i)
					prevEnd = end
				}
				return
			}
			if len(ru.groups) == 0 {
				add(m[0], m[1])
				return
			}
			for _, g := range ru.groups {
				tick(wm, 1)
				if 2*g+1 < len(m) && m[2*g] >= 0 {
					add(m[2*g], m[2*g+1])
				}
			}
		})
	}
}

// Replace redacts s with the built-in rules as [String] does, but writes repl(kind, text) for each match in
// place of its marker and between(text) for the text between matches. A caller that rewrites text before it
// shows it (replaces characters, joins lines, cuts) uses it to redact the text as it was read first, and to
// rewrite only what is left.
func Replace(s string, repl func(kind, text string) string, between func(string) string) string {
	return defaultRedactor.Replace(s, repl, between)
}

// ReplaceChecked is Replace with a flag: ok is false when the entry is withheld, and the text is then Withheld.
func ReplaceChecked(s string, repl func(kind, text string) string, between func(string) string) (out string, ok bool) {
	return defaultRedactor.ReplaceChecked(s, repl, between)
}

// Replace is the package's Replace with r's rules.
// An entry it withholds (budget.go) is Withheld, with neither repl nor between called on its text.
func (r *Redactor) Replace(s string, repl func(kind, text string) string, between func(string) string) string {
	out, _ := r.ReplaceChecked(s, repl, between)
	return out
}

// ReplaceChecked is Replace with a flag: ok is false when the entry is withheld, and the text is then Withheld.
func (r *Redactor) ReplaceChecked(s string, repl func(kind, text string) string, between func(string) string) (out string, ok bool) {
	spans, ok := metered(r, len(s), func(wm *meter.Meter) []span { return r.spans(wm, s) })
	if !ok {
		return Withheld, false
	}
	return r.replaceSpans(unstopped(), s, spans, repl, between), true
}

// replaceSpans is Replace with the sorted, disjoint spans given.
func (r *Redactor) replaceSpans(wm *meter.Meter, s string, spans []span, repl func(kind, text string) string, between func(string) string) string {
	var b strings.Builder
	b.Grow(len(s))
	pos := 0
	for _, cur := range spans {
		tick(wm, 1)
		b.WriteString(between(s[pos:cur.start]))
		b.WriteString(repl(cur.kind, s[cur.start:cur.end]))
		pos = cur.end
	}
	b.WriteString(between(s[pos:]))
	return b.String()
}

// Marker returns the marker a redactor writes for n bytes of the given kind, never longer than n. A caller
// of [Redactor.Replace] that keeps only part of a match writes the marker of that part.
func Marker(kind string, n int) string { return marker(nil, kind, n) }

// render returns s with each of the sorted, disjoint spans replaced by its marker.
func render(wm *meter.Meter, s string, spans []span) string {
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	pos := 0
	for _, cur := range spans {
		tick(wm, 1)
		b.WriteString(s[pos:cur.start])
		b.WriteString(marker(wm, cur.kind, cur.end-cur.start))
		pos = cur.end
	}
	b.WriteString(s[pos:])
	return b.String()
}

// marker returns the replacement for n bytes of the given kind. It is never longer than n, so redaction
// never grows its input.
func marker(wm *meter.Meter, kind string, n int) string { return markerAt(wm, kind, n, 0) }

// markerLevels is the number of levels of markerAt.
const markerLevels = 4

// markerAt is marker starting at a shorter form: level 0 is marker, level 1 starts at "[kind]", level 2 at
// "[REDACTED]" and level 3 at "***". An answer bounded in size takes the first level that fits it
// (PiecesWithin).
func markerAt(wm *meter.Meter, kind string, n, level int) string {
	for _, m := range []string{"[REDACTED:" + kind + "]", "[" + kind + "]", "[REDACTED]", "***"}[level:] {
		tick(wm, 1)
		if len(m) <= n {
			return m
		}
	}
	return strings.Repeat("*", n)
}

// referenceSuffixes mark keys whose values name or point to a secret instead of holding one.
var referenceSuffixes = []string{
	"name", "names", "ref", "refs", "keyref", "path", "file", "filename", "dir", "type", "kind", "id",
	"count", "ttl", "expiry", "expires", "expiresat", "expiration", "version", "length", "len", "url", "uri",
	"endpoint", "provider", "mode", "enabled", "required", "source", "namespace", "selector", "store",
}

// flagReferenceSuffixes mark command-line flags, and only flags, that never take the secret itself:
// "--password-stdin" (the secret comes on standard input, the flag takes no value) and "--token-audience" (the
// audience a token is issued for). As a key of any other form ("PASSWORD_STDIN=", "client_secret_for_audience:")
// the name holds a secret like any other (#585).
var flagReferenceSuffixes = []string{"stdin", "audience"}

// keepReference reports whether the key in group 1 of m is a reference to a secret. It judges only that
// key, never a neighbouring one.
func keepReference(wm *meter.Meter, s string, m []int) bool {
	return isReference(wm, s[m[2]:m[3]])
}

// keySeparators removes the separators of a key name before its suffix is compared.
var keySeparators = strings.NewReplacer("_", "", "-", "", ".", "")

// isReference reports whether key, quoted or not, names or points to a secret instead of holding one. A key that holds the word "secret"
// never takes the suffix "id": a Vault AppRole secret ID ("--secret-id", "secret_id", "secretId") and
// "client_secret_key_id" are credentials, not the ID of one, while "secret_name", "secretRef", "secret_file"
// and "secret_path" stay references (#585).
func isReference(wm *meter.Meter, key string) bool {
	return referenceKey(wm, key, false)
}

// referenceKey is isReference that, with flag, also takes flagReferenceSuffixes: for a flag followed by
// blanks or a line end ("--password-stdin -u bob"), never for one followed by "=" ("--password-stdin=x" holds
// a value like "PASSWORD_STDIN=x").
func referenceKey(wm *meter.Meter, key string, flag bool) bool {
	key = strings.ToLower(strings.Trim(key, `"'\`))
	key = keySeparators.Replace(key)
	secret := strings.Contains(key, "secret")
	for _, suffix := range referenceSuffixes {
		tick(wm, 1)
		if strings.HasSuffix(key, suffix) && (suffix != "id" || !secret) {
			return true
		}
	}
	if flag {
		for _, suffix := range flagReferenceSuffixes {
			tick(wm, 1)
			if strings.HasSuffix(key, suffix) {
				return true
			}
		}
	}
	return false
}

// secretWordPattern finds a secret word (secretWordExpr) anywhere in a key.
var secretWordPattern = regexp.MustCompile(`(?i)` + secretWordExpr)

// shortKeyNames are the short key names that hold secrets but are also parts of other words (shortKeyExpr):
// the one list that the key patterns, isBareShortKey and isSasKey read.
var shortKeyNames = strings.Split(shortKeyNamesExpr, "|")

// sasKeyName is the short key name of an Azure shared access signature.
const sasKeyName = "sas"

// isSasKey reports whether key, quoted or not, counts as a secret only through its last part "sas" (an Azure
// shared access signature): it ends in "sas" and holds no other secret word.
func isSasKey(_ *meter.Meter, key string) bool {
	k := strings.Trim(key, `"'\`)
	return len(k) >= len(sasKeyName) && strings.EqualFold(k[len(k)-len(sasKeyName):], sasKeyName) &&
		!secretWordPattern.MatchString(k)
}

// sasRefused reports whether the key in group 1 of m is a "sas" key (isSasKey) in a place where it does not
// count. "sas" is also a path segment and an image name ("ghcr.io/acme/sas:v1.2.3"), so it never counts
// right after "/" ("/sas: x"); "AZURE_STORAGE_SAS", "storageSas", "azure.storage.sas", "--source-sas" and
// "?sas=" count. With sep it counts only before "=" or before ":" and a blank or a line end ("sas=...",
// "sas: ...", not the tag in "sas:v1"), unless the key is quoted (`"sas":`), whose closing quote ends it
// (#585).
func sasRefused(wm *meter.Meter, s string, m []int, sep bool) bool {
	if !isSasKey(wm, s[m[2]:m[3]]) {
		return false
	}
	end := m[3]
	for end > m[2] && strings.IndexByte(`"'\`, s[end-1]) >= 0 {
		tick(wm, 1)
		end--
	}
	if at := end - len(sasKeyName); at > 0 {
		if s[at-1] == '/' {
			return true
		}
	}
	if !sep || end < m[3] {
		return false
	}
	i := end
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		tick(wm, 1)
		i++
	}
	switch {
	case i < len(s) && s[i] == '=':
		return false
	case i < len(s) && s[i] == ':':
		return i+1 < len(s) && !isSpace(wm, s[i+1])
	}
	return true
}

// bareShortColon reports whether the key in group 1 of m is a bare short key (isBareShortKey), unquoted and
// followed by a colon. In that form it is as often a label in prose ("pass: 12 fail: 0", "auth: failed for
// user bob", "pre-auth: ok") as a key, so its value counts only when it looks like a secret. As "key=value",
// quoted, or as the last part of any other longer key ("DB_PASS", "basic_auth", "db-pass", "proxy-auth") it
// always counts.
func bareShortColon(wm *meter.Meter, s string, m []int) bool {
	if !isBareShortKey(wm, s[m[2]:m[3]]) {
		return false
	}
	i := m[3]
	for i < len(s) && isSpace(wm, s[i]) {
		tick(wm, 1)
		i++
	}
	return i < len(s) && s[i] == ':'
}

// proseShortKeyPrefixes are the words that, before a hyphen, make a short key read as prose ("pre-auth",
// "two-pass", "multi-sig") rather than as a key. The list is closed and short on purpose: any other word
// ("db-pass", "admin-pass", "proxy-auth") names what the credential is for, so such a key always counts.
var proseShortKeyPrefixes = []string{"pre", "post", "re", "two", "multi", "single"}

// isBareShortKey reports whether key is one of shortKeyNames ("pass", "auth", "sig", "sas") on its own, in
// any case, or in lower case after exactly one word of proseShortKeyPrefixes and a hyphen ("pre-auth", but not
// "Pre-Auth", "pre-x-auth" or "db-pass").
func isBareShortKey(_ *meter.Meter, key string) bool {
	if slices.Contains(shortKeyNames, strings.ToLower(key)) {
		return true
	}
	prefix, short, ok := strings.Cut(key, "-")
	return ok && slices.Contains(shortKeyNames, short) && slices.Contains(proseShortKeyPrefixes, prefix)
}

// looksLikeSecret reports whether v has the shape of a credential rather than of prose: at least 8 bytes,
// no whitespace, and a letter together with a digit or a symbol.
func looksLikeSecret(wm *meter.Meter, v string) bool {
	return !strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' }) && looksLikeQuotedSecret(wm, v)
}

// looksLikeQuotedSecret is looksLikeSecret for a quoted value, which may hold blanks (a passphrase such as
// "correct horse battery staple"): at least 8 bytes and a letter together with a digit, a symbol or a blank.
func looksLikeQuotedSecret(wm *meter.Meter, v string) bool {
	if len(v) < 8 {
		return false
	}
	var letter, other bool
	for i := range len(v) {
		tick(wm, 1)
		c := v[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			letter = true
		} else {
			other = true
		}
	}
	return letter && other
}

// lineLooksLikeSecret reports whether the text of one line of a block value, after any sequence entry
// indicators, looks like a secret: as a quoted value (looksLikeQuotedSecret) when it is quoted, otherwise as
// a plain one (looksLikeSecret).
func lineLooksLikeSecret(wm *meter.Meter, text string) bool {
	text = strings.TrimRight(text[dashes(wm, text):], " \t")
	if len(text) >= 2 && (text[0] == '"' || text[0] == '\'') && text[len(text)-1] == text[0] {
		return looksLikeQuotedSecret(wm, text[1:len(text)-1])
	}
	return looksLikeSecret(wm, text)
}

// blockLooksLikeSecret reports whether any line of the block value s[start:end] looks like a secret
// (lineLooksLikeSecret).
func blockLooksLikeSecret(wm *meter.Meter, s string, start, end int) bool {
	for line := range strings.SplitSeq(s[start:end], "\n") {
		tick(wm, 1)
		if lineLooksLikeSecret(wm, strings.Trim(line, " \t\r")) {
			return true
		}
	}
	return false
}

// minSchemeWord is the length from which any value after Basic or Bearer counts: longer than the words
// that follow them in prose ("authentication").
const minSchemeWord = 16

// schemeSpan judges the value after "Basic" or "Bearer" (group 2) outside an Authorization header. Prose
// follows these words too ("failed basic health check", "missing Bearer token"), so the value counts when it
// looks like a credential (looksLikeSecret), is at least minSchemeWord bytes long (an opaque token of
// letters only), or, after Basic, is padded base64 of "user:password" in printable text, which catches
// short ones such as "dTpw" ("u:p").
func schemeSpan(wm *meter.Meter, s string, m []int, _ int) (int, int, bool) {
	v := s[m[4]:m[5]]
	if looksLikeSecret(wm, v) || len(v) >= minSchemeWord || strings.EqualFold(s[m[2]:m[3]], "basic") && isBasicCredential(wm, v) {
		return m[4], m[5], true
	}
	return 0, 0, false
}

// isBasicCredential reports whether v is standard padded base64 of printable text holding a colon.
func isBasicCredential(_ *meter.Meter, v string) bool {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil || bytes.IndexByte(b, ':') < 0 || !utf8.Valid(b) {
		return false
	}
	return !bytes.ContainsFunc(b, func(r rune) bool { return r < ' ' || r == 0x7f })
}

// blockIndicator matches a YAML block scalar header value such as "|", ">-" or "|2+".
var blockIndicator = regexp.MustCompile(`^[|>][1-9+-]{0,2}$`)

// isAlnum reports whether c is an ASCII letter or digit.
func isAlnum(_ *meter.Meter, c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ipv6Span confirms an IPv6 candidate with net/netip, so clock times and similar are kept. A single colon
// at the end that belongs to the surrounding text ("addr ::1: refused") is trimmed, and when a label is
// glued to the front ("source:2001:db8::1" gives the candidate "ce:2001:db8::1"), the address after the
// label's colon is tried. A range glued to letters or digits on either side is kept, as it is part of a
// longer token. Candidates are at most about 45 bytes, so the retries cost constant time.
func ipv6Span(wm *meter.Meter, s string, m []int, _ int) (int, int, bool) {
	start, end := m[0], m[1]
	ends := []int{end}
	if end-start > 1 && s[end-1] == ':' && s[end-2] != ':' {
		ends = append(ends, end-1)
	}
	for a := start; a < end; a++ {
		tick(wm, 1)
		if a > start && (s[a-1] != ':' || s[a] == ':') {
			continue
		}
		if a > 0 && isAlnum(wm, s[a-1]) {
			continue
		}
		for _, b := range ends {
			tick(wm, 1)
			if b <= a || (b < len(s) && isAlnum(wm, s[b])) {
				continue
			}
			if addr, err := netip.ParseAddr(s[a:b]); err == nil && addr.Is6() {
				return a, b, true
			}
		}
	}
	return 0, 0, false
}

// lineEnd returns the index of the end of the line holding pos, before any "\r\n" or "\n".
func lineEnd(wm *meter.Meter, s string, pos int) int {
	end := len(s)
	if i := indexByte(wm, s[pos:], '\n'); i >= 0 {
		end = pos + i
	}
	if end > pos && s[end-1] == '\r' {
		end--
	}
	return end
}

// blockLine is one non-blank line of a YAML block: where the line starts, where its text starts after the
// indentation, and where it ends, before any line break.
type blockLine struct {
	start, text, end int
}

// indent returns the column of the line's text.
func (l blockLine) indent(_ *meter.Meter) int { return l.text - l.start }

// Kinds of block read by blockLines.
const (
	// blockScalar: the lines indented deeper than the key, as in a literal or folded block scalar or the
	// continuation lines of a plain scalar. A comment line is content like any other.
	blockScalar = iota
	// blockValue: the value of a key with nothing after its colon. Comment lines at any indentation are
	// skipped, and a compact sequence ("key:\n- item") may start at the key's own indentation.
	blockValue
)

// blockLines returns, in order, the non-blank lines after pos (which is at a line end) that belong to the
// block of a key at column keyCol, and stops at the first line that does not. Comment lines of a blockValue
// are skipped without being returned. Each line is read once.
func blockLines(wm *meter.Meter, s string, pos, keyCol, kind int) iter.Seq[blockLine] {
	return func(yield func(blockLine) bool) {
		for {
			tick(wm, 1)
			l, next, ok := nextBlockLine(wm, s, pos, keyCol, kind)
			if !ok || !yield(l) {
				return
			}
			pos = next
		}
	}
}

// nextBlockLine returns the first line after pos (which is at a line end) that blockLines would return, and
// the end of that line, from which the next one is read. ok is false when the block ends first.
func nextBlockLine(wm *meter.Meter, s string, pos, keyCol, kind int) (blockLine, int, bool) {
	for pos < len(s) && (s[pos] == '\n' || s[pos] == '\r') {
		tick(wm, 1)
		if s[pos] == '\r' {
			pos++
			continue
		}
		l := blockLine{start: pos + 1, end: lineEnd(wm, s, pos+1)}
		countScan(wm, l.end-l.start+1)
		pos = l.end
		text := s[l.start:l.end]
		trimmed := strings.TrimLeft(text, " \t")
		if trimmed == "" {
			continue
		}
		l.text = l.start + len(text) - len(trimmed)
		if kind == blockValue && trimmed[0] == '#' {
			continue
		}
		deeper := l.indent(wm) > keyCol
		compactSequence := kind == blockValue && l.indent(wm) == keyCol && dashes(wm, trimmed) > 0
		if !deeper && !compactSequence {
			break
		}
		return l, pos, true
	}
	return blockLine{}, pos, false
}

// dashes returns the length of the sequence entry indicators ("- ", "- - ") at the start of text.
func dashes(wm *meter.Meter, text string) int {
	n := 0
	for n < len(text) && text[n] == '-' && (n+1 == len(text) || text[n+1] == ' ' || text[n+1] == '\t') {
		tick(wm, 1)
		n++
		for n < len(text) && (text[n] == ' ' || text[n] == '\t') {
			tick(wm, 1)
			n++
		}
	}
	return n
}

// indentedLines scans the lines after pos, which is at a line end, while they are blank or indented
// deeper than keyCol. It returns the start of the first non-blank one and the end of the last, or -1, -1.
func indentedLines(wm *meter.Meter, s string, pos, keyCol int) (int, int) {
	start, end := -1, -1
	for l := range blockLines(wm, s, pos, keyCol, blockScalar) {
		tick(wm, 1)
		if start < 0 {
			start = l.text
		}
		end = l.end
	}
	return start, end
}

// blockScalarHeader matches the header line of a YAML literal or folded block scalar under a secret key, with
// chomping and indentation indicators: group 1 is the key, group 2 the indicator.
var blockScalarHeader = regexp.MustCompile(
	`(?im)^[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:[ \t]*([|>][1-9+-]{0,2})[ \t]*(?:#[^\r\n]*)?\r?$`)

// blockScalarScan redacts the content of each YAML literal or folded block scalar whose header line
// blockScalarHeaderWindows is blockScalarHeader read by windows (windows.go).
var blockScalarHeaderWindows = newWindowed(nil, blockScalarHeader)

// blockScalarHeader matches: the following lines that are blank or indented deeper than the key, up to the last
// non-blank one. With no content, the indicator itself (group 2) is redacted. Headers inside a block already
// redacted are skipped, so each byte is scanned once. Under a bare short key (bareShortColon) the content
// counts only when one of its lines looks like a secret, and an empty block is kept.
//
// A block is read once, however many headers stand inside it (#845): the block of a header inside a block is
// part of that block, so when a bare short key refused the outer one, no line of the inner one looks like a
// secret either, and a bare short header up to the end of the refused block is skipped unread. Blocks nest by
// indentation, so the end of the outermost refused block is all the bookkeeping needs: lines each indented
// one more than the last, all "pass: |", are read once, not once per header.
func blockScalarScan(wm *meter.Meter, s string, add func(start, end int)) {
	prevEnd, refused := 0, -1
	blockScalarHeaderWindows.each(wm, s, func(m []int) bool {
		tick(wm, 1)
		if m[0] < prevEnd || keepReference(wm, s, m) || sasRefused(wm, s, m, true) {
			return true
		}
		bare := bareShortColon(wm, s, m)
		if bare && m[1] < refused {
			return true
		}
		start, end := indentedLines(wm, s, m[1], m[2]-m[0])
		switch {
		case start < 0:
			if !bare {
				add(m[4], m[5])
				prevEnd = m[5]
			}
		case !bare || blockLooksLikeSecret(wm, s, start, end):
			add(start, end)
			prevEnd = end
		default:
			refused = max(refused, end)
		}
		return true
	})
}

// childKey matches a mapping key at the start of a line's text, after any sequence entry indicators. Group
// 1 is the key; the value starts at the end of the match.
var childKey = regexp.MustCompile(`^(?:-[ \t]+)*("[^"\r\n]*"|'[^'\r\n]*'|[^\s"'#:-][^#:\r\n]*?|-[^\s#:][^#:\r\n]*?)[ \t]*:(?:[ \t]+|$)`)

// inlineValue returns the value that starts at pos and ends at the line end le: the text between the
// quotes of a quoted value, otherwise the text up to the line end without trailing blanks.
func inlineValue(wm *meter.Meter, s string, pos, le int) (int, int) {
	if pos >= le {
		return pos, pos
	}
	if c := s[pos]; c == '"' || c == '\'' {
		if qe := quotedEnd(wm, s, pos); qe > 0 {
			return pos + 1, qe - 1
		}
	}
	end := le
	for end > pos && isSpace(wm, s[end-1]) {
		tick(wm, 1)
		end--
	}
	return pos, end
}

// nextLineSpans redacts the value of a YAML key with nothing after its colon (but blanks or a comment),
// written on the following lines: those indented deeper than the key, comment lines at any indentation,
// and the entries of a compact sequence at the key's own indentation. A key with no such lines is truly
// empty and kept.
//
// A scalar value (plain, quoted, a list of scalars or a flow collection) is redacted whole. A mapping
// keeps its keys and loses the scalar values of its children, at every depth, so "credentials:\n  user:
// admin" loses "admin" as well as any secret-named child. Children whose key is a reference
// ("secretRef", "name") keep their values and everything below them, except the value of a secret key
// with nothing after its colon among them, which is redacted in the same way.
//
// Under a bare short key (bareShortColon) a value counts only when it looks like a secret: a scalar when
// one of its lines does, and in a mapping each value on its own.
//
// The block is read once and its end is returned, so headers inside it are skipped and each byte is
// scanned once.
func nextLineSpans(wm *meter.Meter, s string, m []int, prevEnd int, add func(start, end int)) int {
	if m[0] < prevEnd || keepReference(wm, s, m) || sasRefused(wm, s, m, true) {
		return prevEnd
	}
	return max(prevEnd, nextLineBlock(wm, s, m[1], m[2]-m[0], bareShortColon(wm, s, m), add))
}

// nextLineHeader matches, on one line, the header the next-line rule looks for (see builtinRules). Group 1
// is the key.
var nextLineHeader = regexp.MustCompile(`(?i)^` + nextLineHeaderExpr)

// nestedHeader returns the submatches, as offsets into s, of the next-line rule's header on line l, or nil
// when l holds none or its key is a reference.
func nestedHeader(wm *meter.Meter, s string, l blockLine) []int {
	hm := nextLineHeader.FindStringSubmatchIndex(counted(wm, s[l.start:l.end]))
	if hm == nil {
		return nil
	}
	for i := range hm {
		tick(wm, 1)
		if hm[i] >= 0 {
			hm[i] += l.start
		}
	}
	if keepReference(wm, s, hm) || sasRefused(wm, s, hm, true) {
		return nil
	}
	return hm
}

// nextLineBlock redacts the value of the key at column keyCol whose header line ends at pos, as described
// for nextLineSpans, and returns the end of the last line of the block, or pos when it has none. With bare,
// each value counts only when it looks like a secret.
func nextLineBlock(wm *meter.Meter, s string, pos, keyCol int, bare bool, add func(start, end int)) int {
	tick(wm, 1) // a call, which may be a turn of its recursion
	const (
		undecided = iota
		scalar
		mapping
	)
	addValue := func(start, end int) {
		if !bare || blockLooksLikeSecret(wm, s, start, end) {
			add(start, end)
		}
	}
	mode := undecided
	start, end := -1, pos
	// keepBelow and redactBelow are the key columns of a reference child, whose lines below are kept, and
	// of a child with a block scalar, whose lines below are redacted whole; -1 when there is none.
	keepBelow, redactBelow := -1, -1
	for pos < len(s) {
		tick(wm, 1)
		l, next, ok := nextBlockLine(wm, s, pos, keyCol, blockValue)
		if !ok {
			break
		}
		pos, end = next, l.end
		text := s[l.text:l.end]
		if mode == undecided {
			start, mode = l.text+dashes(wm, text), scalar
			if childKey.MatchString(counted(wm, text)) {
				mode = mapping
			}
		}
		if mode == scalar {
			continue
		}
		kept := keepBelow >= 0 && l.indent(wm) > keepBelow
		if !kept {
			keepBelow = -1
		}
		inBlockScalar := !kept && redactBelow >= 0 && l.indent(wm) > redactBelow
		if !kept && !inBlockScalar {
			redactBelow = -1
		}
		if kept || bare && !inBlockScalar {
			// A secret key with nothing after its colon, below a reference or under a bare short key, is
			// judged on its own, as a match of the rule would be. Its block is read here and skipped, so
			// each line is still read once.
			if hm := nestedHeader(wm, s, l); hm != nil {
				pos = nextLineBlock(wm, s, l.end, hm[2]-hm[0], bareShortColon(wm, s, hm), add)
				end = pos
				continue
			}
		}
		if kept {
			continue
		}
		if inBlockScalar {
			addValue(inlineValue(wm, s, l.text, l.end))
			continue
		}
		cm := childKey.FindStringSubmatchIndex(counted(wm, text))
		if cm == nil || gluedKey(wm, text[cm[2]:cm[3]]) {
			// A scalar line: a sequence entry, a flow collection or a continuation. A plain key holding a
			// blank, a comma or a quote is no key of a secret's map but a value with another key glued to it
			// ("hunter2 token: x"), and goes whole (#840).
			addValue(inlineValue(wm, s, l.text+dashes(wm, text), l.end))
			continue
		}
		keyCol := l.indent(wm) + cm[2]
		if isReference(wm, text[cm[2]:cm[3]]) {
			keepBelow = keyCol
			continue
		}
		vpos := l.text + cm[1]
		switch {
		case vpos >= l.end || s[vpos] == '#':
			// An empty value, maybe with a comment: its children follow on deeper lines.
		case isBlockHeader(wm, s, vpos):
			redactBelow = keyCol
		default:
			addValue(inlineValue(wm, s, vpos, l.end))
		}
	}
	if mode == scalar && (!bare || blockLooksLikeSecret(wm, s, start, end)) {
		add(scalarValue(wm, s, start, end))
	}
	return end
}

// gluedKey reports whether a child key found by childKey is plain and holds a blank, a comma or a quote,
// as a value with another key glued to it does.
func gluedKey(_ *meter.Meter, key string) bool {
	return key[0] != '"' && key[0] != '\'' && strings.ContainsAny(key, " \t,\"'")
}

// scalarValue returns the part of the scalar block value s[start:end] to redact: the text between the
// quotes of a quoted scalar that is the whole value, so it keeps its quotes, otherwise all of it. A quoted
// scalar that a backslash continues past the block's end (an escaped line break, #523) is redacted up to its
// closing quote, continuation lines included.
func scalarValue(wm *meter.Meter, s string, start, end int) (int, int) {
	if start < end && (s[start] == '"' || s[start] == '\'') {
		qe := quotedEnd(wm, s, start)
		if qe > end {
			return start + 1, qe - 1
		}
		if qe > 0 && strings.TrimRight(s[qe:end], " \t") == "" {
			return start + 1, qe - 1
		}
	}
	return start, end
}

// keySpaceExpr is a blank on either side of the colon in the rule for block indicators: ASCII blanks, the
// Unicode space separators (no-break, figure, ideographic and the rest of category Zs), and the zero-width
// space, word joiner and byte order mark, which a message may carry where a blank was.
const keySpaceExpr = `[ \t\p{Zs}\x{200B}\x{2060}\x{FEFF}]`

// anyKey matches a secret key, its colon and a YAML block scalar indicator ("|", ">-", "|2+"), with or
// without blanks around the colon, wherever the key stands on a line (group 1 is the key, group 2 the
// indicator): the one form that says a block follows, also once the block was flattened onto the key's line
// (#840, decided 2026-11-17). anyKeySpans checks what follows the indicator.
var anyKey = regexp.MustCompile(`(?i)(` + secretKeyExpr + `)` + keySpaceExpr + `*:` + keySpaceExpr + `*([|>][1-9+-]{0,2})`)

// countScan counts n bytes read by the rule for block indicators, or of a line by nextBlockLine, as work
// (tick, work.go).
func countScan(wm *meter.Meter, n int) { tick(wm, n) }

// anyKeySpans is the spans function of the rule for a secret key and a block indicator wherever the key
// stands (#840). The first match on a line reads the whole line (anyKeyLine), so each line is read once by
// this rule and later matches on it are skipped.
func anyKeySpans(wm *meter.Meter, s string, m []int, prevEnd int, add func(start, end int)) int {
	if m[0] < prevEnd {
		return prevEnd
	}
	return anyKeyLine(wm, s, m[0], -1, add)
}

// anyKeyLine reads every match of anyKey on the line of s that holds from, from there on, and returns the end
// of what it read: the line end, or the end of a block below the line. For each key, in order:
//
//   - A key inside a flow collection ("{", "[" closed on the line) is left to the flow rules, unless it is
//     inside a quoted string there (a JSON or logfmt string value), which is read like any other text, or
//     text follows its indicator (flowKey). A bracket that never closes on the line opens nothing.
//   - The indicator must be followed by a blank or the end of the value: the line end, or the end of the
//     quoted string the key stands in (where "\n", "\t" and "\r" count as blanks).
//   - The block below: the lines indented deeper than the key (its own column when it is first on its line,
//     otherwise the column of the line's text), after any "---" or "..." lines at column 0; with none, the
//     lines from the next one that is indented less than that column but not at column 0, if any. Its end is
//     known, so only it goes. With no block, the indicator alone goes, as the key-value rules have it.
//   - Text after the indicator, other than a comment over a block below: a block flattened onto the line.
//     Its end cannot be told, so the rest of the line (or of the string) goes, besides the block below.
//
// Under a bare short key (bareShortColon) the text and the block count only where they look like a secret;
// a refused block is read line by line for keys of its own (anyKeyNested), so each line is still read once.
// Lines before refused (-1 for none) are in such a block: no line of it looks like a secret, so the block
// of a bare short key on one of them is refused without reading it, and its lines are read by the caller.
func anyKeyLine(wm *meter.Meter, s string, from, refused int, add func(start, end int)) int {
	ls := lastIndexByte(wm, s[:from], '\n') + 1
	le := lineEnd(wm, s, from)
	countScan(wm, le-ls)
	text := ls
	for text < le && (s[text] == ' ' || s[text] == '\t') {
		tick(wm, 1)
		text++
	}
	r := &anyKeyReader{
		s: s, ls: ls, le: le, lineCol: text - ls, firstKey: text + dashes(wm, s[text:le]), refused: refused, add: add,
		ctx: newLineContext(wm, s, ls, le), end: le, cursor: from, refusedFrom: -1, refusedEnd: -1,
	}
	for _, km := range anyKey.FindAllStringSubmatchIndex(counted(wm, s[from:le]), -1) {
		tick(wm, 1)
		for i := range km {
			tick(wm, 1)
			if km[i] >= 0 {
				km[i] += from
			}
		}
		r.key(wm, km)
	}
	return r.end
}

// anyKeyReader is what anyKeyLine knows about the line it reads, so that no part of the line is read more
// than a fixed number of times, however many keys it holds.
type anyKeyReader struct {
	s                       string
	ls, le, lineCol         int
	firstKey, refused       int
	add                     func(start, end int)
	ctx                     *lineContext
	end, cursor             int
	blockRead               bool
	refusedFrom, refusedEnd int
}

// key reads the match km of anyKey on the line, as anyKeyLine describes it.
func (r *anyKeyReader) key(wm *meter.Meter, km []int) {
	s, le := r.s, r.le
	if km[0] < r.cursor {
		return
	}
	keyAt := km[2]
	if q := s[keyAt]; (q == '"' || q == '\'') && s[km[3]-1] != q {
		// The quote opens the string the key stands in, not the key.
		keyAt++
	}
	strEnd := r.ctx.stringEnd(wm, keyAt)
	inStr := strEnd >= 0
	if !inStr && r.ctx.inFlow(wm, keyAt) {
		r.flowKey(wm, km)
		return
	}
	if keepReference(wm, s, km) || sasRefused(wm, s, km, true) {
		return
	}
	valEnd := le
	if inStr {
		valEnd = strEnd
	}
	after, ok := afterIndicator(wm, s, km[5], valEnd, inStr)
	if !ok {
		return
	}
	bare := bareShortColon(wm, s, km)
	bStart, bEnd := -1, -1
	if !inStr && !r.blockRead && (!bare || le >= r.refused) {
		keyCol := r.lineCol
		if km[2] == r.firstKey {
			keyCol = km[2] - r.ls
		}
		bStart, bEnd = blockBelow(wm, s, le, keyCol)
		r.blockRead = true
	}
	r.cursor = valEnd
	switch {
	case after < valEnd && (s[after] != '#' || bStart < 0):
		r.rest(wm, after, valEnd, bare)
	case bStart < 0 && !bare:
		r.add(km[4], km[5])
	}
	if bStart >= 0 {
		countScan(wm, bEnd-bStart)
		if !bare || blockLooksLikeSecret(wm, s, bStart, bEnd) {
			r.add(bStart, bEnd)
			r.end = max(r.end, bEnd)
		} else {
			r.end = max(r.end, anyKeyNested(wm, s, le, bEnd, r.add))
		}
	}
}

// flowKey reads a key inside a flow collection that closes on the line. Such a key is left to the flow rules,
// except when its indicator is followed by text ("[password: | x]"): no flow value starts with "|" or ">", so
// that is a block flattened onto the line, and the text up to the bracket that closes the innermost
// collection goes, as rest has it. Nothing after that bracket is read for this key.
func (r *anyKeyReader) flowKey(wm *meter.Meter, km []int) {
	end := r.ctx.flowClose(wm)
	if end < 0 || keepReference(wm, r.s, km) || sasRefused(wm, r.s, km, true) {
		return
	}
	after, ok := afterIndicator(wm, r.s, km[5], end, false)
	if !ok || after >= end {
		return
	}
	r.cursor = end
	r.rest(wm, after, end, bareShortColon(wm, r.s, km))
}

// rest redacts the text from after to valEnd that follows a block indicator: a block flattened onto the
// line. Under a bare short key it counts only when a word looks like a secret; a refusal is remembered, so a
// later bare key's text up to the same end is refused without reading it again, and the keys in the refused
// text are read on their own.
func (r *anyKeyReader) rest(wm *meter.Meter, after, valEnd int, bare bool) {
	re := valEnd
	for re > after && isSpace(wm, r.s[re-1]) {
		tick(wm, 1)
		re--
	}
	switch {
	case !bare:
		r.add(after, re)
	case r.refusedFrom >= 0 && after >= r.refusedFrom && valEnd == r.refusedEnd:
		r.cursor = after
	case wordsLookLikeSecret(wm, r.s[after:re]):
		countScan(wm, re-after)
		r.add(after, re)
	default:
		countScan(wm, re-after)
		r.refusedFrom, r.refusedEnd, r.cursor = after, valEnd, after
	}
}

// anyKeyNested reads the lines after pos (a line end) up to limit, a block a bare short key refused, for
// matches of anyKey of their own (with limit as anyKeyLine's refused), and returns the end of what it read:
// limit, or the end of a block below the last line that goes past it.
func anyKeyNested(wm *meter.Meter, s string, pos, limit int, add func(start, end int)) int {
	end := limit
	for pos < limit {
		tick(wm, 1)
		nl := indexByte(wm, s[pos:limit], '\n')
		if nl < 0 {
			break
		}
		ls := pos + nl + 1
		le := lineEnd(wm, s, ls)
		countScan(wm, le-ls)
		pos = le
		if m := anyKey.FindStringIndex(counted(wm, s[ls:le])); m != nil {
			e := anyKeyLine(wm, s, ls+m[0], limit, add)
			pos, end = max(pos, e), max(end, e)
		}
	}
	return end
}

// afterIndicator returns where the text after a block indicator that ends at ie starts, past its blanks, when
// a blank or the end of the value (valEnd) follows the indicator; ok is false otherwise ("|x", "|#", ">=").
func afterIndicator(wm *meter.Meter, s string, ie, valEnd int, inStr bool) (int, bool) {
	if ie == valEnd {
		return ie, true
	}
	i := ie
	for i < valEnd {
		tick(wm, 1)
		n := blankAt(wm, s, i, valEnd, inStr)
		if n == 0 {
			break
		}
		i += n
	}
	return i, i > ie
}

// blankAt returns the length of the blank at s[i] before end, or 0: an ASCII blank, a space keySpaceExpr
// names, or, inside a quoted string, an escaped line break or tab.
func blankAt(_ *meter.Meter, s string, i, end int, inStr bool) int {
	switch c := s[i]; {
	case c == ' ' || c == '\t':
		return 1
	case c == '\\' && inStr && i+1 < end && strings.IndexByte("ntr", s[i+1]) >= 0:
		return 2
	case c >= utf8.RuneSelf:
		r, n := utf8.DecodeRuneInString(s[i:end])
		if unicode.Is(unicode.Zs, r) || r == '\u200b' || r == '\u2060' || r == '\ufeff' {
			return n
		}
	}
	return 0
}

// blockBelow returns the block under a header line that ends at le, of a key at column keyCol, as anyKeyLine
// describes it: the start of its first line's text and the end of its last line, or -1, -1.
func blockBelow(wm *meter.Meter, s string, le, keyCol int) (int, int) {
	pos := le
	for {
		tick(wm, 1)
		l, next, ok := nextBlockLine(wm, s, pos, -1, blockScalar)
		if !ok {
			break
		}
		if t := strings.TrimRight(s[l.text:l.end], " \t"); (t != "---" && t != "...") || l.indent(wm) != 0 {
			break
		}
		pos = next
	}
	if start, end := indentedLines(wm, s, pos, keyCol); start >= 0 {
		return start, end
	}
	l, _, ok := nextBlockLine(wm, s, pos, -1, blockScalar)
	if !ok || l.indent(wm) == 0 || l.indent(wm) >= keyCol && pos == le {
		return -1, -1
	}
	return indentedLines(wm, s, pos, l.indent(wm)-1)
}

// lineContext knows, for one line, the quoted strings on it and the flow collections that close on it, so
// that anyKeyLine can ask about each key in turn, in order, reading the line once.
type lineContext struct {
	// strs are the quoted strings, as the offsets of their opening and closing quotes, in order.
	strs [][2]int
	// flows are the brackets of the flow collections that close on the line, in order: the offset, +1 at an
	// opening bracket and -1 at its closing one, and the offset of the other bracket of the pair.
	flows  [][3]int
	si, fi int
	// open holds the offsets of the closing brackets of the collections inFlow is inside, innermost last.
	open []int
}

// newLineContext reads the line s[ls:le]. A double quote opens a string that runs to the next double quote
// not escaped by a backslash; a single quote opens one only at the start of the line or after a blank or one
// of "{[(,:=", and runs to the next single quote that is not doubled. A quote that does not close on the line
// opens nothing, and no later quote of its kind does either. Brackets inside strings are not counted, and a
// closing bracket that does not match the last open one is skipped.
func newLineContext(wm *meter.Meter, s string, ls, le int) *lineContext {
	ctx := &lineContext{}
	var open []int
	noDouble, noSingle := false, false
	for i := ls; i < le; i++ {
		tick(wm, 1)
		switch c := s[i]; c {
		case '"', '\'':
			if c == '"' && noDouble || c == '\'' && (noSingle || i > ls && strings.IndexByte(" \t{[(,:=", s[i-1]) < 0) {
				continue
			}
			j := closingQuote(wm, s, i, le)
			if j < 0 {
				if c == '"' {
					noDouble = true
				} else {
					noSingle = true
				}
				continue
			}
			ctx.strs = append(ctx.strs, [2]int{i, j})
			i = j
		case '{', '[':
			open = append(open, i)
		case '}', ']':
			if n := len(open); n > 0 && s[open[n-1]] == c-2 {
				// '{' and '[' are two below '}' and ']'.
				ctx.flows = append(ctx.flows, [3]int{open[n-1], 1, i}, [3]int{i, -1, open[n-1]})
				open = open[:n-1]
			}
		}
	}
	sortFunc(wm, ctx.flows, func(a, b [3]int) int { return a[0] - b[0] })
	return ctx
}

// closingQuote returns the offset of the quote that closes the one at i before le, or -1.
func closingQuote(wm *meter.Meter, s string, i, le int) int {
	q := s[i]
	for j := i + 1; j < le; j++ {
		tick(wm, 1)
		switch {
		case q == '"' && s[j] == '\\':
			j++
		case s[j] == q && q == '\'' && j+1 < le && s[j+1] == '\'':
			j++
		case s[j] == q:
			return j
		}
	}
	return -1
}

// stringEnd returns the offset of the closing quote of the string pos is inside, or -1. Calls must come
// with pos in increasing order.
func (c *lineContext) stringEnd(wm *meter.Meter, pos int) int {
	for c.si < len(c.strs) && c.strs[c.si][1] < pos {
		tick(wm, 1)
		c.si++
	}
	if c.si < len(c.strs) && c.strs[c.si][0] < pos && pos <= c.strs[c.si][1] {
		return c.strs[c.si][1]
	}
	return -1
}

// inFlow reports whether pos is inside a flow collection that closes on the line. Calls must come with pos
// in increasing order.
func (c *lineContext) inFlow(wm *meter.Meter, pos int) bool {
	for c.fi < len(c.flows) && c.flows[c.fi][0] < pos {
		tick(wm, 1)
		if f := c.flows[c.fi]; f[1] > 0 {
			c.open = append(c.open, f[2])
		} else if n := len(c.open); n > 0 {
			c.open = c.open[:n-1]
		}
		c.fi++
	}
	return len(c.open) > 0
}

// flowClose returns the offset of the bracket that closes the innermost flow collection the last position
// given to inFlow is inside, or -1.
func (c *lineContext) flowClose(_ *meter.Meter) int {
	if n := len(c.open); n > 0 {
		return c.open[n-1]
	}
	return -1
}

// wordsLookLikeSecret reports whether the text of a line, or any blank-separated word of it, looks like a
// secret (lineLooksLikeSecret).
func wordsLookLikeSecret(wm *meter.Meter, text string) bool {
	if lineLooksLikeSecret(wm, strings.TrimSpace(text)) {
		return true
	}
	for w := range strings.FieldsSeq(text) {
		tick(wm, 1)
		if lineLooksLikeSecret(wm, w) {
			return true
		}
	}
	return false
}

// envEntryLines bounds the lines of an environment entry read to find its "value": Kubernetes writes it
// right after "name", or after "valueFrom".
const envEntryLines = 16

// envValueSpan redacts the value of a Kubernetes environment variable whose name holds a secret, in the
// block form "- name: DB_PASSWORD\n  value: ...": the "value" key among the entry's own keys, inline or on
// the following lines. A name that is a reference ("DB_PASSWORD_FILE") keeps its value. At most
// envEntryLines lines of the entry are read, so nested entries cost constant work per match.
func envValueSpan(wm *meter.Meter, s string, m []int, prevEnd int) (int, int, bool) {
	if m[0] < prevEnd || keepReference(wm, s, m) || sasRefused(wm, s, m, false) {
		return 0, 0, false
	}
	line := s[m[0]:m[1]]
	trimmed := strings.TrimLeft(line, " \t")
	nameCol := len(line) - len(trimmed) + dashes(wm, trimmed)
	read := 0
	for l := range blockLines(wm, s, m[1], nameCol-1, blockScalar) {
		tick(wm, 1)
		if read++; read > envEntryLines {
			break
		}
		if l.indent(wm) != nameCol {
			continue
		}
		rest, ok := strings.CutPrefix(s[l.text:l.end], "value")
		rest = strings.TrimLeft(rest, " \t")
		if !ok || rest == "" || rest[0] != ':' {
			continue
		}
		vpos := l.end - len(rest) + 1
		for vpos < l.end && (s[vpos] == ' ' || s[vpos] == '\t') {
			tick(wm, 1)
			vpos++
		}
		if vpos >= l.end || isBlockHeader(wm, s, vpos) {
			start, end := indentedLines(wm, s, l.end, nameCol)
			return start, end, start >= 0
		}
		start, end := valueSpan(wm, s, vpos, boundaryLine, nameCol)
		return start, end, true
	}
	return 0, 0, false
}

// minTokenLength is the shortest part of a run of base64 or hex characters that is taken for a random token.
const minTokenLength = 24

// longTokenSpans redacts a run of base64, base64url or hex characters when some part of it between slashes
// looks random: a piece between hyphens of at least minTokenLength that mixes letters and digits, or a
// hyphenated part of at least that length that mixes upper case, lower case and digits (base64url). A run
// that no such part makes random may still hold standard base64 that its slashes cut short: then only the
// slash-separated parts that read as base64 are redacted, each stretch on its own (base64Windows). Paths
// and image names (short parts between slashes), long names made of words, Kubernetes names and UUIDs
// (lower case, short pieces between hyphens) and the hex of an image digest ("sha256:...", which identifies
// a build, not a credential, also with a suffix such as "-amd64") are kept; what follows the digest's hex is
// judged on its own. It returns the end of the run.
func longTokenSpans(wm *meter.Meter, s string, m []int, _ int, add func(start, end int)) int {
	start, run := m[0], s[m[0]:m[1]]
	if prefix := strings.ToLower(s[max(0, m[0]-7):m[0]]); prefix == "sha256:" || prefix == "sha512:" {
		if digest, rest, _ := strings.Cut(run, "-"); isHex(wm, digest) {
			start, run = m[1]-len(rest), rest
		}
	}
	if randomRun(wm, run) {
		add(start, m[1])
		return m[1]
	}
	base64Windows(wm, run, func(a, b int) { add(start+a, start+b) })
	return m[1]
}

// randomRun reports whether some part of run between slashes looks random, as described for longTokenSpans.
func randomRun(wm *meter.Meter, run string) bool {
	for part := range strings.SplitSeq(run, "/") {
		tick(wm, 1)
		if len(part) < minTokenLength {
			continue
		}
		if strings.Contains(part, "-") && mixesCase(wm, part) && mixesLettersAndDigits(wm, part) {
			return true
		}
		for piece := range strings.SplitSeq(part, "-") {
			tick(wm, 1)
			if len(piece) >= minTokenLength && mixesLettersAndDigits(wm, piece) {
				return true
			}
		}
	}
	return false
}

// minBase64Window is the number of characters, slashes and padding aside, from which slash-separated parts
// are judged together as standard base64 (base64Windows): less than the 40 of an AWS secret access key or
// the 44 of an older Entra client secret, and enough for the score of a window to tell words from base64.
const minBase64Window = 32

// maxBase64Score is the highest score per pair of neighbouring characters (wordScore) at which a window
// counts as base64. Random base64 scores about -0.47 and a window of 32 random characters scores above 0.15
// a few times in 10,000; a 40-character key gives several windows, and 5 of 100,000 seeded slash-split keys
// scored above it in all of them (long runs of lower-case letters by chance). Path words score from about 0.2 (a Windows temp path ending in a random name) to 0.9
// (TestSlashSplitRuleKeepsPaths).
const maxBase64Score = 0.15

// Character classes for wordScore.
const (
	classVowel = iota // a lower-case vowel
	classConsonant
	classUpper
	classDigit
	classOther
	numClasses
)

// wordScore holds, for a character of one class followed by one of another, the log of how much likelier
// that pair is in words (paths, CamelCase names) than in random base64, where each of the 64 characters is
// as likely in any place. The rates for words are rough estimates; the threshold maxBase64Score was set by
// measuring random keys and real paths, not from these rates alone.
var wordScore = func() (t [numClasses][numClasses]float64) {
	inWords := [numClasses][numClasses]float64{
		classVowel:     {.12, .75, .07, .05, .01},
		classConsonant: {.45, .38, .08, .07, .02},
		classUpper:     {.35, .30, .30, .04, .01},
		classDigit:     {.07, .08, .15, .65, .05},
		classOther:     {.15, .25, .30, .20, .10},
	}
	inBase64 := [numClasses]float64{5.0 / 64, 21.0 / 64, 26.0 / 64, 10.0 / 64, 2.0 / 64}
	for a := range t {
		tick(nil, 1)
		for b := range t[a] {
			tick(nil, 1)
			t[a][b] = math.Log(inWords[a][b] / inBase64[b])
		}
	}
	return t
}()

// wordClass returns the class of c for wordScore.
func wordClass(_ *meter.Meter, c byte) int {
	switch {
	case c == 'a' || c == 'e' || c == 'i' || c == 'o' || c == 'u':
		return classVowel
	case c >= 'a' && c <= 'z':
		return classConsonant
	case c >= 'A' && c <= 'Z':
		return classUpper
	case c >= '0' && c <= '9':
		return classDigit
	}
	return classOther
}

// mixesCase reports whether s holds both an upper-case and a lower-case ASCII letter.
func mixesCase(_ *meter.Meter, s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' }) &&
		strings.ContainsFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' })
}

// isHex reports whether s holds only hexadecimal digits.
func isHex(wm *meter.Meter, s string) bool {
	for i := range len(s) {
		tick(wm, 1)
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// mixesLettersAndDigits reports whether s holds both an ASCII letter and a digit.
func mixesLettersAndDigits(wm *meter.Meter, s string) bool {
	var letter, digit bool
	for i := range len(s) {
		tick(wm, 1)
		c := s[i]
		letter = letter || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit = digit || c >= '0' && c <= '9'
	}
	return letter && digit
}

// quotedEnd returns the index just past the closing quote of the quoted value starting at pos, honouring
// backslash escapes in double quotes and doubled quotes in single quotes, or -1 if the line ends first. A
// backslash before a line break ("\n" or "\r\n") escapes it, as in YAML, so the value goes on past that line.
func quotedEnd(wm *meter.Meter, s string, pos int) int {
	q := s[pos]
	for i := pos + 1; i < len(s); i++ {
		tick(wm, 1)
		switch c := s[i]; {
		case c == '\n' || c == '\r':
			return -1
		case q == '"' && c == '\\':
			i++
			if i+1 < len(s) && s[i] == '\r' && s[i+1] == '\n' {
				i++
			}
		case c == q:
			if q == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i + 1
		}
	}
	return -1
}

// escapeLevel returns n when the value at pos opens with a double quote escaped n times (2^n - 1
// backslashes and a quote: JSON inside a JSON string, once, twice or three times), for n from 1 to 3, or 0.
func escapeLevel(wm *meter.Meter, s string, pos int) int {
	r := 0
	for pos+r < len(s) && s[pos+r] == '\\' && r < 8 {
		tick(wm, 1)
		r++
	}
	if pos+r >= len(s) || s[pos+r] != '"' {
		return 0
	}
	switch r {
	case 1:
		return 1
	case 3:
		return 2
	case 7:
		return 3
	}
	return 0
}

// escapedQuotedSpan returns the value of a double-quoted string escaped level times inside other JSON
// strings, such as the value in "{\"clientSecret\":\"a,b\"}" (level 1), whose escaped opening quote starts
// at pos. A quote preceded by a run of r backslashes closes the string of the level j given by the lowest
// zero bit of r (r mod 2^(j+1) == 2^j - 1): j == level closes the value, a lower j ends an enclosing
// string first (the value then runs up to it), and a higher j is an escaped quote inside the value. So a
// comma, or an inner escaped quote, does not end the value. A line break ends it too.
func escapedQuotedSpan(wm *meter.Meter, s string, pos, level int) (int, int) {
	k := 1<<level - 1
	start := pos + k + 1
	run := 0
	for i := start; i < len(s); i++ {
		tick(wm, 1)
		switch s[i] {
		case '\\':
			run++
			continue
		case '"':
			j := 0
			for run%(2<<j) != 1<<j-1 {
				tick(wm, 1)
				j++
			}
			if j == level {
				return start, i - k
			}
			if j < level {
				return start, i - run
			}
		case '\n', '\r':
			return start, i - run
		}
		run = 0
	}
	return start, len(s)
}

// Value boundaries for the key-value rules, one per format.
const (
	// boundaryWhitespace: env files, logfmt and free text. The value runs to the next whitespace, so
	// punctuation inside it ("&", ";", ",") is redacted with it.
	boundaryWhitespace = iota
	// boundaryFlow: JSON and YAML flow mappings. An unquoted value ends at ",", "}", "]" or the line end.
	boundaryFlow
	// boundaryLine: YAML block mappings. A plain value runs to the line end, plus continuation lines
	// indented deeper than the key.
	boundaryLine
	// boundaryFlowWord: an unquoted value under a bare short key (bareShortColon) in a flow mapping. It is
	// the value boundaryFlow gives when that holds no blank; otherwise it is cut just past the first text
	// after a blank, which still holds the blank, so the value is refused all the same (looksLikeSecret). A
	// refused value does not move the rule past it, so reading on to the next flow delimiter would read the
	// same tail again for every key on the line (#526); this reads only the first word and the blanks after it.
	boundaryFlowWord
)

// valueSpan returns the value that starts at pos, after a key and its separator, for the given format.
// A quoted value is the text between its quotes; an unterminated quote is redacted to the boundary. A value
// in escaped quotes (JSON inside a JSON string, escaped up to three times) ends at its own closing quote.
func valueSpan(wm *meter.Meter, s string, pos, boundary, keyCol int) (int, int) {
	if pos >= len(s) {
		return pos, pos
	}
	if level := escapeLevel(wm, s, pos); level > 0 {
		return escapedQuotedSpan(wm, s, pos, level)
	}
	if c := s[pos]; c == '"' || c == '\'' {
		if end := quotedEnd(wm, s, pos); end > 0 {
			return pos + 1, end - 1
		}
	}
	return plainValueSpan(wm, s, pos, boundary, keyCol)
}

// extendedValueSpan is valueSpan with the extent rule (#708) for a key that is no bare short key: after JSON
// comments in flow form the value that follows them goes with them; an array or object goes through its
// closing bracket (ctx is the escape level of the key's quotes); a quoted value may run over line ends; and
// in free text (boundaryWhitespace) a value is the whole shell word, glued quoted pieces and "$(" included.
// A quoted string crosses a line end only to a line indented deeper than the key that starts at keyPos
// (groupEnd). Where the extent rule gives no extent, the value is valueSpan's. With wide false the extents
// #769 widens are read as on main before it (keyValueSpans).
func extendedValueSpan(wm *meter.Meter, s string, pos, boundary, keyCol, ctx, keyPos int, wide bool, w *wordMemo) (int, int) {
	tick(wm, 1) // a call, which may be a turn of its recursion
	if pos >= len(s) {
		return pos, pos
	}
	if boundary == boundaryFlow {
		if v := commentEnd(wm, s, pos); v > pos {
			_, end := extendedValueSpan(wm, s, v, boundary, keyCol, ctx, keyPos, wide, w)
			return pos, max(end, v)
		}
	}
	if start, end, ok := bracketSpan(wm, s, pos, ctx, wide); ok {
		return start, end
	}
	if boundary == boundaryWhitespace && escapeLevel(wm, s, pos) == 0 && !opensGroup(wm, s, pos) {
		if end := plainWordEnd(wm, s, pos, extentLimit(wm, s, pos), keyPos, wide, nil); end > pos {
			return pos, end
		}
	}
	if escapeLevel(wm, s, pos) == 0 && opensGroup(wm, s, pos) {
		if boundary == boundaryWhitespace {
			if start, end, ok := quotedWordSpan(wm, s, pos, keyPos, s[keyPos] == '-', wide, w); ok {
				return start, end
			}
		} else if c := s[pos]; c == '"' || c == '\'' {
			if quotedEnd(wm, s, pos) < 0 {
				if end, ok := groupEnd(wm, s, pos, extentLimit(wm, s, pos), 0, keyPos, false); ok {
					return pos + 1, end - 1
				}
			}
		}
	}
	return valueSpan(wm, s, pos, boundary, keyCol)
}

// keyQuoteLevel returns the escape level of the quote that opens the key at pos ("\"password\"" is 1), or 0.
func keyQuoteLevel(wm *meter.Meter, s string, pos int) int {
	run := 0
	for pos+run < len(s) && s[pos+run] == '\\' && run < 8 {
		tick(wm, 1)
		run++
	}
	if pos+run < len(s) && s[pos+run] == '"' {
		return quoteLevel(wm, run)
	}
	return 0
}

// argumentEnd returns, for "--name=value" in free text inside a quoted argument that opens right before the
// flag ("[\"--client-secret=horse battery staple\"]", also escaped as JSON inside a string), the end of the value
// at the quote that closes the argument, on the same line and within maxValueExtent bytes (#708): the
// argument is one word to the program, blanks and other quotes included ("[\"--password='a b' c\"]": the
// single quotes are literal). It returns 0 for any other key, or when the value opens with the argument's own
// quote, or the argument does not close. A value that opens with a backslash runs to the closing quote too
// (#769): a backslash escaped as the argument's level is literal text, and a quote escaped deeper than the
// argument does not close it. With wide false such a value gives 0, as on main before #769 (keyValueSpans).
func argumentEnd(wm *meter.Meter, s string, m []int, wide bool) int {
	pos := m[1]
	if s[m[2]] != '-' || indexByte(wm, s[m[3]:pos], '=') < 0 || pos >= len(s) || !wide && s[pos] == '\\' {
		return 0
	}
	level, ok := enclosingQuote(wm, s, m[2])
	if !ok {
		return 0
	}
	q := s[m[2]-1]
	if s[pos] == q {
		return 0
	}
	return stringCloseFrom(wm, s, pos, q, level)
}

// enclosingStringEnd returns, for a key that opens right after a quote (enclosingQuote: a JSON or YAML
// quoted string such as an argument "--client-secret=..."), the index of the quote that closes that string,
// read from the key, on the same line and within maxValueExtent bytes of the key; -1 when the key opens after
// no quote, the string does not close there, or it closes before the value at pos (a quoted JSON or YAML key
// such as "password": the value is outside the key's string). The extent of a value read from inside such a
// string never crosses that quote, whatever shell syntax is open inside the string (clampToString).
func enclosingStringEnd(wm *meter.Meter, s string, keyStart, pos int) int {
	level, ok := enclosingQuote(wm, s, keyStart)
	if !ok {
		return -1
	}
	if end := stringCloseFrom(wm, s, keyStart, s[keyStart-1], level); end >= pos {
		return end
	}
	return -1
}

// clampToString cuts the extent xe of a value back to the closing quote at se of the string the key sits in
// (enclosingStringEnd), so an unclosed "$(", backtick, bracket or escape inside the string ends at the
// string's end and the arguments after it stay (["--client-secret=$(echo", "x)", "--region"]).
func clampToString(_ *meter.Meter, xs, xe, se int) (int, int) {
	if se < 0 {
		return xs, xe
	}
	return min(xs, se), min(xe, se)
}

// stringCloseFrom returns the index of the first quote q from pos that closes a string opened at escape
// level level (a single quote has no escapes), on the same line and within maxValueExtent bytes, or 0.
func stringCloseFrom(wm *meter.Meter, s string, pos int, q byte, level int) int {
	run := 0
	for i := pos; i < extentLimit(wm, s, pos); i++ {
		tick(wm, 1)
		switch c := s[i]; {
		case c == '\n' || c == '\r':
			return 0
		case c == '\\':
			run++
			continue
		case c == q && (q == '\'' || quoteLevel(wm, run) <= level):
			return i - run
		}
		run = 0
	}
	return 0
}

// plainValueSpan returns the unquoted value at pos by the boundary of its format (valueSpan).
func plainValueSpan(wm *meter.Meter, s string, pos, boundary, keyCol int) (int, int) {
	end := pos
	switch boundary {
	case boundaryWhitespace:
		for end < len(s) && !isSpace(wm, s[end]) {
			tick(wm, 1)
			end++
		}
	case boundaryFlow:
		end = len(s)
		if i := indexAny(wm, s[pos:], ",}]\r\n"); i >= 0 {
			end = pos + i
		}
		for end > pos && isSpace(wm, s[end-1]) {
			tick(wm, 1)
			end--
		}
	case boundaryFlowWord:
		for end < len(s) && !isSpace(wm, s[end]) && strings.IndexByte(",}]", s[end]) < 0 {
			tick(wm, 1)
			end++
		}
		next := end
		for next < len(s) && isSpace(wm, s[next]) && s[next] != '\r' && s[next] != '\n' {
			tick(wm, 1)
			next++
		}
		if next < len(s) && strings.IndexByte(",}]\r\n", s[next]) < 0 {
			end = next + 1
		}
	case boundaryLine:
		le := lineEnd(wm, s, pos)
		end = le
		for end > pos && isSpace(wm, s[end-1]) {
			tick(wm, 1)
			end--
		}
		if end == pos {
			break
		}
		if _, cont := indentedLines(wm, s, le, keyCol); cont > 0 {
			end = cont
		}
	}
	return pos, end
}

// plainLineRefused reports, for a bare short key, whether the plain value at pos that a line ends
// (boundaryLine) holds a blank and so does not look like a secret (looksLikeSecret): a blank on its line, or a
// continuation line below it. It reads the value's line and the first line below it, never the whole
// continuation, so a key on each of lines that are each indented one more than the last ("pass: | x") costs
// constant time per line instead of a read to the end of the text (#845). A quoted value that closes is not
// plain (false); one that does not close is, as valueSpan reads it.
func plainLineRefused(wm *meter.Meter, s string, pos, keyCol int) bool {
	if pos >= len(s) || escapeLevel(wm, s, pos) > 0 || (s[pos] == '"' || s[pos] == '\'') && quotedEnd(wm, s, pos) > 0 {
		return false
	}
	le := lineEnd(wm, s, pos)
	end := le
	for end > pos && isSpace(wm, s[end-1]) {
		tick(wm, 1)
		end--
	}
	for i := pos; i < end; i++ {
		tick(wm, 1)
		if s[i] <= ' ' {
			return true
		}
	}
	_, _, cont := nextBlockLine(wm, s, le, keyCol, blockScalar)
	return end > pos && cont
}

// isBlockHeader reports whether a YAML block scalar indicator ("|", ">-", "|2+") starts at pos and is
// followed only by blanks or a comment up to the line end. It reads only the indicator and the blanks after
// it, so it costs constant time per key on any input.
func isBlockHeader(wm *meter.Meter, s string, pos int) bool {
	end := pos
	for end < len(s) && end-pos < 3 && strings.IndexByte("|>123456789+-", s[end]) >= 0 {
		tick(wm, 1)
		end++
	}
	if !blockIndicator.MatchString(counted(wm, s[pos:end])) {
		return false
	}
	blank := end
	for blank < len(s) && (s[blank] == ' ' || s[blank] == '\t') {
		tick(wm, 1)
		blank++
	}
	return blank == len(s) || s[blank] == '\n' || s[blank] == '\r' || (s[blank] == '#' && blank > end)
}

// isSpace reports whether c is ASCII whitespace.
func isSpace(_ *meter.Meter, c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// keyValueSpans returns the spans function of a key-value rule whose regex matches the key (group 1) and its
// separator; the value is found by valueSpan with the given boundary. Keys that are references are left
// alone without reading their value, so a reference never hides a neighbouring secret. A key inside a value
// already read is skipped, so each byte is scanned once. A bare YAML block scalar indicator is left to
// the block scalar rule, and a YAML comment over a block on the following lines to the next-line rule
// (commentOverBlock). A bare short key followed by a colon counts only with a value that looks like a
// secret (bareShortColon); a quoted value may hold blanks (looksLikeQuotedSecret), and in flow form an
// unquoted one is read only up to the text after its first blank (boundaryFlowWord). A refused value
// leaves prevEnd where it was. Any other key takes the union of valueSpan's value and the extent rule's
// (extendedValueSpan, argumentEnd, #708), and after ":=" also the value main read from its "=", so the
// value is never shorter than main's (TestExtentStillRedactsWhatMainRedacted).
//
// The extents #769 widens never hide a key (TestMonotonicAgainstMain): the end returned, before which later
// keys of the rule are skipped, is the end of the value read without them (wide false), as on main. A key in
// the part they add is matched as on main, and its own value redacted.
func keyValueSpans(boundary int, envName bool) func() spansFunc {
	return func() spansFunc {
		w := &wordMemo{}
		return func(wm *meter.Meter, s string, m []int, prevEnd int, add func(start, end int)) int {
			return keyValueSpan(wm, s, m, prevEnd, add, boundary, envName, w)
		}
	}
}

// keyValueSpan is keyValueSpans for one match, with the memory of the shell words read in this pass of the
// rule over s.
func keyValueSpan(wm *meter.Meter, s string, m []int, prevEnd int, add func(start, end int), boundary int, envName bool, w *wordMemo) int {
	if m[2] < prevEnd || keepReference(wm, s, m) || sasRefused(wm, s, m, !envName) {
		return prevEnd
	}
	if isBlockHeader(wm, s, m[1]) || commentOverBlock(wm, s, m) {
		return prevEnd
	}
	bare := bareShortColon(wm, s, m)
	b := boundary
	if bare && b == boundaryFlow {
		b = boundaryFlowWord
	}
	if bare && b == boundaryLine && plainLineRefused(wm, s, m[1], m[2]-m[0]) {
		return prevEnd
	}
	start, end := valueSpan(wm, s, m[1], b, m[2]-m[0])
	if bare {
		if v := s[start:end]; start > m[1] && !looksLikeQuotedSecret(wm, v) || start == m[1] && !looksLikeSecret(wm, v) {
			return prevEnd
		}
		add(start, end)
		return skipEnd(wm, prevEnd, start, end)
	}
	if i := index(wm, s[m[3]:m[1]], ":="); i >= 0 {
		// Main read Go's ":=" as ":" and a value that opens with "=": that value stays redacted.
		ms, me := valueSpan(wm, s, m[3]+i+1, b, m[2]-m[0])
		start, end = min(start, ms), max(end, me)
	}
	ws, we := keyValueExtent(wm, s, m, b, start, end, true, w)
	add(ws, we)
	ms, me := keyValueExtent(wm, s, m, b, start, end, false, w)
	return skipEnd(wm, prevEnd, ms, me)
}

// keyValueExtent returns the union of the value start, end and the extent rule's value after the key of m,
// with the extents #769 widens when wide is true.
func keyValueExtent(wm *meter.Meter, s string, m []int, boundary, start, end int, wide bool, w *wordMemo) (int, int) {
	xs, xe := extendedValueSpan(wm, s, m[1], boundary, m[2]-m[0], keyQuoteLevel(wm, s, m[2]), m[2], wide, w)
	xs, xe = clampToString(wm, xs, xe, enclosingStringEnd(wm, s, m[2], m[1]))
	return min(start, xs), max(end, xe, argumentEnd(wm, s, m, wide))
}

// skipEnd returns the end a value from start to end gives the matches after it: its end when it is not
// empty, else prevEnd, as a rule's span does.
func skipEnd(_ *meter.Meter, prevEnd, start, end int) int {
	if end > start {
		return end
	}
	return prevEnd
}

// flagValueSpan is the span function of the command-line flag rule: a secret-named flag ("--client-secret",
// "--aws-secret-access-key", "--password", "--secret-id", "--sas", or with one dash and two or more letters,
// as Go programs take them: "-password", "-token"), the separator after it (flagSeparatorExpr) and then its
// value (keyNameValue). The key name is the signal: the value is redacted whatever it reads as or starts
// with, so a key that the long-token rule would keep in part (TestSlashSplitBase64KnownLeaks) goes whole, and
// so does "--Hunter2pw" or the next flag ("--use-token --verbose" loses "--verbose", accepted
// over-redaction). Each flag is judged on its own: one taken as the value of an earlier flag still has its
// own value redacted ("--enable-auth --client-secret x"). The flag counts wherever its dash does not follow a
// letter or a digit ("EXTRA_ARGS=--client-secret x" counts, "a--token x" and "my-token x" do not; after
// another "-" the match starts at that dash). A reference flag ("--secret-name", "--token-ttl",
// "--password-file", "--password-stdin") keeps its value. A value that ends inside the previous value is
// skipped, as it is redacted already; one that starts inside it and runs past its end (a quote opened inside
// the previous value) is redacted to its own end.
func flagValueSpan(wm *meter.Meter, s string, m []int, prevEnd int, tm *tokenMemo) (int, int, bool) {
	switch {
	case m[0] > 0 && isAlnum(wm, s[m[0]-1]), referenceKey(wm, s[m[2]:m[3]], true):
		return 0, 0, false
	case m[2] == m[0]+1 && !isLetter(wm, s[m[2]]):
		// One dash: the flag needs a letter after it ("-password"), as "-_x" or "-1" is no flag.
		return 0, 0, false
	}
	sep := s[m[3]:m[1]]
	q := strings.TrimLeft(sep, `\`)
	closed := q != "" && (q[0] == '"' || q[0] == '\'')
	start, end := flagExtent(wm, s, m, closed, tm)
	// A value that lies inside the previous value is redacted already; one that runs past it is not.
	return start, end, end > prevEnd
}

// flagExtent returns the value after the secret flag of m by the extent rule (#708): in a YAML block sequence
// ("- --password" then "- |" on the next line) a block scalar is its indented lines, with an anchor or tag
// before its indicator ("- &pw |-") going with them; in a block or flow
// sequence an anchor or tag before the value goes with it, and the value after them is the rest of its item
// (the line, in a block sequence); in a flow sequence ("[--password, a b]") a plain
// value runs to the end of its item; otherwise the value is keyNameValue's, a quoted or "$(" group with the
// pieces glued to it included. A value that is an item of a JSON array or a YAML sequence is no shell word:
// a quoted item ends at its closing quote, whatever it holds (extLiteral). closed is as in keyNameValue.
//
// The value is the union of keyNameValue's without the extent rule (the value main gave) and the extent
// rule's, so it is never shorter than main's (TestExtentStillRedactsWhatMainRedacted).
func flagExtent(wm *meter.Meter, s string, m []int, closed bool, tm *tokenMemo) (int, int) {
	start, end := keyNameValue(wm, s, m[1], m[0], closed, extNone, tm)
	xs, xe := flagExtendedValue(wm, s, m, closed, tm)
	xs, xe = clampToString(wm, xs, xe, enclosingStringEnd(wm, s, m[0], m[1]))
	if xe > xs {
		start, end = min(start, xs), max(end, xe)
	}
	return start, end
}

// flagExtendedValue is the extent rule's part of flagExtent.
func flagExtendedValue(wm *meter.Meter, s string, m []int, closed bool, tm *tokenMemo) (int, int) {
	sep := s[m[3]:m[1]]
	pos := m[1]
	list := strings.IndexByte(sep, '-') >= 0
	flow := strings.IndexByte(sep, ',') >= 0 && inFlowSequence(wm, s, m[0])
	ext := extShell
	if list || flow || closed && strings.IndexByte(sep, ',') >= 0 {
		ext = extLiteral
	}
	if list {
		if v := nodeProperties(wm, s, pos); isBlockHeader(wm, s, v) {
			nl := m[3] + strings.LastIndexAny(sep, "\r\n") + 1
			start, end := listBlockScalarSpan(wm, s, v, indexByte(wm, s[nl:pos], '-'), tm)
			if v > pos {
				start = pos
			}
			return start, end
		}
	}
	if list || flow {
		if v := nodeProperties(wm, s, pos); v > pos {
			_, end := keyNameValue(wm, s, v, m[0], true, ext, tm)
			switch {
			case opensGroup(wm, s, v):
			case flow:
				end = max(end, flowItemEnd(wm, s, v))
			default:
				end = max(end, lineItemEnd(wm, s, v))
			}
			return pos, end
		}
	}
	start, end := keyNameValue(wm, s, pos, m[0], closed, ext, tm)
	if start == pos && pos < len(s) && !opensGroup(wm, s, pos) && s[pos] != '\\' {
		switch {
		case flow:
			end = max(end, flowItemEnd(wm, s, pos))
		case list && blockSequenceItem(wm, s, m):
			end = max(end, lineItemEnd(wm, s, pos))
		}
	}
	return start, end
}

// blockSequenceItem reports whether the secret flag of m is an item of a YAML block sequence ("- --password",
// after indentation at the start of its line) and its value the next item of the same sequence: the dash of
// the value's line is in the column of the flag's dash (#769). A flag that is no item ("--password" then
// "  - x" on the next line) gives false, so only the first token after the dash goes.
func blockSequenceItem(wm *meter.Meter, s string, m []int) bool {
	sep := s[m[3]:m[1]]
	nl := strings.LastIndexAny(sep, "\r\n")
	if nl < 0 {
		return false
	}
	valueDash := strings.IndexByte(sep[nl+1:], '-')
	if valueDash < 0 {
		return false
	}
	i := m[0]
	for i > 0 && m[0]-i < maxValueExtent && (s[i-1] == ' ' || s[i-1] == '\t') {
		tick(wm, 1)
		i--
	}
	if i == m[0] || i == 0 || s[i-1] != '-' {
		return false
	}
	dash := i - 1
	j := dash
	for j > 0 && dash-j < maxValueExtent && (s[j-1] == ' ' || s[j-1] == '\t') {
		tick(wm, 1)
		j--
	}
	if j > 0 && s[j-1] != '\n' && s[j-1] != '\r' {
		return false
	}
	return dash-j == valueDash
}

// compoundValueSpans is the spans function of the rule for a key name of two or more words, a blank and the
// value after it (keyNameValue), as in "aws configure set aws_secret_access_key x" (#585). The words are
// joined by "_" ("client_secret", "STORAGE_SAS", "SECRET_KEY_BASE"), by "-" ("client-secret") or in camelCase
// ("clientSecret", "ClientSecret", "SecretId"). The name rules are those of the other forms: a secret word
// anywhere in the name and no reference suffix (referenceKey; "client_id" and "access_key_id" keep their
// value, "secret_id" and "password_stdin" do not, as "stdin" is a reference only for a flag). The key must
// start a word, and a one-word name ("secret", "token", "password") never counts in this form, so "kubectl
// get secret foo" and "kubectl create token sa" stay readable. A key name before a blank is as often a word
// of a sentence, a type or a column of a table ("db_password not set", "ClientSecretCredential
// AuthenticationFailed", "db-password   kubernetes.io/basic-auth"), so this form exists only for an opaque
// key after the name: the value is redacted only when the whole of it is an opaque token (opaqueToken). A
// shorter or otherwise shaped value ("hunter2", "Password!") stays (TestKeyNameKnownLeaks). A name joined by
// "-" or in camelCase is also how Kubernetes names a Secret ("kubectl describe secret db-password -n prod")
// or a volume ("volumes=[secret-volume kube-api-access-x]"), so after such a name a value that reads as a flag
// ("--from-literal=x=y", flagWord) is kept, and so is the value after a name ending in "volume"
// (blankReferenceSuffixes). It also takes a flag ("-client_secret x", "--client_secret x", which the flag
// rule takes too), and leaves "=" or ":" after the blanks to the key-value rules. A bare short key with a
// prose prefix ("re-auth") does not count.
//
// The value of a counted name is taken whether or not it is redacted, and so is the value after a
// secret-named flag ("--password x", which the flag rule redacts): a match that starts inside a value already
// taken is skipped ("client_secret db_password --region west" does not read "db_password" as a name), except
// a secret-named flag, which is judged on its own. Each value is read once, so the cost stays linear.
func compoundValueSpans(wm *meter.Meter, s string, m []int, prevEnd int, add func(start, end int), tm *tokenMemo) int {
	name := s[m[2]:m[3]]
	key := strings.TrimLeft(name, "-")
	dashes := len(name) - len(key)
	flag := dashes > 0 && key != "" && (dashes == 2 || dashes == 1 && isLetter(wm, key[0]))
	switch {
	case m[2] < prevEnd && !flag, m[2] > 0 && isTokenByte(wm, s[m[2]-1]),
		m[1] < len(s) && (s[m[1]] == '=' || s[m[1]] == ':'), referenceKey(wm, name, dashes > 0):
		return prevEnd
	}
	joined := strings.Contains(key, "_")
	spelled := !joined && !isBareShortKey(wm, key) &&
		(strings.Contains(key, "-") && !strings.Contains(key, "--") || isCamelCase(wm, key))
	named := (joined || spelled) && !hasSuffixFold(wm, keySeparators.Replace(key), blankReferenceSuffixes)
	if !named && !flag {
		return prevEnd
	}
	start, end := keyNameValue(wm, s, m[1], m[2], false, extNone, tm)
	if v := s[start:end]; named && opaqueToken(wm, v) && (!spelled || !flagWord(wm, v)) {
		add(start, end)
	}
	return max(prevEnd, end)
}

// blankReferenceSuffixes mark key names that, before a blank and a value, name a Kubernetes object rather than
// hold a secret: "secret-volume" is the name of a volume (#585). In the other forms such a name holds a secret
// as before.
var blankReferenceSuffixes = []string{"volume"}

// hasSuffixFold reports whether key ends in one of suffixes, in any case.
func hasSuffixFold(wm *meter.Meter, key string, suffixes []string) bool {
	key = strings.ToLower(key)
	for _, suffix := range suffixes {
		tick(wm, 1)
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// opaqueToken reports whether v is an opaque key, the one value the blank form redacts: 16 or more
// characters, every one of them from "A-Z a-z 0-9 / + = _ -", and at least one digit or one of "/+=".
func opaqueToken(wm *meter.Meter, v string) bool {
	if len(v) < 16 {
		return false
	}
	marked := false
	for i := range len(v) {
		tick(wm, 1)
		switch c := v[i]; {
		case c >= '0' && c <= '9', c == '/', c == '+', c == '=':
			marked = true
		case !isLetter(wm, c) && c != '_' && c != '-':
			return false
		}
	}
	return marked
}

// isLetter reports whether c is an ASCII letter.
func isLetter(_ *meter.Meter, c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isCamelCase reports whether key joins words in camelCase: it starts with an ASCII letter and has a
// lower-case letter or a digit right before an upper-case one ("clientSecret", "dbPassword", "ClientSecret").
func isCamelCase(wm *meter.Meter, key string) bool {
	if key == "" || !isLetter(wm, key[0]) {
		return false
	}
	for i := 1; i < len(key); i++ {
		tick(wm, 1)
		if key[i] >= 'A' && key[i] <= 'Z' && (key[i-1] >= 'a' && key[i-1] <= 'z' || key[i-1] >= '0' && key[i-1] <= '9') {
			return true
		}
	}
	return false
}

// flagWord reports whether v reads as a command-line flag: one or two dashes, then a lower-case letter, then
// only lower-case letters, digits and dashes, and optionally "=" and its value ("-n", "--namespace",
// "--dry-run", "--from-literal=x=y").
func flagWord(wm *meter.Meter, v string) bool {
	v, _, _ = strings.Cut(v, "=")
	w := strings.TrimPrefix(v, "-")
	if w == v {
		return false
	}
	w = strings.TrimPrefix(w, "-")
	if w == "" || w[0] < 'a' || w[0] > 'z' {
		return false
	}
	for i := range len(w) {
		tick(wm, 1)
		if c := w[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// keyNameValue returns the value after a secret flag or a compound key name and a blank (#585), from pos;
// the name starts at keyStart. A value that opens with a quote, plain or escaped as JSON inside a JSON string
// (escapeLevel), runs to its closing quote on the same line, with escapes handled (quotedEnd,
// escapedQuotedSpan); when that quote does not close, the value is the quote and the token after it. Any
// other value is one unquoted token (tokenEnd). A value never starts in the middle of a quoted string it did
// not open: when a quote opens right before the name and the separator did not close it (closed is false),
// the same quote at pos closes that string, and the value is empty. Each call reads forward from pos to the
// end of its line at most (maxValueExtent bytes with ext), and back from keyStart over at most eight
// backslashes.
//
// With extShell (a flag's value on a command line, #708), a value that opens a quote, "$(" or a backtick is
// the whole shell word by the extent rule (quotedWordSpan), over line ends and with glued pieces; with
// extLiteral (an item of a JSON array or YAML sequence) a value that opens a quote is that quoted string, over
// line ends, read literally (groupEnd). When that group does not close within maxValueExtent, a value that
// opens a quote takes the rest of its line (unclosedQuoteEnd), and any other the rules above. The compound
// rule passes extNone: an opaque token holds no quote or blank, so a longer extent could not change what it
// redacts.
func keyNameValue(wm *meter.Meter, s string, pos, keyStart int, closed bool, ext extentMode, tm *tokenMemo) (int, int) {
	if pos >= len(s) {
		return pos, pos
	}
	level := escapeLevel(wm, s, pos)
	if !closed {
		if enclosing, ok := enclosingQuote(wm, s, keyStart); ok && enclosing == level && s[pos+1<<level-1] == s[keyStart-1] {
			return pos, pos
		}
	}
	if ext != extNone && level == 0 {
		if start, end, ok := flagWordSpan(wm, s, pos, ext, tm.plain()); ok {
			return start, end
		}
	}
	if level > 0 {
		start, end := escapedQuotedSpan(wm, s, pos, level)
		q := end
		for q < len(s) && s[q] == '\\' {
			tick(wm, 1)
			q++
		}
		if q < len(s) && s[q] == '"' {
			return start, end
		}
		// The escaped quote does not close on its line: the quote and the token after it, as for a plain one.
		return pos, tm.tokenEnd(wm, s, start)
	}
	if c := s[pos]; c == '"' || c == '\'' {
		if end := quotedEnd(wm, s, pos); end > 0 {
			return pos + 1, end - 1
		}
		return pos, tm.tokenEnd(wm, s, pos+1)
	}
	if end := tm.tokenEnd(wm, s, pos); end > pos || s[pos] != '\\' {
		return pos, end
	}
	// A quote escaped in a way no JSON level gives ("\'"): the quote and the token after it, as for a
	// quote that does not close.
	q := pos
	for q < len(s) && s[q] == '\\' {
		tick(wm, 1)
		q++
	}
	return pos, tm.tokenEnd(wm, s, q+1)
}

// flagWordSpan is the extent rule's part of keyNameValue for a value at pos that opens with no escaped quote:
// with extShell a shell word (plainWordEnd, quotedWordSpan), with extLiteral a quoted string read literally
// (groupEnd); a quote that does not close within maxValueExtent takes the rest of its line
// (unclosedQuoteEnd). ok is false where the rule gives no extent. pr is as in plainWordEnd.
func flagWordSpan(wm *meter.Meter, s string, pos int, ext extentMode, pr *plainRun) (int, int, bool) {
	if !opensGroup(wm, s, pos) {
		if ext == extShell {
			if end := plainWordEnd(wm, s, pos, extentLimit(wm, s, pos), noKey, true, pr); end > pos {
				return pos, end, true
			}
		}
		return 0, 0, false
	}
	c := s[pos]
	switch {
	case ext == extShell:
		if start, end, ok := quotedWordSpan(wm, s, pos, noKey, true, true, nil); ok {
			return start, end, true
		}
	case c == '"' || c == '\'':
		if end, ok := groupEnd(wm, s, pos, extentLimit(wm, s, pos), 0, noKey, false); ok {
			return pos + 1, end - 1, true
		}
	}
	if c == '"' || c == '\'' {
		if end := unclosedQuoteEnd(wm, s, pos); end > pos {
			return pos, end, true
		}
	}
	return 0, 0, false
}

// extentMode is how keyNameValue reads a value with the extent rule (#708).
type extentMode int

const (
	// extNone reads the value as main did, without the extent rule.
	extNone extentMode = iota
	// extShell reads a value that is a shell word: glued pieces, "$(" and backticks.
	extShell
	// extLiteral reads a value that is an item of a JSON array or YAML sequence: a quoted item is literal.
	extLiteral
)

// flagValueSpanFor returns the span function of the flag rule for one pass over a text (flagValueSpan), with
// its own memory of the last value end read.
func flagValueSpanFor() spanFunc {
	tm := &tokenMemo{}
	return func(wm *meter.Meter, s string, m []int, prevEnd int) (int, int, bool) {
		return flagValueSpan(wm, s, m, prevEnd, tm)
	}
}

// compoundValueSpansFor returns the spans function of the compound key name rule for one pass over a text
// (compoundValueSpans), with its own memory of the last value end read.
func compoundValueSpansFor() spansFunc {
	tm := &tokenMemo{}
	return func(wm *meter.Meter, s string, m []int, prevEnd int, add func(start, end int)) int {
		return compoundValueSpans(wm, s, m, prevEnd, add, tm)
	}
}

// tokenMemo is the last value end tokenEnd read in one pass of a rule over one text, so that the values of a
// run of keys glued with no blank ("-password,-password,...") are read once and not each to the end of the run
// (#845, fifteenth decision, P3); nil keeps nothing. tokenEnd(s, p) is the first stop at or after p, so it is the
// same end for every p from the start of the last read to its end: no stop lies between them.
type tokenMemo struct {
	from, end int
	ok        bool
	// words is the plain run plainWordEnd read furthest in the same pass (#873).
	words plainRun
}

// plain returns the plain run of the pass for plainWordEnd, or nil when tm keeps nothing.
func (tm *tokenMemo) plain() *plainRun {
	if tm == nil {
		return nil
	}
	return &tm.words
}

// tokenEnd is tokenEnd(s, pos), with the memory of the last read.
func (tm *tokenMemo) tokenEnd(wm *meter.Meter, s string, pos int) int {
	if tm == nil {
		return tokenEnd(wm, s, pos)
	}
	if tm.ok && tm.from <= pos && pos <= tm.end {
		tick(wm, 1)
		return tm.end
	}
	end := tokenEnd(wm, s, pos)
	tm.from, tm.end, tm.ok = pos, end, true
	return end
}

// tokenEnd returns the end of the unquoted value that starts at pos, after a flag or a compound key name: the value ends at whitespace, the line end, or a quote character ("\"", "'", or one escaped
// by backslashes), and at nothing else. So a key inside a JSON or logfmt string cannot take the fields after
// it ("{\"msg\":\"ClientSecretCredential AuthenticationFailed\",\"reason\":\"x\"}" keeps "reason"), while ",",
// "]" and "}" stay in the value ("[--password, abc, --x]" loses "abc,"), as one inside the value cannot be
// told apart from one of the text around it ("--password abc,def").
func tokenEnd(wm *meter.Meter, s string, pos int) int {
	for i := pos; i < len(s); i++ {
		tick(wm, 1)
		switch c := s[i]; {
		case isSpace(wm, c), c == '"', c == '\'':
			return i
		case c == '\\':
			j := i
			for j < len(s) && s[j] == '\\' {
				tick(wm, 1)
				j++
			}
			if j < len(s) && (s[j] == '"' || s[j] == '\'') {
				return i
			}
			i = j - 1
		}
	}
	return len(s)
}

// enclosingQuote reports whether a quote opens right before the key at keyStart, and the level of its
// escaping: 0 for a plain quote, 1 to 3 for a double quote escaped as in escapeLevel.
func enclosingQuote(wm *meter.Meter, s string, keyStart int) (int, bool) {
	if keyStart == 0 {
		return 0, false
	}
	q := s[keyStart-1]
	if q != '"' && q != '\'' {
		return 0, false
	}
	r := 0
	for keyStart-1-r > 0 && s[keyStart-2-r] == '\\' && r < 8 {
		tick(wm, 1)
		r++
	}
	switch {
	case r == 0:
		return 0, true
	case q == '\'':
		return 0, false
	case r == 1:
		return 1, true
	case r == 3:
		return 2, true
	case r == 7:
		return 3, true
	}
	return 0, false
}

// commentOverBlock reports whether the value of the key in group 1 of m, at the end of m, is a YAML comment
// ("credentials: # set by CI") over a block on the following lines, which the next-line rule redacts so
// that reference children keep their names: a "#" after a colon and a blank, a key first on its line, and
// a line of its block after it. A "#" with no such block, or after "=", counts as the value. Only one key
// per line is first on it, so the line is read at most once.
func commentOverBlock(wm *meter.Meter, s string, m []int) bool {
	pos := m[1]
	if pos >= len(s) || s[pos] != '#' || pos == 0 || (s[pos-1] != ' ' && s[pos-1] != '\t') {
		return false
	}
	sep := pos - 1
	for sep > m[3] && (s[sep] == ' ' || s[sep] == '\t') {
		tick(wm, 1)
		sep--
	}
	if s[sep] != ':' {
		return false
	}
	ls := m[2]
	for ls > 0 && (s[ls-1] == ' ' || s[ls-1] == '\t' || s[ls-1] == '-') {
		tick(wm, 1)
		ls--
	}
	if ls > 0 && s[ls-1] != '\n' {
		return false
	}
	_, _, ok := nextBlockLine(wm, s, lineEnd(wm, s, pos), m[2]-ls, blockValue)
	return ok
}

// Key names that hold secrets, in three forms: bare, optionally quoted (YAML block mappings), and quoted
// or bare after "{" or "," (JSON and YAML flow mappings, also with escaped quotes inside a JSON string).
// The short names in shortKeyExpr count only as a whole key or the last part of one.
const (
	secretWordExpr = `(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|client[_-]?secret|credentials?|connection[_-]?string|conn[_-]?str)`
	bareKeyExpr    = `(?:[A-Za-z0-9_.-]*` + secretWordExpr + `[A-Za-z0-9_.-]*|` + shortKeyExpr + `)`
	// shortKeyNamesExpr lists the short key names (shortKeyNames), joined by "|".
	shortKeyNamesExpr = `pass|auth|sig|sas`
	// shortKeyExpr names short keys that hold secrets but are also parts of other words ("bypass",
	// "author", "signal", "kansas", "sasl"), so they count only as a whole key or as the last part of a
	// longer one after "_", "." or "-" ("DB_PASS", "basic_auth", "AZURE_STORAGE_SAS"). "sas" (an Azure shared
	// access signature) also counts as the last word of a camelCase key, spelled "Sas" ("storageSas",
	// "StorageSas"; not "kansas" or "KANSAS"), and only where sasRefused lets it count.
	shortKeyExpr  = `(?:(?:[A-Za-z0-9_.-]*[_.-]|\b)(?:` + shortKeyNamesExpr + `)|[A-Za-z0-9_.-]*(?-i:[a-z0-9]Sas))`
	secretKeyExpr = `["']?` + bareKeyExpr + `["']?`
	flowKeyExpr   = `\\*["']` + bareKeyExpr + `\\*["']|` + bareKeyExpr
	// nextLineHeaderExpr is a YAML key that names a secret with nothing after its colon but blanks or a
	// comment, after the start of a line.
	nextLineHeaderExpr = `[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:(?:[ \t]+#[^\r\n]*)?[ \t]*\r?$`
	// jsonSpaceExpr is JSON whitespace, also escaped inside a JSON string ("\n", "\t").
	jsonSpaceExpr = `(?:[ \t\r\n]|\\+[nrt])*`
	// flagSpaceExpr is one piece of the separator between a secret-named flag and its value: whitespace,
	// real line ends included, a YAML list dash and a blank only after a line end and indentation, or a
	// backslash directly before a line end (a shell line continuation).
	flagSpaceExpr = `(?:[ \t\v\f]|(?:\r\n?|\n)(?:[ \t]*-[ \t])?|\\(?:\r\n?|\n))`
	// flagSeparatorExpr is what stands between a secret-named flag and its value: a comma directly after the
	// flag name or after its closing quote, plain or escaped (JSON and YAML flow lists, of quoted or plain
	// items), then flagSpaceExpr pieces if any; or the closing quote and flagSpaceExpr pieces; or
	// flagSpaceExpr pieces alone. Nothing else: literal "\n", "\r" and "\t" pairs, a comma after whitespace
	// and a bare "-" are the start of the value.
	flagSeparatorExpr = `(?:(?:\\*["'])?,` + flagSpaceExpr + `*|\\*["']` + flagSpaceExpr + `+|` + flagSpaceExpr + `+)`
)

// Quoted value forms for the Authorization rule: double-quoted with backslash escapes, and single-quoted
// with doubled quotes (YAML).
const (
	doubleQuotedExpr = `"((?:[^"\\\r\n]|\\.)*)"`
	singleQuotedExpr = `'((?:[^'\r\n]|'')*)'`
)

func builtinRules(_ *meter.Meter) []rule {
	return []rule{
		// PEM private keys, also when truncated before the END line.
		{kind: KindPrivateKey, re: regexp.MustCompile(
			`(?s)-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY( BLOCK)?-----.*?(?:-----END [A-Z0-9 ]{0,40}PRIVATE KEY( BLOCK)?-----|\z)`)},
		// JSON Web Tokens: a base64url header starting with '{"', any payload, optional signature.
		{kind: KindJWT, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)},
		// Well-known token formats with a recognisable prefix.
		{kind: KindToken, re: regexp.MustCompile(`\b(?:` + strings.Join([]string{
			`gh[pousr]_[A-Za-z0-9]{36,255}`,                 // GitHub
			`github_pat_[A-Za-z0-9_]{22,255}`,               // GitHub fine-grained
			`glpat-[A-Za-z0-9_-]{20,}`,                      // GitLab
			`(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}`,           // AWS access key ID
			`xox[abposr]-[A-Za-z0-9-]{10,}`,                 // Slack
			`xapp-[A-Za-z0-9-]{10,}`,                        // Slack app
			`AIza[0-9A-Za-z_-]{35}`,                         // Google API key
			`ya29\.[0-9A-Za-z_-]{20,}`,                      // Google OAuth access token
			`[rs]k_(?:live|test)_[0-9A-Za-z]{16,}`,          // Stripe
			`sk-(?:proj-|ant-|svcacct-)?[A-Za-z0-9_-]{20,}`, // model provider API keys
			`npm_[A-Za-z0-9]{36}`,                           // npm
			`pypi-[A-Za-z0-9_-]{50,}`,                       // PyPI
			`dop_v1_[a-f0-9]{64}`,                           // DigitalOcean
			`hv[sb]\.[A-Za-z0-9_-]{20,}`,                    // HashiCorp Vault
		}, "|") + `)`)},
		// Microsoft Entra client secrets of the current format: three characters, a digit and "Q~" (mostly
		// "8Q~"), then 31 to 34 characters, 40 in all, of letters, digits and "_~.-". Matched as a whole here,
		// because "~" and "." cut it into parts the long-token rule may keep (#581). No word boundary before
		// it: the three characters before the marker are taken, whatever is glued in front. After the 31 to
		// 34 characters, the match runs on to the end of the run of those characters, though not onto a last
		// ".", so a secret glued to another is taken with it instead of leaving the other's tail, while a full
		// stop after a secret stays. "~" may be percent-encoded ("%7E"), as a URL encoder writes it. The
		// minimum length keeps Windows short names ("APP8Q~1"), tilde version ranges and
		// dotted names out; a secret cut to 36 characters or fewer by truncation is not matched.
		{kind: KindToken, re: regexp.MustCompile(
			`[A-Za-z0-9_~.-]{3}[0-9]Q(?:~|%7[Ee])(?:[A-Za-z0-9_~.-]|%7[Ee]){31,34}` +
				`(?:(?:[A-Za-z0-9_~.-]|%7[Ee])*(?:[A-Za-z0-9_~-]|%7[Ee]))?`)},
		// URLs with any userinfo part, such as postgres://user:pass@host/db, https://token:@host/repo or
		// https://token@host/repo: a user name alone may be a token. The user info may hold "<" and ">" (a
		// password such as "Hunter<2"), which a JSON or HTML encoder would have escaped and text read raw
		// does not (#840); it still ends at "/", so it never reaches into the next URL. The host part ends at
		// "<" and ">", so a bracket of the markup around the URL ("<postgres://u:p@h/x>") stays.
		// A list of URLs in angle brackets ("<https://example.com>,<mailto:ops@example.com>") is one match up to
		// the "@", as on main: property 1 of #845 has no exceptions, and showing more is #860.
		{kind: KindConnectionString, re: regexp.MustCompile(
			`(?i)\b[a-z][a-z0-9+.-]{1,30}://[^\s/?#@'"]+@[^\s'"<>]+`)},
		// Secret fields of semicolon-separated connection strings (Azure Storage, Service Bus, SQL Server):
		// quoted with doubled quotes, or bare up to the next ";" or line end, spaces included.
		{kind: KindConnectionString, groups: []int{1, 2, 3}, re: regexp.MustCompile(
			`(?i)\b(?:AccountKey|SharedAccessKey|SharedAccessSignature|SharedSecretValue|Password|Pwd)[ \t]*=[ \t]*` +
				`(?:"((?:[^"\r\n]|"")*)"|'((?:[^'\r\n]|'')*)'|([^;\r\n]+))`)},
		// Authorization headers: the whole value, scheme and every parameter (SigV4, Digest), quoted or up
		// to the line end.
		{kind: KindCredential, groups: []int{1, 2, 3}, re: regexp.MustCompile(
			`(?i)\b(?:proxy-)?authorization["']?[ \t]*[:=][ \t]*(?:` + doubleQuotedExpr + `|` + singleQuotedExpr + `|([^\r\n]+))`)},
		// Bearer and Basic credentials outside an Authorization header, when the value has the shape of one.
		{kind: KindCredential, span: schemeSpan, re: regexp.MustCompile(
			`(?i)\b(bearer|basic)[ \t]+([A-Za-z0-9._~+/-]+=*)`)},
		// Credentials in URL query strings, such as Azure SAS signatures and presigned URLs.
		{kind: KindCredential, groups: []int{1}, re: regexp.MustCompile(
			`(?i)[?&](?:sig|signature|token|access_token|refresh_token|id_token|api_key|apikey|key|code|password|secret|client_secret|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature)=([^&\s#"'<>]+)`)},
		// Key-value pairs whose key names a secret, one rule per format so each value ends at its own
		// format's boundary. Env files, logfmt and free text: the value runs to the next whitespace.
		// Go's ":=" is one separator.
		{kind: KindCredential, spansFor: keyValueSpans(boundaryWhitespace, false), re: regexp.MustCompile(
			`(?i)(` + bareKeyExpr + `)[ \t]*(?::=|[:=])[ \t]*`)},
		// Command-line flags whose name holds a secret, with the value after blanks or line ends, in an
		// argument list (JSON, escaped JSON or a YAML list) or after a shell line continuation:
		// "--client-secret <value>", "\"--password\", \"<value>\"", "- --password\n- <value>"
		// (flagValueSpan). "--name=value" is the rule above.
		{kind: KindCredential, spanFor: flagValueSpanFor, re: regexp.MustCompile(
			`(?i)--?(` + bareKeyExpr + `)` + flagSeparatorExpr)},
		// Kubernetes environment variables in flow form with the value before the name (#845, view.go).
		{kind: KindCredential, span: envValueFirstSpan, re: envValueFirst},
		// A key in one string of a sequence and its value in a later one (#845, view.go).
		{kind: KindCredential, scan: seqScan},
		// A key name of two or more words joined by "_", blanks and one token: "aws_secret_access_key <value>"
		// (compoundValueSpans).
		{kind: KindCredential, spansFor: compoundValueSpansFor, re: regexp.MustCompile(
			`(?i)(` + bareKeyExpr + `)[ \t]+`)},
		// JSON and YAML flow mappings: a field after "{" or ",", ending at its closing quote or at , } ].
		// JSON whitespace, newlines included and also escaped, may surround the key and the colon.
		{kind: KindCredential, spansFor: keyValueSpans(boundaryFlow, false), re: regexp.MustCompile(
			`(?i)[{,]` + jsonSpaceExpr + `(` + flowKeyExpr + `)` + jsonSpaceExpr + `:` + jsonSpaceExpr)},
		// Kubernetes environment variables in JSON and YAML flow form: the value after a name that holds a
		// secret, {"name":"DB_PASSWORD","value":"..."}.
		{kind: KindCredential, spansFor: keyValueSpans(boundaryFlow, true), re: regexp.MustCompile(
			`(?i)\\*["']?\bname\\*["']?` + jsonSpaceExpr + `:` + jsonSpaceExpr + `\\*["']?(` + bareKeyExpr + `)\\*["']?` +
				jsonSpaceExpr + `,` + jsonSpaceExpr + `\\*["']?value\\*["']?` + jsonSpaceExpr + `:` + jsonSpaceExpr)},
		// YAML block mappings: a key at the start of a line, its value to the line end plus continuation
		// lines indented deeper than the key.
		{kind: KindCredential, spansFor: keyValueSpans(boundaryLine, false), re: regexp.MustCompile(
			`(?im)^[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:[ \t]+`)},
		// YAML literal and folded block scalars under a secret key, with chomping and indentation indicators.
		{kind: KindCredential, scan: blockScalarScan},
		// YAML block mappings: a key with nothing after its colon but blanks or a comment, its value on the
		// following lines indented deeper than the key.
		{kind: KindCredential, spans: nextLineSpans, re: regexp.MustCompile(`(?im)^` + nextLineHeaderExpr)},
		// Kubernetes environment variables in YAML block form: "- name: DB_PASSWORD" and its "value".
		{kind: KindCredential, span: envValueSpan, re: regexp.MustCompile(
			`(?im)^[ \t]*-[ \t]+name[ \t]*:[ \t]*["']?(` + bareKeyExpr + `)["']?[ \t]*\r?$`)},
		// A secret key and a YAML block scalar indicator wherever the key stands on a line (#840): the block
		// below, and the rest of the line when a block was flattened onto it (anyKeySpans).
		{kind: KindCredential, spans: anyKeySpans, re: anyKey},
		// The block-value rule on the canonical view (#845, view.go): a secret key with a block indicator
		// wherever it stands, indented with any Unicode space or control character, after any line end, with
		// several keys on one line, and a value in a flow collection followed to the end of its brackets.
		{kind: KindCredential, scan: viewBlockSpans},
		// Long random tokens without a known prefix: a run of base64, base64url or hex characters (see
		// longTokenSpans).
		{kind: KindToken, spans: longTokenSpans, re: regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}=*`)},
		// Email addresses.
		{kind: KindEmail, re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`)},
		// IPv4 addresses.
		{kind: KindIP, re: regexp.MustCompile(
			`\b(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\b`)},
		// IPv6 addresses: candidates are confirmed with net/netip, so clock times and similar are kept.
		{kind: KindIP, span: ipv6Span, re: regexp.MustCompile(
			`(?:[0-9A-Fa-f]{0,4}:){2,8}(?:[0-9A-Fa-f]{1,4}|(?:[0-9]{1,3}\.){3}[0-9]{1,3})?`)},
	}
}
