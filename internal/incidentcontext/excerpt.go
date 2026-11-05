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

	"github.com/pkalin/shadewright/internal/providers"
)

// RedactFunc redacts text. Its output must never be longer than its input, as package redact guarantees.
type RedactFunc func(string) string

// Redact makes s valid UTF-8 (each invalid run becomes one "?", so it never grows) and redacts it with r.
func Redact(r RedactFunc, s string) string {
	return r(strings.ToValidUTF8(s, "?"))
}

// Value redacts one value from the cluster for a result line: a value longer than maxValueBytes once redacted
// is left out whole, never cut, and replaced by a note of its size; control characters and Unicode line and
// paragraph separators become spaces, so a value never adds lines of its own, nor anything a reader could
// take for a line break. Package internal/tools shows values the same way.
func Value(r RedactFunc, s string) string {
	v := Redact(r, s)
	if len(v) > maxValueBytes {
		return "(" + strconv.Itoa(len(v)) + " bytes left out)"
	}
	return strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || c == '\u2028' || c == '\u2029' {
			return ' '
		}
		return c
	}, v)
}

// LogRead says how a read of the log source ended, as [RenderLog] shows it.
type LogRead int

// Outcomes of a log read.
const (
	// LogReadOK: the source answered within its bound.
	LogReadOK LogRead = iota
	// LogReadNotFound: the pod does not exist.
	LogReadNotFound
	// LogReadRejected: the source refused the request (no such container, not started, no previous instance).
	LogReadRejected
	// LogReadFailed: the read failed; the error text is not shown.
	LogReadFailed
	// LogReadOversized: the source answered more than twice what was asked for, and the text was not read.
	LogReadOversized
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
func RenderLog(r RedactFunc, title string, ex providers.LogExcerpt, err error, asked, budget int) LogView {
	var v LogView
	var h lines
	h.add("%s", title)
	var body string
	switch {
	case errors.Is(err, providers.ErrNotFound):
		v.Read = LogReadNotFound
		h.add("no logs: the pod does not exist")
	case errors.Is(err, providers.ErrRejected):
		v.Read = LogReadRejected
		h.add("no logs: the source refused the request (no such container, it has not started, or it has no previous instance)")
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
		h.add("instance: pod uid %s, container id %s, restarts %d, read at %s", Value(r, in.PodUID),
			Value(r, in.ContainerID), in.RestartCount, ex.CollectedAt.UTC().Format(time.RFC3339))
		body, v.LeadingDropped = dropLeadingContinuation(ex.Text)
		v.Truncated, v.EndMissing, v.InstanceUncertain = ex.Truncated, ex.EndMissing, ex.InstanceUncertain
	}
	head, _ := keepHead(Redact(r, h.String()), budget)
	tail, cut := keepTail(Redact(r, body), min(asked, budget-len(head)))
	v.Text, v.HeadLen, v.Cut = head+tail, len(head), cut
	return v
}

// FitLog bounds a rendered log text again after a second redaction pass: the first headLen bytes are the
// header, redacted and kept from its start; the rest is the excerpt, redacted and cut to its newest lines in
// what the header leaves of n. It returns the text, the new header length and whether anything was cut.
func FitLog(r RedactFunc, text string, headLen, n int) (string, int, bool) {
	head, headCut := keepHead(Redact(r, text[:headLen]), n)
	tail, tailCut := keepTail(Redact(r, text[headLen:]), n-len(head))
	return head + tail, len(head), headCut || tailCut
}
