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

package prometheus

import (
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file is a fake Prometheus that evaluates the instant queries of the templates against stored series,
// so that a test checks what a query computes, not only its text. It knows the subset of PromQL the templates
// use: selectors with =, !=, =~ and !~ matchers, rate over a fixed window, sum and max by labels, "/" between
// vectors with the same label sets, and "== <number>" as a filter. A stored series of a counter holds its
// rate over the window directly: the tests are about which series are combined, not about rate itself.

// storedSeries is one series of the fake: its labels, the metric name under __name__, and its value.
type storedSeries struct {
	labels map[string]string
	value  float64
}

// sample is an element of an instant vector.
type sample struct {
	labels map[string]string
	value  float64
}

// evalFake answers an instant query with the series it selects from stored, like Prometheus would, and fails
// the test on a query outside the subset.
func evalFake(t *testing.T, stored []storedSeries) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		p := &promqlParser{toks: tokenize(t, query), stored: stored}
		v := p.expr()
		if p.err == nil && p.pos != len(p.toks) {
			p.fail("trailing tokens")
		}
		if p.err != nil {
			t.Errorf("fake prometheus cannot evaluate %s: %v", query, p.err)
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		items := make([]string, 0, len(v))
		for _, s := range v {
			delete(s.labels, "__name__")
			metric := make([]string, 0, len(s.labels))
			for _, k := range slices.Sorted(maps.Keys(s.labels)) {
				metric = append(metric, fmt.Sprintf("%q:%q", k, s.labels[k]))
			}
			items = append(items, fmt.Sprintf(`{"metric":{%s},"value":[%s,%q]}`, strings.Join(metric, ","), nowSecs,
				strconv.FormatFloat(s.value, 'g', -1, 64)))
		}
		slices.Sort(items)
		writeJSON(w, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Join(items, ",")+`]}}`)
	}
}

var promqlToken = regexp.MustCompile(`^(?:\s+|"(?:[^"\\]|\\.)*"|[A-Za-z_:][A-Za-z0-9_:]*|[0-9][0-9A-Za-z.]*|` +
	`=~|!~|!=|==|[=(){}\[\],/])`)

func tokenize(t *testing.T, s string) []string {
	t.Helper()
	var toks []string
	for s != "" {
		m := promqlToken.FindString(s)
		if m == "" {
			t.Errorf("fake prometheus cannot tokenize %q", s)
			return toks
		}
		if strings.TrimSpace(m) != "" {
			toks = append(toks, m)
		}
		s = s[len(m):]
	}
	return toks
}

type promqlParser struct {
	toks   []string
	pos    int
	stored []storedSeries
	err    error
}

func (p *promqlParser) fail(msg string) {
	if p.err == nil {
		p.err = fmt.Errorf("%s at token %d of %v", msg, p.pos, p.toks)
	}
}

func (p *promqlParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *promqlParser) next() string {
	tok := p.peek()
	p.pos++
	return tok
}

func (p *promqlParser) expect(tok string) {
	if got := p.next(); got != tok {
		p.fail(fmt.Sprintf("want %q, got %q", tok, got))
	}
}

// expr := div ["==" number]
func (p *promqlParser) expr() []sample {
	v := p.div()
	if p.peek() != "==" {
		return v
	}
	p.next()
	n, err := strconv.ParseFloat(p.next(), 64)
	if err != nil {
		p.fail("comparison with a non-number")
	}
	var out []sample
	for _, s := range v {
		if s.value == n {
			out = append(out, s)
		}
	}
	return out
}

// div := unary {"/" unary}, matching elements with identical label sets.
func (p *promqlParser) div() []sample {
	v := p.unary()
	for p.peek() == "/" {
		p.next()
		rhs := p.unary()
		var out []sample
		for _, l := range v {
			for _, r := range rhs {
				if maps.Equal(withoutName(l.labels), withoutName(r.labels)) {
					out = append(out, sample{labels: withoutName(l.labels), value: l.value / r.value})
				}
			}
		}
		v = out
	}
	return v
}

// unary := ("sum"|"max") "by" "(" labels ")" "(" expr ")" | "rate" "(" selector "[5m]" ")" | selector
func (p *promqlParser) unary() []sample {
	switch tok := p.next(); tok {
	case "sum", "max":
		p.expect("by")
		p.expect("(")
		var by []string
		for p.peek() != ")" && p.err == nil {
			by = append(by, p.next())
			if p.peek() == "," {
				p.next()
			}
		}
		p.expect(")")
		p.expect("(")
		v := p.expr()
		p.expect(")")
		return aggregate(tok, by, v)
	case "rate":
		p.expect("(")
		v := p.selector(p.next())
		p.expect("[")
		p.expect("5m")
		p.expect("]")
		p.expect(")")
		for i := range v {
			v[i].labels = withoutName(v[i].labels)
		}
		return v
	default:
		return p.selector(tok)
	}
}

// selector := name "{" label op "string" {"," label op "string"} "}"
func (p *promqlParser) selector(name string) []sample {
	type matcher struct{ label, op, value string }
	var ms []matcher
	p.expect("{")
	for p.peek() != "}" && p.err == nil {
		label, op := p.next(), p.next()
		value, err := strconv.Unquote(p.next())
		if err != nil {
			p.fail("unquoted matcher value")
		}
		ms = append(ms, matcher{label, op, value})
		if p.peek() == "," {
			p.next()
		}
	}
	p.expect("}")
	var out []sample
	for _, s := range p.stored {
		ok := s.labels["__name__"] == name
		for _, m := range ms {
			got := s.labels[m.label] // a missing label is the empty string, as in Prometheus
			switch m.op {
			case "=":
				ok = ok && got == m.value
			case "!=":
				ok = ok && got != m.value
			case "=~", "!~":
				re, err := regexp.Compile("^(?:" + m.value + ")$")
				if err != nil {
					p.fail("bad regular expression")
					return nil
				}
				ok = ok && re.MatchString(got) == (m.op == "=~")
			default:
				p.fail("unknown matcher " + m.op)
			}
		}
		if ok {
			out = append(out, sample{labels: maps.Clone(s.labels), value: s.value})
		}
	}
	return out
}

func aggregate(op string, by []string, v []sample) []sample {
	groups := map[string]*sample{}
	var order []string
	for _, s := range v {
		labels := map[string]string{}
		for _, l := range by {
			if s.labels[l] != "" {
				labels[l] = s.labels[l]
			}
		}
		key := fmt.Sprint(labels)
		g, ok := groups[key]
		if !ok {
			groups[key] = &sample{labels: labels, value: s.value}
			order = append(order, key)
			continue
		}
		switch op {
		case "sum":
			g.value += s.value
		case "max":
			g.value = max(g.value, s.value)
		}
	}
	out := make([]sample, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out
}

func withoutName(labels map[string]string) map[string]string {
	out := maps.Clone(labels)
	delete(out, "__name__")
	return out
}
