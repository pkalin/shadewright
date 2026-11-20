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
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// The grammar of the differential fuzz targets of #845 (fifteenth decision of 2026-11-18): each target of package
// redact, of the read tools and of the context builder decodes its fuzz bytes with [Decode] into the texts it
// puts in its places (the strings of a sequence, a label's name and value, a waiting reason and its message), and
// checks that every probe secret the redactor of the release before hid ([Recorder]) is hidden by the branch too.

// ProbePrefix starts every probe secret: a probe is ProbePrefix and four digits, unique in one input.
const ProbePrefix = "SECRETPROBE"

// Probe returns the probe secret with number n.
func Probe(n int) string { return fmt.Sprintf("%s%04d", ProbePrefix, n%10000) }

// FieldSep separates the fields of an input in raw mode, and ProbeByte stands for a fresh probe there.
const (
	FieldSep  = '\x1e'
	ProbeByte = '\x01'
)

// MaxFieldBytes bounds one field: above the largest limit a target reaches (16,384 bytes of get_resource and the
// log budgets), so that a field can reach each limit and pass it.
const MaxFieldBytes = 40 << 10

// Input is a decoded fuzz input.
type Input struct {
	// Raw reports that the bytes were taken as text (mode byte divisible by 4), not built from the grammar.
	Raw bool
	// Knob is the byte after the mode byte: the target's own choices (a place, a size, a variant).
	Knob byte
	// Fields are the texts, n of them as Decode was asked; a field the input did not reach is empty.
	Fields []string
	// Probes are the probe secrets in the fields.
	Probes []string
}

// Size returns the bytes of the fields.
func (in Input) Size() int {
	n := 0
	for _, f := range in.Fields {
		n += len(f)
	}
	return n
}

// Joined returns the fields joined by sep.
func (in Input) Joined(sep string) string { return strings.Join(in.Fields, sep) }

// Key words, signal words and the other pieces of the grammar, as the findings of #845 and the corpus use them.
var (
	keyWords = []string{
		"password", "token", "authorization", "Authorization", "api_key", "apikey", "secret", "client_secret",
		"TOKEN", "PASSWORD", "passwd", "pwd", "private_key", "access_token", "x-api-key", "DB_PASSWORD",
		"aws_secret_access_key", "credentials", "auth", "bearer",
	}
	// seps are separators after a key: plain ones, quoted ones, and the look-alike colons and equals signs the
	// fold of package redact names (fold.go).
	seps = []string{
		":", ": ", "=", "=\"", "\":\"", "\": \"", " ", "\uff1a", "\ufe55", "\ufe13", "\u2236", "\u02d0", "\ua789", "\u2237", "\uff1d", "\u2a75", "\u2192", "->",
		"=>", ":\t", " = ", "='",
	}
	// format characters, inside and after keys.
	formats = []string{"\u200b", "\u200d", "\u00ad", "\ufeff", "\u2060", "\u200c"}
	eols    = []string{"\n", "\r\n", "\r", "\x00", "\u2028", "\u0085"}
	blocks  = []string{"|", ">", "|-", ">-", "|+", ">+", "|2"}
	flows   = []string{"{", "}", "[", "]", ", ", ",", "? ", "- ", "\"", "'", "`", "(", ")", ";", "&"}
	flags   = []string{"--password", "--pass=", "--token=", "--token", "-p", "--api-key", "--client-secret",
		"--password=", "-password", "--auth"}
	pemHeaders = []string{
		"-----BEGIN PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----", "-----END PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----", "-----BEGIN ENCRYPTED PRIVATE KEY-----",
	}
	words = []string{
		"x", "a", "b", "web", "reason", "status", "message", "Error", "BackOff", "value", "name", "key", "data",
		"10.0.0.1", "192.168.1.1", "postgres://u:", "@db:5432/x", "Bearer ", "Basic ", "https://h/p?sig=",
		"Server=db;Password=", "  ", "    ", "\t", "\u00a0", "user", "env", "msg: ", "- name: ", "  value: ",
	}
	// sameMarkers are map keys that the redactor replaces by markers that read alike (IP addresses), so that the
	// read tools rename the second (get_resource's " (2)").
	sameMarkers = []string{"0.0.0.1", "0.0.0.2", "10.0.0.1", "10.0.0.2", "192.168.1.1", "192.168.1.2"}
	// repeats are the counts a repeat takes, up to the sizes at each limit (the 16,384 bytes of get_resource, the
	// excerpt and section budgets).
	repeats = []int{2, 3, 8, 64, 512, 1024, 2047, 4096, 8048}
)

