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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// The expected reads of a rubric (#505): a line such as "get_resource Deployment/web" says the model should have
// read that object with that tool. The grammar, one form per example, is in docs/evaluation.md:
//
//	<tool>                                 any accepted call of the tool
//	<tool> <Kind>/<name>                   a call whose kind and name arguments are these
//	<tool> [<Kind>/<name>] <arg>=<value>...  a call whose named arguments have these values
//
// Parts are separated by white space; a value holds no white space and may hold '=' and '/'. The tool must be one
// the evaluation offers, every argument one of its schema, a value of an enum one of its values, a boolean true
// or false and an integer a whole number.

// rubricRead is one entry of a rubric's reads.
type rubricRead struct {
	// Read is the expected read, in the grammar above.
	Read string `json:"read"`
}

// The JSON schema types and the argument names the grammar knows.
const (
	typeBoolean  = "boolean"
	typeInteger  = "integer"
	argKind      = "kind"
	argName      = "name"
	argNamespace = "namespace"
)

// paramSpec is what the grammar checks of one argument of a tool: its JSON type and its enum.
type paramSpec struct {
	typ  string
	enum []string
}

// toolParams are a tool's arguments by name.
type toolParams map[string]paramSpec

// readRule is a parsed expected read.
type readRule struct {
	// text is the line as written, trimmed: what the report names.
	text string
	tool string
	// args are the constrained arguments and their values; an integer in its canonical form.
	args   map[string]string
	params toolParams
}

// parseRead parses one expected read against the tools offered, by name.
func parseRead(line string, offered map[string]toolParams) (readRule, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return readRule{}, errors.New("the read is empty")
	}
	r := readRule{text: strings.TrimSpace(line), tool: fields[0], args: map[string]string{}}
	params, ok := offered[r.tool]
	if !ok {
		return readRule{}, fmt.Errorf("read %q: no tool named %q is offered to the model (offered: %s)", r.text, r.tool,
			strings.Join(slices.Sorted(maps.Keys(offered)), ", "))
	}
	r.params = params
	set := func(name, value string) error {
		if name == "" {
			return fmt.Errorf("read %q: %q has an empty argument name", r.text, name+"="+value)
		}
		p, ok := params[name]
		if !ok {
			return fmt.Errorf("read %q: tool %s has no argument %q (it has: %s)", r.text, r.tool, name,
				strings.Join(slices.Sorted(maps.Keys(params)), ", "))
		}
		if value == "" {
			return fmt.Errorf("read %q: argument %s has an empty value", r.text, name)
		}
		if _, dup := r.args[name]; dup {
			return fmt.Errorf("read %q: argument %s is given twice", r.text, name)
		}
		switch {
		case len(p.enum) > 0 && !slices.Contains(p.enum, value):
			return fmt.Errorf("read %q: argument %s must be one of %s", r.text, name, strings.Join(p.enum, ", "))
		case p.typ == typeBoolean && value != strconv.FormatBool(true) && value != strconv.FormatBool(false):
			return fmt.Errorf("read %q: argument %s must be true or false", r.text, name)
		case p.typ == typeInteger:
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("read %q: argument %s must be an integer", r.text, name)
			}
			value = strconv.FormatInt(n, 10)
		}
		r.args[name] = value
		return nil
	}
	for i, f := range fields[1:] {
		if name, value, ok := strings.Cut(f, "="); ok {
			if err := set(name, value); err != nil {
				return readRule{}, err
			}
			continue
		}
		if !strings.Contains(f, "/") {
			return readRule{}, fmt.Errorf("read %q: %q is neither argument=value nor Kind/name", r.text, f)
		}
		_, hasKind := params[argKind]
		_, hasName := params[argName]
		kind, name, _ := strings.Cut(f, "/")
		switch {
		case !hasKind || !hasName:
			return readRule{}, fmt.Errorf("read %q: tool %s has no kind and name, so Kind/name does not apply", r.text,
				r.tool)
		case i != 0:
			return readRule{}, fmt.Errorf("read %q: Kind/name %q must come right after the tool", r.text, f)
		case kind == "" || name == "" || strings.Contains(name, "/"):
			return readRule{}, fmt.Errorf("read %q: %q is not Kind/name", r.text, f)
		}
		if err := set(argKind, kind); err != nil {
			return readRule{}, err
		}
		if err := set(argName, name); err != nil {
			return readRule{}, err
		}
	}
	return r, nil
}

// readCall is one call of a read tool that the loop ran: the tool the model named, its arguments as the model
// wrote them, and whether the tool accepted them (any status but refused).
type readCall struct {
	tool     string
	args     json.RawMessage
	accepted bool
}

