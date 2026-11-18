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

package tools

import (
	"cmp"
	"reflect"
	"slices"
	"strings"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/redact"
)

// A tool result is one text, decided once (#845, third decision of 2026-11-18). Each read tool lays out the
// values it shows as one text (layout): the fields in document order, each as a "name: value" line, the items
// of a list of objects as "- " items one level deeper, the strings of a list as lines of their own, the entries of a map as "key: value" lines, nothing escaped or
// encoded. The redactor takes one decision on that text (redact.Redactor.Pieces) and each value gets its
// result back through the setter recorded with it. No field, list or map is decided by itself, and the
// answer, once encoded, is not redacted again.

// pieceRedactor is a [Redactor] that takes one decision on a text laid out from pieces (*redact.Redactor's
// Pieces).
type pieceRedactor interface {
	Pieces(ps []redact.Piece) []string
}

// layout is a text being laid out from the values of a tool result, with a setter for each value.
type layout struct {
	pieces []redact.Piece
	set    []func(string) // by piece; nil for the layout code writes
	after  []func()       // run once every value is set, in order (a map rebuilt from its redacted entries)
}

// text appends layout code writes.
func (l *layout) text(t string) {
	l.pieces = append(l.pieces, redact.Piece{Text: t})
	l.set = append(l.set, nil)
}

// value appends a value from a source; set receives it once decided.
func (l *layout) value(v string, set func(string)) {
	l.pieces = append(l.pieces, redact.Piece{Text: v, Data: true})
	l.set = append(l.set, set)
}

// field appends a line "name: value" at indentation ind, for a value from a source.
func (l *layout) field(ind, name, v string, set func(string)) {
	l.text(ind + name + ": ")
	l.value(v, set)
	l.text("\n")
}

// decide takes the one decision on the laid out text with the redactor and gives each value its result, made
// valid UTF-8. A redactor without Pieces redacts each value on its own.
func (s *Set) decide(l *layout) {
	var out []string
	if pr, ok := s.redactor.(pieceRedactor); ok {
		out = pr.Pieces(l.pieces)
	} else {
		out = make([]string, len(l.pieces))
		for i, p := range l.pieces {
			out[i] = p.Text
			if p.Data {
				out[i] = s.redactor.String(p.Text)
			}
		}
	}
	for i, set := range l.set {
		if set != nil {
			set(strings.ToValidUTF8(out[i], "?"))
		}
	}
	for _, f := range l.after {
		f()
	}
}

// shown returns a setter that stores a decided value for a result line, as incidentcontext.Shown shows it.
func shown(dst *string) func(string) {
	return func(v string) { *dst = incidentcontext.Shown(v) }
}

// jsonName returns the name encoding/json gives a struct field, and whether its fields are inlined (an embedded
// struct without a name) or it is left out ("-").
func jsonName(f reflect.StructField) (name string, inline, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	name, _, _ = strings.Cut(tag, ",")
	if name == "" && f.Anonymous {
		return "", true, false
	}
	if name == "" {
		name = f.Name
	}
	return name, false, false
}