// reader hands out the bytes of a fuzz input, and zeros when they run out.
type reader struct {
	b []byte
	i int
}

func (r *reader) more() bool { return r.i < len(r.b) }

func (r *reader) next() byte {
	if r.i >= len(r.b) {
		return 0
	}
	c := r.b[r.i]
	r.i++
	return c
}

func pick(r *reader, list []string) string { return list[int(r.next())%len(list)] }

// builder builds the fields of one input.
type builder struct {
	fields []strings.Builder
	at     int
	probes []string
	// last are the starts of the last pieces written in the current field, for a repeat.
	last []int
}

func (b *builder) probe() string {
	p := Probe(len(b.probes))
	b.probes = append(b.probes, p)
	return p
}

func (b *builder) cur() *strings.Builder { return &b.fields[b.at] }

func (b *builder) write(s string) {
	f := b.cur()
	if f.Len()+len(s) > MaxFieldBytes {
		return
	}
	b.last = append(b.last, f.Len())
	if len(b.last) > 8 {
		b.last = b.last[1:]
	}
	f.WriteString(s)
}

func (b *builder) nextField() {
	if b.at+1 < len(b.fields) {
		b.at++
	}
	b.last = b.last[:0]
}

// repeat writes the last k pieces of the current field n times more, within MaxFieldBytes.
func (b *builder) repeat(k, n int) {
	if len(b.last) == 0 {
		return
	}
	k = min(max(k, 1), len(b.last))
	f := b.cur()
	seg := f.String()[b.last[len(b.last)-k]:]
	if len(seg) == 0 {
		return
	}
	n = min(n, (MaxFieldBytes-f.Len())/len(seg))
	f.WriteString(strings.Repeat(seg, max(n, 0)))
}

// Decode decodes data into an input of n fields. The first byte selects the mode: divisible by 4, the rest after
// the knob byte is the text itself (raw mode: FieldSep separates fields, ProbeByte stands for a fresh probe);
// otherwise the rest is read as the grammar, one operation per byte with its operands after it (with a mode byte of
// extendedMode or more, a byte of extendedOps or more is one of the operations added later, extendedOp).
func Decode(data []byte, n int) Input {
	n = max(n, 1)
	r := &reader{b: data}
	mode := r.next()
	in := Input{Knob: r.next()}
	b := &builder{fields: make([]strings.Builder, n)}
	if mode%4 == 0 {
		in.Raw = true
		for r.more() {
			switch c := r.next(); c {
			case FieldSep:
				b.nextField()
			case ProbeByte:
				b.write(b.probe())
			default:
				b.write(string([]byte{c}))
			}
		}
	} else {
		for r.more() {
			b.op(r, mode >= extendedMode)
		}
	}
	in.Fields = make([]string, n)
	for i := range b.fields {
		in.Fields[i] = b.fields[i].String()
	}
	in.Probes = b.probes
	return in
}

// extendedOps is the first operation byte of the operations added after the first corpus was written, read as
// such only after a mode byte of extendedMode or more: with a smaller one every byte selects one of the 24 first
// operations, as before, so the inputs of testdata/fuzz decode as they did.
const (
	extendedOps  = 240
	extendedMode = 0xf0
)