// matches says whether c is the read r expects: the same tool, accepted, and every constrained argument present
// with r's value, compared as the tool's schema types it.
func (r readRule) matches(c readCall) bool {
	if c.tool != r.tool || !c.accepted {
		return false
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(c.args, &args); err != nil || args == nil {
		return false
	}
	for name, want := range r.args {
		raw, ok := args[name]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
		var got string
		switch r.params[name].typ {
		case typeInteger:
			var n int64
			if err := json.Unmarshal(raw, &n); err != nil {
				return false
			}
			got = strconv.FormatInt(n, 10)
		case typeBoolean:
			var b bool
			if err := json.Unmarshal(raw, &b); err != nil {
				return false
			}
			got = strconv.FormatBool(b)
		default:
			if err := json.Unmarshal(raw, &got); err != nil {
				return false
			}
		}
		if got != want {
			return false
		}
	}
	return true
}

// readsScore is the reads part of a scenario's score.
type readsScore struct {
	// Pass is whether every expected read was made; true when the rubric expects none.
	Pass     bool        `json:"pass"`
	Expected []readCheck `json:"expected,omitempty"`
	// Extra counts the accepted calls that match no expected read: counted, never failed.
	Extra int `json:"extra"`
	// Refused counts the calls the loop ran that read nothing: refused by the loop (a tool not offered, arguments
	// that are not one JSON object, another namespace than the subject's) or by the tool (arguments outside its
	// schema), or stopped by the analysis deadline.
	Refused int `json:"refused"`
}

// readCheck is one expected read and whether a call made it.
type readCheck struct {
	Read string `json:"read"`
	Pass bool   `json:"pass"`
}

// scoreReads scores the calls that reached the read tools against the expected reads. ran is the number of tool
// calls the loop ran (Outcome.ToolCalls): the loop refuses some before they reach the tools, and those count as
// refused.
func scoreReads(rules []readRule, calls []readCall, ran int) readsScore {
	s := readsScore{Pass: true}
	for _, r := range rules {
		ok := slices.ContainsFunc(calls, r.matches)
		s.Expected = append(s.Expected, readCheck{Read: r.text, Pass: ok})
		s.Pass = s.Pass && ok
	}
	accepted := 0
	for _, c := range calls {
		if !c.accepted {
			continue
		}
		accepted++
		if !slices.ContainsFunc(rules, func(r readRule) bool { return r.matches(c) }) {
			s.Extra++
		}
	}
	s.Refused = max(ran-accepted, 0)
	return s
}

// offeredToolParams are the arguments of the tools the loop offers in an evaluation run (toolDefinitions), by
// tool, read from their JSON schemas.
var offeredToolParams = sync.OnceValues(func() (map[string]toolParams, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	defs, err := toolDefinitions(scheme)
	if err != nil {
		return nil, err
	}
	out := map[string]toolParams{}
	for _, d := range defs {
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
				Enum []any  `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(d.Parameters, &schema); err != nil {
			return nil, fmt.Errorf("the schema of %s: %w", d.Name, err)
		}
		params := toolParams{}
		for name, p := range schema.Properties {
			spec := paramSpec{typ: p.Type}
			for _, e := range p.Enum {
				spec.enum = append(spec.enum, fmt.Sprint(e))
			}
			params[name] = spec
		}
		out[d.Name] = params
	}
	return out, nil
})

// checkReads parses the rubric's expected reads against the tools the scenario is offered (query_metrics only
// with a metrics.yaml), and refuses a read no accepted call can make (readRule.callable). It runs once the
// scenario's Incident and metrics are loaded.
func (s *scenario) checkReads() error {
	offered, err := offeredToolParams()
	if err != nil {
		return err
	}
	if s.metrics == nil {
		offered = maps.Clone(offered)
		delete(offered, tools.NameQueryMetrics)
	}
	set, err := readCheckTools()
	if err != nil {
		return err
	}
	s.reads = nil
	for _, rr := range s.rubric.Reads {
		r, err := parseRead(rr.Read, offered)
		if err != nil {
			return fmt.Errorf("rubric.yaml: %w", err)
		}
		if err := r.callable(set, s.incident.Spec.Subject.Namespace); err != nil {
			return fmt.Errorf("rubric.yaml: %w", err)
		}
		if slices.ContainsFunc(s.reads, func(o readRule) bool { return o.tool == r.tool && maps.Equal(o.args, r.args) }) {
			return fmt.Errorf("rubric.yaml: read %q is listed twice", r.text)
		}
		s.reads = append(s.reads, r)
	}
	return nil
}

// readCheckTools are the read tools the expected reads are checked with (readRule.callable): the tool set of an
// evaluation run over an empty cluster, so every call the tools accept reads nothing and ends not found.
var readCheckTools = sync.OnceValues(func() (*tools.Set, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	r, err := redact.New()
	if err != nil {
		return nil, err
	}
	c := newClient(scheme, nil)
	return tools.New(tools.Config{Pods: c, Events: c, Objects: c, Redactor: r, Namespaces: watchedNamespaces,
		Metrics: newReplayMetrics(&metricsRecording{}), Now: func() time.Time { return fixtureNow }})
})

// callable returns why no call that r matches can be accepted in a scenario whose subject is in
// subjectNamespace, or nil. A read the loop or the tool refuses every time can never be made, so every run of
// the scenario would fail for the rubric's error, not the model's. The tool's own argument checks decide
// (tools.Set.Call, the code that checks the model's calls: types, ranges, lengths, names, patterns, the
// watched namespaces): r's arguments are completed in every way the grammar leaves open (each free argument
// absent, each value of its enum, or a valid name), and r is callable when the tool accepts one of them. The
// loop's own rule comes first: every tool with a namespace argument reads only the subject's namespace
// (internal/analysis, run.boundCall), so a free namespace is the subject's and a constrained one must be.
func (r readRule) callable(set *tools.Set, subjectNamespace string) error {
	fixed := map[string]any{}
	var free []string
	for _, name := range slices.Sorted(maps.Keys(r.params)) {
		value, ok := r.args[name]
		switch {
		case name == argNamespace && ok && value != subjectNamespace:
			return fmt.Errorf("read %q: the analysis reads only the namespace of the Incident's subject, %q", r.text,
				subjectNamespace)
		case ok:
			fixed[name] = r.params[name].typed(value)
		case name == argNamespace:
			fixed[name] = subjectNamespace
		default:
			free = append(free, name)
		}
	}
	refusal := ""
	var try func(i int, args map[string]any) (bool, error)
	try = func(i int, args map[string]any) (bool, error) {
		if i == len(free) {
			raw, err := json.Marshal(args)
			if err != nil {
				return false, err
			}
			res, err := set.Call(context.Background(), providers.ToolCall{ID: "check", Name: r.tool, Arguments: raw})
			if err != nil {
				return false, fmt.Errorf("read %q: checking it with the tool: %w", r.text, err)
			}
			if res.Status != tools.StatusRefused {
				return true, nil
			}
			if refusal == "" {
				refusal = strings.TrimSpace(res.Text)
			}
			return false, nil
		}
		for _, v := range r.params[free[i]].options() {
			next := maps.Clone(args)
			if v != nil {
				next[free[i]] = v
			}
			if ok, err := try(i+1, next); ok || err != nil {
				return ok, err
			}
		}
		return false, nil
	}
	ok, err := try(0, fixed)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("read %q: tool %s refuses every call this read matches (%s)", r.text, r.tool, refusal)
	}
	return nil
}

// typed is value, in the grammar's canonical form, as the JSON value of the argument's type.
func (p paramSpec) typed(value string) any {
	switch p.typ {
	case typeInteger:
		n, _ := strconv.ParseInt(value, 10, 64)
		return n
	case typeBoolean:
		return value == strconv.FormatBool(true)
	}
	return value
}

// options are the values callable tries for an argument the read leaves free: absent (nil), and each value of
// its enum, or for a string without one a valid object name. An integer or a boolean is only tried absent: no
// tool requires one.
func (p paramSpec) options() []any {
	out := []any{nil}
	switch {
	case len(p.enum) > 0:
		for _, e := range p.enum {
			out = append(out, e)
		}
	case p.typ == "string":
		out = append(out, "x")
	}
	return out
}

// recordedTools wraps the read tools of one scenario run and keeps every call the loop ran, in a tally. It only
// reads what the tools returned.
type recordedTools struct {
	inner analysis.ToolSet
	t     *tally
}

func (r recordedTools) Definitions() []providers.ToolDefinition { return r.inner.Definitions() }

func (r recordedTools) Call(ctx context.Context, call providers.ToolCall) (tools.Result, error) {
	res, err := r.inner.Call(ctx, call)
	if err != nil {
		// The context is done: nothing was read. The loop still counts the call, so it counts as refused.
		return res, err
	}
	r.t.mu.Lock()
	defer r.t.mu.Unlock()
	r.t.reads = append(r.t.reads, readCall{tool: call.Name, args: slices.Clone(call.Arguments),
		accepted: res.Status != tools.StatusRefused})
	r.t.toolText = append(r.t.toolText, res.Text)
	return res, nil
}