// lay appends the values reachable from v to l, at indentation ind, under the field name (empty for the root or
// an inlined struct), and records a setter that writes each decided value back into v. v must be settable and
// own everything it reaches (a deep copy): strings are written in place and maps are rebuilt. It reports whether
// it appended any value. Unexported fields (a time, a quantity) hold no free text and are left out; so are
// empty strings, numbers and booleans, which hold no text to decide. A map without string keys and an array
// are left out of the answer: none holds text in the objects the tools read.
func (l *layout) lay(v reflect.Value, ind, name string) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return false
		}
		return l.lay(v.Elem(), ind, name)
	case reflect.String:
		if v.Len() == 0 || !v.CanSet() {
			return false
		}
		l.field(ind, name, v.String(), v.SetString)
		return true
	case reflect.Struct:
		return l.under(ind, name, func(inner string) bool {
			added := false
			for sf, f := range v.Fields() {
				if !sf.IsExported() {
					continue
				}
				n, inline, skip := jsonName(sf)
				switch {
				case skip:
				case inline:
					added = l.lay(f, inner, "") || added
				default:
					added = l.lay(f, inner, n) || added
				}
			}
			return added
		})
	case reflect.Slice:
		if v.Len() == 0 {
			return false
		}
		if v.Type().Elem().Kind() == reflect.String {
			return l.under(ind, name, func(string) bool { return l.strings(v) })
		}
		return l.under(ind, name, func(inner string) bool {
			added := false
			for i := range v.Len() {
				e := v.Index(i)
				// An item's first line follows its dash, the rest one level deeper, as YAML writes a list of
				// objects.
				mark := len(l.pieces)
				if l.lay(e, inner+"  ", "") {
					l.pieces[mark].Text = inner + "- " + strings.TrimPrefix(l.pieces[mark].Text, inner+"  ")
					added = true
				}
			}
			return added
		})
	case reflect.Map:
		if v.Len() == 0 {
			return false
		}
		if v.Type().Key().Kind() != reflect.String {
			// No such map holds text in the objects the tools read; one that did would not be decided, so it
			// is left out (fail closed).
			if v.CanSet() {
				v.SetZero()
			}
			return false
		}
		return l.under(ind, name, func(inner string) bool { return l.mapEntries(v, inner) })
	case reflect.Array:
		// No array holds text in the objects the tools read either; one is left out like such a map.
		if v.CanSet() {
			v.SetZero()
		}
		return false
	}
	return false
}

// listEnd is the line that ends the strings of a list in the layout: text at column 0 that is no comment and
// no document marker, so a block a string opens ends before the fields after the list.
const listEnd = "(end of list)\n"

// strings appends the strings of the list v, each as a line of its own at column 0, then listEnd. The strings
// of a list are laid out as the strings of a sequence are joined (redact.Strings): a key in one string and its
// block in the next keep their columns, and a line at column 0 holds no block of a key before the list.
func (l *layout) strings(v reflect.Value) bool {
	for i := range v.Len() {
		e := v.Index(i)
		l.value(e.String(), e.SetString)
		l.text("\n")
	}
	l.text(listEnd)
	return true
}

// under appends the values that body lays out, under a line "name:" at indentation ind when name is set (body
// then lays out one level deeper); the line is left out when body appends no value.
func (l *layout) under(ind, name string, body func(inner string) bool) bool {
	if name == "" {
		return body(ind)
	}
	mark := len(l.pieces)
	l.text(ind + name + ":\n")
	if body(ind + "  ") {
		return true
	}
	l.pieces, l.set = l.pieces[:mark], l.set[:mark]
	return false
}

// mapEntries appends the entries of the map v, in the order encoding/json writes them (by key), as "key: value"
// lines at indentation ind, and rebuilds v from the decided keys and values once they are set. Two keys redacted
// alike are one entry: the value of the one written last is kept, never a secret.
func (l *layout) mapEntries(v reflect.Value, ind string) bool {
	type entry struct{ k, e reflect.Value }
	var es []entry
	for it := v.MapRange(); it.Next(); {
		k := reflect.New(v.Type().Key()).Elem()
		k.Set(it.Key())
		e := reflect.New(v.Type().Elem()).Elem()
		e.Set(it.Value())
		es = append(es, entry{k, e})
	}
	slices.SortFunc(es, func(a, b entry) int { return cmp.Compare(a.k.String(), b.k.String()) })
	for _, en := range es {
		l.text(ind)
		l.value(en.k.String(), en.k.SetString)
		if en.e.Kind() == reflect.String {
			l.text(": ")
			if en.e.Len() > 0 {
				l.value(en.e.String(), en.e.SetString)
			}
			l.text("\n")
			continue
		}
		l.text(":\n")
		l.lay(en.e, ind+"  ", "")
	}
	l.after = append(l.after, func() {
		c := reflect.MakeMapWithSize(v.Type(), len(es))
		for _, en := range es {
			c.SetMapIndex(en.k, en.e)
		}
		v.Set(c)
	})
	return true
}