// op reads and applies one operation of the grammar; extended says whether the operations added later are read.
func (b *builder) op(r *reader, extended bool) {
	c := r.next()
	if extended && c >= extendedOps {
		b.extendedOp(c-extendedOps, r)
		return
	}
	switch c % 24 {
	case 0:
		b.write(pick(r, keyWords))
	case 1:
		b.write(pick(r, seps))
	case 2:
		b.write(pick(r, formats))
	case 3:
		b.write(pick(r, eols))
	case 4:
		b.write(pick(r, blocks))
	case 5:
		b.write(pick(r, flows))
	case 6:
		b.write(pick(r, flags))
	case 7:
		b.write(pick(r, pemHeaders))
	case 8:
		b.write(pick(r, words))
	case 9, 10:
		b.write(b.probe())
	case 11:
		b.nextField()
	case 12:
		k, c := int(r.next()%8)+1, r.next()
		b.repeat(k, repeats[int(c)%len(repeats)])
	case 13:
		// A key with a format character inside it: "pass\u200bword".
		k, f := pick(r, keyWords), pick(r, formats)
		i := int(r.next()) % (len(k) + 1)
		b.write(k[:i] + f + k[i:])
	case 14:
		// A key, a separator and a probe: the plain secret pair.
		b.write(pick(r, keyWords) + pick(r, seps) + b.probe())
	case 15:
		// A block scalar under a key, its content on the next line.
		k, bl, e, ind := pick(r, keyWords), pick(r, blocks), pick(r, eols), strings.Repeat(" ", int(r.next()%5))
		b.write(k + ":" + " " + bl + e + ind + b.probe())
	case 16:
		// A flow map or sequence holding a key and its value.
		k, sep := pick(r, keyWords), pick(r, seps)
		if r.next()%2 == 0 {
			b.write("{" + k + sep + b.probe() + "}")
		} else {
			b.write("[" + k + sep + ", " + b.probe() + "]")
		}
	case 17:
		// An explicit key.
		b.write("? " + pick(r, keyWords) + pick(r, eols) + ": " + b.probe())
	case 18:
		// A flag in one field and its value in the next: ["--password", V], ["--pass=", V].
		b.write(pick(r, flags))
		b.nextField()
		b.write(b.probe())
	case 19:
		// A signal in one field and the secret in the next (a waiting reason and its message, a label's name and
		// its value, an event's reason and its note).
		b.write(pick(r, keyWords) + pick(r, seps) + pick(r, keyWords))
		b.nextField()
		b.write(b.probe())
	case 20:
		// A private key header, a line end and a secret line.
		b.write(pick(r, pemHeaders) + pick(r, eols) + b.probe())
	case 21:
		// TOKEN="a",b, as a repeated unit with a format character after it.
		b.write(`TOKEN="a",b,`)
	case 22:
		// One byte as it is, so that the grammar reaches any character.
		c := r.next()
		if c < utf8.RuneSelf {
			b.write(string(rune(c)))
		} else {
			b.write(string([]byte{c}))
		}
	default:
		// A key, a look-alike separator with a format character, and a probe.
		b.write(pick(r, keyWords) + pick(r, formats) + pick(r, seps) + pick(r, formats) + b.probe())
	}
}

// extendedOp applies the operation op of those added after the first corpus (#845, fifteenth decision, the classes
// the fuzzer did not reach on its own).
func (b *builder) extendedOp(op byte, r *reader) {
	switch op % 3 {
	case 0:
		// A key and a quoted value, repeated, then a quote, a line end, a line of blanks and a closing quote with a
		// look-alike colon after it: each key reads its value as one shell word up to that group, whose end depends
		// on the key's column (P3).
		k, n, m, sep := pick(r, keyWords), repeats[int(r.next())%len(repeats)], repeats[int(r.next())%len(repeats)], pick(r, seps)
		b.write(strings.Repeat(k+`="a"`, min(n, MaxFieldBytes/(len(k)+4)/2)) + "\"\n" + strings.Repeat(" ", min(m, MaxFieldBytes/2)) + "\"" + sep)
	case 1:
		// A map key that is replaced by a marker that reads like another's, in one field, and its value in the next
		// (labels, as get_resource renames them after the decision, near its bound with a long argument).
		b.write(pick(r, sameMarkers))
		b.nextField()
		b.write(pick(r, words))
		b.nextField()
	default:
		// Short runs of invalid bytes, each before a character, repeated up to the bound of an answer (each run
		// one "?" in the answer, as the release before wrote it).
		run, n := strings.Repeat("\xff", int(r.next()%3)+1), repeats[int(r.next())%len(repeats)]
		b.write(strings.Repeat(run+pick(r, words), min(n, MaxFieldBytes/(len(run)+12))))
	}
}

// RawSeed returns the fuzz bytes of a raw-mode input with the given knob: the fields separated by FieldSep, each
// occurrence of ProbeByte in them a fresh probe.
func RawSeed(knob byte, fields ...string) []byte {
	return append([]byte{0, knob}, strings.Join(fields, string(rune(FieldSep)))...)
}

// ProbesIn returns the probes found in s, each once.
func ProbesIn(s string) map[string]bool {
	out := map[string]bool{}
	for i := 0; ; {
		j := strings.Index(s[i:], ProbePrefix)
		if j < 0 {
			return out
		}
		k := i + j + len(ProbePrefix)
		if k+4 <= len(s) && digits(s[k:k+4]) {
			out[s[i+j:k+4]] = true
		}
		i = k
	}
}

func digits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Recorder records the probes a redactor of the release before hid: every probe in the input of a call that is
// not in its output. A probe only cut away or left out later is not recorded, so a difference in where a text is
// cut is never taken for a leak.
type Recorder struct {
	hidden map[string]bool
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{hidden: map[string]bool{}} }

