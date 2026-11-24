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

package incidentcontext

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// RedactFunc redacts text. Its output must never be longer than its input, as package redact guarantees.
type RedactFunc func(string) string

// String calls f. A RedactFunc has no flag for a withheld text, so it is not a [Redactor].
func (f RedactFunc) String(s string) string { return f(s) }

// twicer is a [Redactor] that also applies the reading of the release before #845 where that release redacted a
// text, made it valid UTF-8 and redacted it again ([Redact] then; redact.Redactor.StringTwiceChecked, with its flag).
type twicer interface {
	StringTwiceChecked(s string, cut func(string) int) (string, bool)
}

// checkedTwice redacts s as [Twice] of r does (with cut, as StringTwice takes it, when r is a twicer) and reports
// false when redaction withheld it whole. Withheld is told by the redactor's flag, never by the text, so a value
// from the cluster that spells redact.Withheld is text like any other (#873, decision of 2026-11-20). Every
// [Redactor] has the flag (StringChecked), so none is taken for one that never withholds. A text larger than
// redact.LargestEntry is withheld before the redactor reads it, with the reason size ([overLargestEntry]).
func checkedTwice(r Redactor, s string, cut func(string) int) (string, bool) {
	if overLargestEntry(len(s)) {
		return "", false
	}
	if t, ok := r.(twicer); ok {
		return t.StringTwiceChecked(s, cut)
	}
	return r.StringChecked(s)
}

// checkedValue is [Value] of s with [checkedTwice]: false when redaction withheld s whole.
func checkedValue(r Redactor, s string) (string, bool) {
	out, ok := checkedTwice(r, s, nil)
	return Shown(strings.ToValidUTF8(out, "?")), ok
}

// Twice returns the RedactFunc for a place where the release before #845 redacted a text twice ([Redact],
// [Value], a log excerpt): r's StringTwice when it has one, so that what that release hid there is still hidden
// (#845, thirteenth decision of 2026-11-18), and its String otherwise.
func Twice(r Redactor) RedactFunc {
	if t, ok := r.(twicer); ok {
		return func(s string) string {
			out, _ := t.StringTwiceChecked(s, nil)
			return out
		}
	}
	return r.String
}

// Redact redacts s with r once, as it was read, and makes the result valid UTF-8 (each invalid run becomes one
// "?", so it never grows): no character is replaced before redaction has seen the text (#840), and the repaired
// text is not redacted again, since redaction decides on the text as it was read (#845).
func Redact(r RedactFunc, s string) string {
	return strings.ToValidUTF8(r(s), "?")
}

// Value redacts one value from the cluster for a result line: a value longer than maxValueBytes once redacted
// is left out whole, never cut, and replaced by a note of its size; control characters and Unicode line and
// paragraph separators become spaces, so a value never adds lines of its own, nor anything a reader could
// take for a line break. Package internal/tools shows values the same way.
func Value(r RedactFunc, s string) string {
	return Shown(Redact(r, s))
}

// Shown is [Value] for a value already redacted as it was read, and made valid UTF-8 ([Redact]): a caller that
// redacts several values together (the name and value of a metric label, #845) shows each result with it.
func Shown(v string) string {
	if len(v) > maxValueBytes {
		return LeftOut(len(v))
	}
	return strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || c == '\u2028' || c == '\u2029' {
			return ' '
		}
		return c
	}, v)
}

// LeftOut is the note that replaces a value of n bytes left out whole ([Shown]; get_resource for an answer over
// its bound).
func LeftOut(n int) string { return "(" + strconv.Itoa(n) + " bytes left out)" }

// LogRead says how a read of the log source ended, as [RenderLog] shows it.
type LogRead int

// Outcomes of a log read.
const (
	// LogReadOK: the source answered within its bound.
	LogReadOK LogRead = iota
	// LogReadNotFound: the pod does not exist, or the node no longer holds the instance's log.
	LogReadNotFound
	// LogReadRejected: the source refused the request (no such container, not started, no previous instance).
	LogReadRejected
	// LogReadFailed: the read failed; the error text is not shown.
	LogReadFailed
	// LogReadOversized: the source answered more than twice what was asked for, and the text was not read.
	LogReadOversized
	// LogReadWithheld: redaction withheld the excerpt whole, in the source (providers.ErrWithheld) or here; nothing
	// of it is shown.
	LogReadWithheld
	// LogReadEmpty: the source answered with no log lines.
	LogReadEmpty
)

// Fixed header lines of a read that gave no excerpt. None quotes the cluster or the source's answer.
const (
	logGone      = "no logs: the node no longer holds the log of this instance"
	logForbidden = "could not fetch: the API server refused the identity"
	logEmpty     = "no log lines were returned"
)

// LogView is one log excerpt rendered for a reader: header lines naming the container and its instance, then
// the redacted excerpt cut to its newest lines, and the marks the caller turns into its own labels.
type LogView struct {
	// Text is the header followed by the excerpt, at most the budget [RenderLog] was given.
	Text string
	// HeadLen is the length of the header lines at the start of Text; a later cut keeps them and cuts the
	// excerpt from its oldest lines.
	HeadLen int
	// Read says how the read ended.
	Read LogRead
	// LeadingDropped: leading continuation lines were dropped before redaction.
	LeadingDropped bool
	// Truncated, EndMissing and InstanceUncertain repeat the source's marks.
	Truncated, EndMissing, InstanceUncertain bool
	// Cut: the excerpt was cut to the budget.
	Cut bool
}

