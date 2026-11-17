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
	"encoding/json"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The generated corpus of the read rule (#505): lines whose parse, match and score are known by construction,
// valid and invalid, over a synthetic set of tools, so the tests do not depend on the real tools' schemas.

// The corpus's tool with kind and name, and the reason the parser gives for a misused Kind/name.
const (
	corpusThing  = "get_thing"
	corpusLogs   = "get_logs"
	corpusString = "string"
	whyShorthand = "Kind/name"
)

// corpusTools are the synthetic tools of the corpus: one with kind and name (the Kind/name form), one with
// typed arguments, one with a single string argument.
var corpusTools = map[string]toolParams{
	corpusThing: {argKind: {typ: corpusString, enum: []string{"Alpha", "Beta"}}, "namespace": {typ: corpusString},
		argName: {typ: corpusString}},
	corpusLogs:  {"pod": {typ: corpusString}, "previous": {typ: typeBoolean}, "since": {typ: typeInteger}},
	"read_file": {"path": {typ: corpusString}},
}

// corpusSeps are the white space runs that may separate the parts of a line.
var corpusSeps = []string{" ", "  ", "\t", " \t "}

const corpusRuns = 2000

// corpusValue is a string value: any non-space characters, '=' and '/' included, which the grammar allows in a
// value.
func corpusValue(rng *rand.Rand, slashFree bool) string {
	alphabet := "abcXYZ019-._=/:"
	if slashFree {
		alphabet = "abcXYZ019-._:"
	}
	n := 1 + rng.IntN(8)
	var b strings.Builder
	for range n {
		b.WriteByte(alphabet[rng.IntN(len(alphabet))])
	}
	return b.String()
}

// corpusLine is a valid line built from its parts, and the parse it must give.
type corpusLine struct {
	tool      string
	shorthand bool // kind and name written as Kind/name
	args      map[string]string
	order     []string // the arguments written as name=value, in order
}

func (c corpusLine) render(rng *rand.Rand) string {
	sep := func() string { return corpusSeps[rng.IntN(len(corpusSeps))] }
	parts := []string{c.tool}
	if c.shorthand {
		parts = append(parts, c.args[argKind]+"/"+c.args[argName])
	}
	for _, a := range c.order {
		parts = append(parts, a+"="+c.args[a])
	}
	var b strings.Builder
	if rng.IntN(3) == 0 {
		b.WriteString(sep())
	}
	for i, p := range parts {
		if i > 0 {
			b.WriteString(sep())
		}
		b.WriteString(p)
	}
	if rng.IntN(3) == 0 {
		b.WriteString(sep())
	}
	return b.String()
}

// genValid builds a valid line of a random tool with a random set of constrained arguments.
func genValid(rng *rand.Rand) corpusLine {
	names := slices.Sorted(maps.Keys(corpusTools))
	tool := names[rng.IntN(len(names))]
	params := corpusTools[tool]
	c := corpusLine{tool: tool, args: map[string]string{}}
	_, hasKind := params[argKind]
	_, hasName := params[argName]
	if hasKind && hasName && rng.IntN(2) == 0 {
		c.shorthand = true
		c.args[argKind] = corpusTyped(rng, params[argKind], true)
		c.args[argName] = corpusValue(rng, true)
	}
	for _, a := range slices.Sorted(maps.Keys(params)) {
		if _, done := c.args[a]; done || rng.IntN(2) == 0 {
			continue
		}
		c.args[a] = corpusTyped(rng, params[a], false)
		c.order = append(c.order, a)
	}
	rng.Shuffle(len(c.order), func(i, j int) { c.order[i], c.order[j] = c.order[j], c.order[i] })
	return c
}

// corpusTyped is a valid value of a parameter.
func corpusTyped(rng *rand.Rand, p paramSpec, slashFree bool) string {
	switch {
	case len(p.enum) > 0:
		return p.enum[rng.IntN(len(p.enum))]
	case p.typ == typeBoolean:
		return strconv.FormatBool(rng.IntN(2) == 0)
	case p.typ == typeInteger:
		return strconv.Itoa(rng.IntN(2000) - 1000)
	}
	return corpusValue(rng, slashFree)
}

// Every valid line parses to the tool and the constraints it was built from, whatever white space separates
// its parts.
func TestReadRuleCorpusValid(t *testing.T) {
	rng := rand.New(rand.NewPCG(505, 1)) // #nosec G404 -- a seeded generator, so the corpus is reproducible
	for range corpusRuns {
		c := genValid(rng)
		line := c.render(rng)
		got, err := parseRead(line, corpusTools)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if got.tool != c.tool || !maps.Equal(got.args, c.args) {
			t.Fatalf("%q: parsed %s %v, built %s %v", line, got.tool, got.args, c.tool, c.args)
		}
		if got.text != strings.TrimSpace(line) {
			t.Fatalf("%q: text %q", line, got.text)
		}
	}
}

// corpusMutation turns a valid line into an invalid one, or reports that it does not apply to it; why is a
// part of the error the parser must give.
type corpusMutation struct {
	name  string
	why   string
	apply func(rng *rand.Rand, c corpusLine) (string, bool)
}

var corpusMutations = []corpusMutation{
	{"empty", "empty", func(rng *rand.Rand, _ corpusLine) (string, bool) {
		return []string{"", " ", "\t", "  \t"}[rng.IntN(4)], true
	}},
	{"unknown tool", "no tool", func(rng *rand.Rand, c corpusLine) (string, bool) {
		c.tool = []string{"nope", "get_thing2", "Get_thing", "kind=Alpha", "get_thing/x"}[rng.IntN(5)]
		return c.render(rng), true
	}},
	{"unknown argument", "has no argument", func(rng *rand.Rand, c corpusLine) (string, bool) {
		c.args = maps.Clone(c.args)
		c.args["bogus"] = "1"
		c.order = append(slices.Clone(c.order), "bogus")
		return c.render(rng), true
	}},
	{"empty value", "empty value", func(rng *rand.Rand, c corpusLine) (string, bool) {
		if len(c.order) == 0 {
			return "", false
		}
		c.args = maps.Clone(c.args)
		c.args[c.order[rng.IntN(len(c.order))]] = ""
		return c.render(rng), true
	}},
	{"empty argument name", "empty argument name", func(rng *rand.Rand, c corpusLine) (string, bool) {
		c.args = maps.Clone(c.args)
		c.args[""] = "x"
		c.order = append(slices.Clone(c.order), "")
		return c.render(rng), true
	}},
	{"repeated argument", "twice", func(rng *rand.Rand, c corpusLine) (string, bool) {
		if len(c.order) == 0 {
			return "", false
		}
		return c.render(rng) + " " + c.order[0] + "=" + c.args[c.order[0]], true
	}},
	{"shorthand and argument", "twice", func(rng *rand.Rand, c corpusLine) (string, bool) {
		if !c.shorthand {
			return "", false
		}
		a := []string{argKind, argName}[rng.IntN(2)]
		return c.render(rng) + " " + a + "=" + c.args[a], true
	}},
	{"shorthand on a tool without kind", whyShorthand, func(rng *rand.Rand, c corpusLine) (string, bool) {
		if c.tool == corpusThing {
			return "", false
		}
		return c.tool + " Alpha/web", true
	}},
	{"shorthand after an argument", whyShorthand, func(rng *rand.Rand, c corpusLine) (string, bool) {
		if !c.shorthand || len(c.order) == 0 {
			return "", false
		}
		a := c.order[0]
		return c.tool + " " + a + "=" + c.args[a] + " " + c.args[argKind] + "/" + c.args[argName], true
	}},
	{"malformed shorthand", whyShorthand, func(rng *rand.Rand, c corpusLine) (string, bool) {
		if c.tool != corpusThing {
			return "", false
		}
		return c.tool + " " + []string{"Alpha/", "/web", "Alpha/a/b", "/", "Alpha//b"}[rng.IntN(5)], true
	}},
	{"value outside the enum", "one of", func(rng *rand.Rand, c corpusLine) (string, bool) {
		if c.tool != corpusThing {
			return "", false
		}
		return c.tool + " " + []string{"kind=Gamma", "kind=alpha", "Gamma/web"}[rng.IntN(3)], true
	}},
	{"not a boolean", "true or false", func(rng *rand.Rand, c corpusLine) (string, bool) {
		if c.tool != corpusLogs {
			return "", false
		}
		return c.tool + " previous=" + []string{"yes", "True", "1", "false/"}[rng.IntN(4)], true
	}},
	{"not an integer", typeInteger, func(rng *rand.Rand, c corpusLine) (string, bool) {
		if c.tool != corpusLogs {
			return "", false
		}
		return c.tool + " since=" + []string{"ten", "1.5", "0x10", "5m"}[rng.IntN(4)], true
	}},
	{"bare word", "neither", func(rng *rand.Rand, c corpusLine) (string, bool) {
		return c.render(rng) + " " + []string{"web", "kinds", "Alpha", "x:y"}[rng.IntN(4)], true
	}},
}

// Every mutation of a valid line is refused, with the reason that mutation breaks.
func TestReadRuleCorpusInvalid(t *testing.T) {
	rng := rand.New(rand.NewPCG(505, 2)) // #nosec G404 -- as above
	applied := map[string]int{}
	for range corpusRuns {
		c := genValid(rng)
		m := corpusMutations[rng.IntN(len(corpusMutations))]
		line, ok := m.apply(rng, c)
		if !ok {
			continue
		}
		applied[m.name]++
		r, err := parseRead(line, corpusTools)
		if err == nil {
			t.Fatalf("%s: %q was accepted as %s %v", m.name, line, r.tool, r.args)
		}
		if !strings.Contains(err.Error(), m.why) {
			t.Fatalf("%s: %q refused with %q, want a reason with %q", m.name, line, err, m.why)
		}
	}
	for _, m := range corpusMutations {
		if applied[m.name] == 0 {
			t.Errorf("mutation %q never applied", m.name)
		}
	}
}

// corpusArgs is the JSON arguments of a call that sets every constraint of c, typed as the tool's schema
// types them, plus extra arguments the rule does not constrain.
func corpusArgs(rng *rand.Rand, c corpusLine, extra bool) map[string]any {
	params := corpusTools[c.tool]
	out := map[string]any{}
	for a, v := range c.args {
		out[a] = corpusJSON(params[a], v)
	}
	if extra {
		for _, a := range slices.Sorted(maps.Keys(params)) {
			if _, set := out[a]; !set && rng.IntN(2) == 0 {
				out[a] = corpusJSON(params[a], corpusTyped(rng, params[a], false))
			}
		}
	}
	return out
}

func corpusJSON(p paramSpec, v string) any {
	switch p.typ {
	case typeBoolean:
		return v == strconv.FormatBool(true)
	case typeInteger:
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		return n
	}
	return v
}

func corpusCall(t *testing.T, tool string, args map[string]any, accepted bool) readCall {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return readCall{tool: tool, args: raw, accepted: accepted}
}

// differentValue is a value of p that differs from v.
func differentValue(rng *rand.Rand, p paramSpec, v string) string {
	for {
		if w := corpusTyped(rng, p, false); w != v {
			return w
		}
		if len(p.enum) == 1 {
			panic("an enum of one value has no other value")
		}
	}
}

// A call matches a rule when its tool is the rule's, the tool accepted it and every constrained argument has the
// rule's value; arguments the rule leaves out do not matter. Each way a call can differ is a miss.
func TestReadRuleCorpusMatch(t *testing.T) {
	rng := rand.New(rand.NewPCG(505, 3)) // #nosec G404 -- as above
	for range corpusRuns {
		c := genValid(rng)
		r, err := parseRead(c.render(rng), corpusTools)
		if err != nil {
			t.Fatal(err)
		}
		args := corpusArgs(rng, c, true)
		if call := corpusCall(t, c.tool, args, true); !r.matches(call) {
			t.Fatalf("%q does not match %s %s", r.text, call.tool, call.args)
		}
		if call := corpusCall(t, c.tool, args, false); r.matches(call) {
			t.Fatalf("%q matches a refused call %s", r.text, call.args)
		}
		other := slices.DeleteFunc(slices.Sorted(maps.Keys(corpusTools)), func(n string) bool { return n == c.tool })
		if call := corpusCall(t, other[rng.IntN(len(other))], args, true); r.matches(call) {
			t.Fatalf("%q matches a call of %s", r.text, call.tool)
		}
		if len(c.args) == 0 {
			continue
		}
		keys := slices.Sorted(maps.Keys(c.args))
		k := keys[rng.IntN(len(keys))]
		missing := maps.Clone(args)
		delete(missing, k)
		if call := corpusCall(t, c.tool, missing, true); r.matches(call) {
			t.Fatalf("%q matches %s without %s", r.text, call.args, k)
		}
		changed := maps.Clone(args)
		changed[k] = corpusJSON(corpusTools[c.tool][k], differentValue(rng, corpusTools[c.tool][k], c.args[k]))
		if call := corpusCall(t, c.tool, changed, true); r.matches(call) {
			t.Fatalf("%q matches %s with another %s", r.text, call.args, k)
		}
	}
}

// Arguments that are not a JSON object, or a constrained argument of another JSON type, never match.
func TestReadRuleMatchOddArguments(t *testing.T) {
	r, err := parseRead(corpusLogs+" pod=web since=5 previous=false", corpusTools)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{``, `null`, `[]`, `"pod=web"`, `{"pod":["web"],"since":5,"previous":false}`,
		`{"pod":"web","since":"5x","previous":false}`, `{"pod":"web","since":5,"previous":null}`,
		`{"pod":"web","since":5}`, `{`} {
		if r.matches(readCall{tool: corpusLogs, args: json.RawMessage(raw), accepted: true}) {
			t.Errorf("%q matches %s", r.text, raw)
		}
	}
	if !r.matches(readCall{tool: corpusLogs, args: json.RawMessage(`{"previous":false,"since":5,"pod":"web"}`),
		accepted: true}) {
		t.Error("the typed arguments do not match")
	}
}

// A set of rules over distinct tools and a set of calls built to satisfy a known subset: each rule passes exactly
// when its call was made and accepted, a call refused by the tool or by the loop satisfies nothing and counts as
// refused, and every accepted call no rule expects counts as extra.
func TestReadScoreCorpus(t *testing.T) {
	rng := rand.New(rand.NewPCG(505, 4)) // #nosec G404 -- as above
	for range corpusRuns {
		var rules []readRule
		var calls []readCall
		var wantPass []bool
		wantExtra, wantRefused := 0, 0
		for _, tool := range slices.Sorted(maps.Keys(corpusTools)) {
			if rng.IntN(3) == 0 {
				continue
			}
			c := genValid(rng)
			for c.tool != tool {
				c = genValid(rng)
			}
			r, err := parseRead(c.render(rng), corpusTools)
			if err != nil {
				t.Fatal(err)
			}
			rules = append(rules, r)
			present := rng.IntN(2) == 0
			wantPass = append(wantPass, present)
			if present {
				for range 1 + rng.IntN(2) { // a read made twice is still one expected read
					calls = append(calls, corpusCall(t, c.tool, corpusArgs(rng, c, true), true))
				}
			}
			if rng.IntN(2) == 0 {
				calls = append(calls, corpusCall(t, c.tool, corpusArgs(rng, c, true), false))
				wantRefused++
			}
		}
		for range rng.IntN(3) {
			calls = append(calls, readCall{tool: "list_other", args: json.RawMessage(`{}`), accepted: true})
			wantExtra++
		}
		// Calls the loop refused before they reached the tools: counted by the loop only.
		loopRefused := rng.IntN(3)
		wantRefused += loopRefused
		rng.Shuffle(len(calls), func(i, j int) { calls[i], calls[j] = calls[j], calls[i] })
		got := scoreReads(rules, calls, len(calls)+loopRefused)
		wantAll := !slices.Contains(wantPass, false)
		if got.Pass != wantAll || got.Extra != wantExtra || got.Refused != wantRefused || len(got.Expected) != len(rules) {
			t.Fatalf("score %+v, want pass %v extra %d refused %d of %d rules", got, wantAll, wantExtra, wantRefused,
				len(rules))
		}
		for i, e := range got.Expected {
			if e.Read != rules[i].text || e.Pass != wantPass[i] {
				t.Fatalf("expected read %d %+v, want %q %v", i, e, rules[i].text, wantPass[i])
			}
		}
	}
}

// The rules of every scenario parse against the tools the evaluation really offers, and a few hand-picked lines
// over the real tools, one per form of docs/evaluation.md.
func TestReadRuleRealTools(t *testing.T) {
	offered, err := offeredToolParams()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"get_resource",
		readGetWeb,
		readGetWebDemo,
		readEventsPods,
		"check_missing_reference ConfigMap/web-config pod=web-0",
		"query_metrics template=container_memory_working_set_bytes window_minutes=60",
		"get_pod_logs pod=web-0 container=web previous=true", // offered to a scenario with a logs.yaml
	} {
		if _, err := parseRead(line, offered); err != nil {
			t.Errorf("%q: %v", line, err)
		}
	}
	for _, line := range []string{
		"get_delivery_status Kustomization/apps", // not offered by the analysis loop
		"get_resource Service/web",               // not a kind get_resource reads
		"get_pod_logs Pod/web-0",                 // get_pod_logs has no kind
		"query_metrics Pod/web-0",                // query_metrics has no kind
		"query_metrics window_minutes=1h",        // an integer argument
		"exec command=ls",
	} {
		if _, err := parseRead(line, offered); err == nil {
			t.Errorf("%q was accepted", line)
		}
	}
}