// Wrap returns f, recording what each call of it hides.
func (rec *Recorder) Wrap(f func(string) string) func(string) string {
	return func(s string) string {
		out := f(s)
		rec.Saw(s, out)
		return out
	}
}

// Saw records the probes of in that out does not hold.
func (rec *Recorder) Saw(in, out string) {
	if !strings.Contains(in, ProbePrefix) {
		return
	}
	kept := ProbesIn(out)
	for p := range ProbesIn(in) {
		if !kept[p] {
			rec.hidden[p] = true
		}
	}
}

// Hidden reports whether a call recorded hid p.
func (rec *Recorder) Hidden(p string) bool { return rec.hidden[p] }

// Leaks returns the probes the recorder saw hidden that out shows, sorted.
func (rec *Recorder) Leaks(out string) []string {
	var leaks []string
	for p := range ProbesIn(out) {
		if rec.hidden[p] {
			leaks = append(leaks, p)
		}
	}
	sortStrings(leaks)
	return leaks
}

// Hiding returns the probes of in that out does not hold, as a recorder that saw that one call: what a text's
// redaction hid, for the comparison of two readings of package redact (String of the joined strings against
// Strings).
func Hiding(in, out string) *Recorder {
	rec := NewRecorder()
	rec.Saw(in, out)
	return rec
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// KnownFailure is an input of a fuzz target that fails one property on the current build: Property is its name
// ("P1" to "P6"), Reason says what the input is.
type KnownFailure struct {
	Property, Reason string
}

// Check ends a fuzz input: the failures (each starting with its property and a colon) fail it, unless known
// holds the input, which then goes to [CheckKnown]. counted reports whether the work property P3 was checked.
func Check(t testing.TB, known map[string]KnownFailure, data []byte, counted bool, fails []string) {
	t.Helper()
	k, ok := known[string(data)]
	if !ok {
		if len(fails) > 0 && collect(t, data, fails) {
			return
		}
		if len(fails) > 0 {
			t.Fatal(strings.Join(fails, "\n"))
		}
		return
	}
	CheckKnown(t, k, counted, fails)
}

// CheckKnown skips a known failing input while it fails its property, naming it, and fails it when it fails
// another property or none: the fix of the rule then removes the input from the known failures.
func CheckKnown(t testing.TB, k KnownFailure, counted bool, fails []string) {
	t.Helper()
	var own, other []string
	for _, f := range fails {
		if strings.HasPrefix(f, k.Property+":") {
			own = append(own, f)
		} else {
			other = append(other, f)
		}
	}
	switch {
	case len(other) > 0:
		t.Fatal(strings.Join(other, "\n"))
	case len(own) > 0:
		t.Skipf("known failure: %s (%s)", k.Reason, Clip(own[0]))
	case k.Property == "P3" && !counted:
		t.Skipf("known failure: %s (P3 is checked only with -tags redactwork)", k.Reason)
	default:
		t.Fatalf("known failing input passes %s now (%s): remove it from the known failures", k.Property, k.Reason)
	}
}

// LeakFails returns the P1 failure when out shows a probe rec saw hidden, or nothing.
func LeakFails(rec *Recorder, what, out string) []string {
	if l := rec.Leaks(out); len(l) > 0 {
		return []string{LeakLine("P1", what+" shows what the release before hid", l, out)}
	}
	return nil
}

// LeakLine is the failure of property prop: what, the probes it shows, the shape of the text before the first
// of them (the part of the line its signature is made of, [Signature]) and the text.
func LeakLine(prop, what string, probes []string, out string) string {
	return fmt.Sprintf("%s: %s %v (after %q): %q", prop, what, probes, shapeBefore(out, probes[0]), Clip(out))
}

// shapeBefore is the shape of the 24 bytes before probe in out: each run of letters one "a", each run of digits
// one "9", everything else as it is.
func shapeBefore(out, probe string) string {
	i := strings.Index(out, probe)
	if i < 0 {
		return ""
	}
	return shape(out[max(i-24, 0):i])
}

func shape(s string) string {
	var b strings.Builder
	last := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_':
			c = 'a'
		case c >= '0' && c <= '9':
			c = '9'
		default:
			b.WriteByte(c)
			last = 0
			continue
		}
		if c != last {
			b.WriteByte(c)
		}
		last = c
	}
	return b.String()
}

// Clip shortens a text for a failure message.
func Clip(s string) string {
	if len(s) > 600 {
		return s[:300] + " ... " + s[len(s)-300:]
	}
	return s
}