// RenderLog turns one answer of the log source into a [LogView]. title is the first header line, made of
// values already passed through [Value]. asked is the byte bound asked of the source, which also bounds the
// excerpt; budget bounds the whole text, header included. The header is redacted and budgeted first, then
// the excerpt keeps its newest lines in what is left. Leading continuation lines are dropped before
// redaction, so a secret value whose key line was cut never reaches the redactor alone.
//
// Each text is redacted with [Twice] of red, as the release before #845 redacted it twice. refit is set by a
// caller that bounds the text again after this (get_pod_logs, FitLog), where that release redacted the header
// and the excerpt it kept a second time: with a redactor that has StringTwice, what that release hid when it cut
// the excerpt to its newest lines and redacted them again is hidden too (#845, thirteenth decision of
// 2026-11-18).
//
// An excerpt that redaction withholds (package redact, budget.go: the body, the header, or a value of the instance
// in it) is shown as a read withheld at the source: LogReadWithheld, a header that says so, and nothing of the
// answer (#873).
func RenderLog(red Redactor, refit bool, title string, ex providers.LogExcerpt, err error, asked, budget int) LogView {
	var v LogView
	var h lines
	h.add("%s", title)
	var body string
	switch {
	case errors.Is(err, providers.ErrLogGone):
		v.Read = LogReadNotFound
		h.add(logGone)
	case errors.Is(err, providers.ErrNotFound):
		v.Read = LogReadNotFound
		h.add("no logs: the pod does not exist")
	case errors.Is(err, providers.ErrRejected):
		v.Read = LogReadRejected
		h.add("no logs: the source refused the request (no such container, it has not started, or it has no previous instance)")
	case errors.Is(err, providers.ErrWithheld):
		v.Read = LogReadWithheld
		h.add("could not fetch: withheld by redaction")
	case errors.Is(err, providers.ErrForbidden):
		v.Read = LogReadFailed
		h.add(logForbidden)
	case err != nil:
		v.Read = LogReadFailed
		h.add("could not fetch")
	case len(ex.Text) > 2*asked:
		// More than was asked for, beyond what redaction markers could add: the source broke its bound, and
		// the text is not read further.
		v.Read = LogReadOversized
		h.add("could not fetch: the answer was larger than asked for")
	default:
		in := ex.Instance
		podUID, okUID := checkedValue(red, in.PodUID)
		containerID, okID := checkedValue(red, in.ContainerID)
		if !okUID || !okID {
			// A value of the header withheld whole withholds the excerpt, as a withheld body does (#873).
			return RenderLog(red, refit, title, providers.LogExcerpt{}, providers.ErrWithheld, asked, budget)
		}
		h.add("instance: pod uid %s, container id %s, restarts %d, read at %s", podUID, containerID,
			in.RestartCount, ex.CollectedAt.UTC().Format(time.RFC3339))
		if ex.Text == "" {
			v.Read = LogReadEmpty
			h.add(logEmpty)
			break
		}
		body, v.LeadingDropped = dropLeadingContinuation(ex.Text)
		v.Truncated, v.EndMissing, v.InstanceUncertain = ex.Truncated, ex.EndMissing, ex.InstanceUncertain
	}
	_, ok := red.(twicer)
	if !refit || !ok {
		header, okHeader := checkedTwice(red, h.String(), nil)
		redacted, okBody := checkedTwice(red, body, nil)
		if v.Read != LogReadWithheld && (!okHeader || !okBody) {
			return RenderLog(red, refit, title, providers.LogExcerpt{}, providers.ErrWithheld, asked, budget)
		}
		header, redacted = strings.ToValidUTF8(header, "?"), strings.ToValidUTF8(redacted, "?")
		head, _ := keepHead(header, budget)
		tail, cut := keepTail(redacted, min(asked, budget-len(head)))
		v.Text, v.HeadLen, v.Cut = head+tail, len(head), cut
		return v
	}
	header, okHeader := checkedTwice(red, h.String(), func(string) int { return 0 })
	if v.Read != LogReadWithheld && !okHeader {
		return RenderLog(red, refit, title, providers.LogExcerpt{}, providers.ErrWithheld, asked, budget)
	}
	head, _ := keepHead(strings.ToValidUTF8(header, "?"), budget)
	n := min(asked, budget-len(head))
	// The release before cut its excerpt to its newest lines in n bytes, then redacted what it kept again.
	newest := func(t string) int {
		kept, _ := keepTail(t, n)
		return len(t) - len(kept)
	}
	redacted, okBody := checkedTwice(red, body, newest)
	if v.Read != LogReadWithheld && !okBody {
		return RenderLog(red, refit, title, providers.LogExcerpt{}, providers.ErrWithheld, asked, budget)
	}
	tail, cut := keepTail(strings.ToValidUTF8(redacted, "?"), n)
	v.Text, v.HeadLen, v.Cut = head+tail, len(head), cut
	return v
}

// FitLog bounds a rendered log text again: the first headLen bytes are the header, kept from its start; the
// rest is the excerpt, cut to its newest lines in what the header leaves of n. The text was redacted when it
// was rendered and is not redacted again (#845). It returns the text, the new header length and whether
// anything was cut.
func FitLog(text string, headLen, n int) (string, int, bool) {
	head, headCut := keepHead(text[:headLen], n)
	tail, tailCut := keepTail(text[headLen:], n-len(head))
	return head + tail, len(head), headCut || tailCut
}
