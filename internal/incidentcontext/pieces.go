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
	"fmt"
	"strings"

	"github.com/pkalin/shadewright/internal/redact"
)

// A section is decided once, on the values as they were read (#845, fifteenth decision of 2026-11-18). The
// builder writes each section as pieces: the text it writes (a label, a sentence, a number) and each value from a
// source, written on a line as [Shown] writes it. A redactor that decides on pieces (redact.Redactor's
// PiecesWithin) takes one decision on them, which also holds what the release before #845 hid where it redacted
// each value, wrote it on its line and redacted the assembled text again; no reading takes text another reading
// already replaced. Any other redactor redacts each value, writes it, and redacts the assembled text again, as
// that release did.

// pieceRedactor is a [Redactor] that takes one decision on a text written as pieces (*redact.Redactor's
// PiecesWithin).
type pieceRedactor interface {
	PiecesWithin(ps []redact.Piece, limit int) []string
}

// piece is one piece of a section: text the builder writes or a value from a source, and the quotable value
// ([Block.Values]) it is part of.
type piece struct {
	redact.Piece
	// quote is one more than the index of the quotable value the piece is part of, 0 for none.
	quote int
}

// frag is a part of a section's text, as pieces in order.
type frag []piece

// val returns s, a value from a source, as a fragment.
func val(s string) frag {
	return frag{{Piece: redact.Piece{Text: s, Data: true, Form: redact.Line}}}
}

// fragf formats like fmt.Sprintf, with each argument that is a frag (only with the verb %s) placed as its pieces
// and every other argument formatted into text the builder writes.
func fragf(format string, args ...any) frag {
	var out frag
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, piece{Piece: redact.Piece{Text: text.String()}})
			text.Reset()
		}
	}
	next := 0
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			text.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("+-# 0123456789.", format[j]) >= 0 {
			j++
		}
		if j >= len(format) {
			text.WriteString(format[i:])
			break
		}
		spec := format[i : j+1]
		i = j
		if format[j] == '%' {
			text.WriteByte('%')
			continue
		}
		if next >= len(args) {
			text.WriteString(spec)
			continue
		}
		arg := args[next]
		next++
		if f, ok := arg.(frag); ok && spec == "%s" {
			flush()
			out = append(out, f...)
			continue
		}
		fmt.Fprintf(&text, spec, arg)
	}
	flush()
	return out
}

// cat joins strings (text the builder writes) and fragments into one fragment.
func cat(parts ...any) frag {
	var out frag
	for _, p := range parts {
		switch v := p.(type) {
		case frag:
			out = append(out, v...)
		case string:
			if v != "" {
				out = append(out, piece{Piece: redact.Piece{Text: v}})
			}
		default:
			out = append(out, piece{Piece: redact.Piece{Text: fmt.Sprint(v)}})
		}
	}
	return out
}

// joinFrags joins fragments with sep, text the builder writes, between them.
func joinFrags(fs []frag, sep string) frag {
	var out frag
	for i, f := range fs {
		if i > 0 {
			out = append(out, piece{Piece: redact.Piece{Text: sep}})
		}
		out = append(out, f...)
	}
	return out
}

// text returns the fragment as written, with nothing redacted.
func (f frag) text() string {
	var b strings.Builder
	for _, p := range f {
		b.WriteString(p.Text)
	}
	return b.String()
}

// decide returns the text of the pieces ps, decided once with r, and the quotable values they hold (by quote
// index, each the decided text of its pieces joined): with a redactor that decides on pieces, each value comes
// back decided and shown on its line ([Shown]: a value over the bound once decided is left out whole) and the
// text is not redacted again; with any other redactor each value is redacted and shown ([Value] with [Twice] of
// r) and the assembled text is redacted again ([Redact]), as the release before #845 did. quotes is the number of
// quotable values.
func decide(r Redactor, ps frag, quotes int) (string, []string) {
	out := make([]string, len(ps))
	if pr, ok := r.(pieceRedactor); ok {
		for i, d := range decidedPieces(pr, ps) {
			out[i] = d.text
		}
	} else {
		red := Twice(r)
		for i, p := range ps {
			out[i] = p.Text
			if p.Data {
				out[i] = Value(red, p.Text)
			}
		}
	}
	vs := make([]string, quotes)
	for i, p := range ps {
		if p.quote > 0 {
			vs[p.quote-1] += out[i]
		}
	}
	text := strings.Join(out, "")
	if _, ok := r.(pieceRedactor); !ok {
		text = Redact(Twice(r), text)
	}
	return text, vs
}

// decidedPiece is a piece as decided: its text, and whether it is a value from a source.
type decidedPiece struct {
	text string
	data bool
}

// decidedPieces takes the one decision on ps with r and returns each piece as decided, a value shown on its line
// ([Shown] of the decided value made valid UTF-8).
func decidedPieces(r pieceRedactor, ps frag) []decidedPiece {
	in := make([]redact.Piece, len(ps))
	for i, p := range ps {
		in[i] = p.Piece
	}
	decided := r.PiecesWithin(in, -1)
	out := make([]decidedPiece, len(ps))
	for i, p := range ps {
		out[i] = decidedPiece{text: decided[i], data: p.Data}
		if p.Data {
			out[i].text = Shown(strings.ToValidUTF8(decided[i], "?"))
		}
	}
	return out
}

// releasedTexter is a [Redactor] that writes a text as the release before #845 wrote it (*redact.Redactor's
// ReleasedText).
type releasedTexter interface {
	ReleasedText(s string) string
}

// releasedKey returns f as the release before #845 wrote it: each value redacted by that release and shown
// ([Value]), the text the builder writes as it is. With a redactor that has no such reading, each value as that
// redactor writes it alone, as before. written holds the values already written, by their text, so that a value
// repeated in the lines of one answer is read once.
func (b *Builder) releasedKey(f frag, written map[string]string) string {
	red := Twice(b.redactor)
	if rt, ok := b.redactor.(releasedTexter); ok {
		red = rt.ReleasedText
	}
	var s strings.Builder
	for _, p := range f {
		if p.Data {
			v, ok := written[p.Text]
			if !ok {
				v = Value(red, p.Text)
				written[p.Text] = v
			}
			s.WriteString(v)
		} else {
			s.WriteString(p.Text)
		}
	}
	return s.String()
}
