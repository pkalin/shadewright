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

package vertexai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// ctxText is the text of the first user message of the conversations built in tests.
const ctxText = "ctx"

// replyMark is text of a reply that must never appear in an error.
const replyMark = "REPLY-MARK-91bc"

// offeredTools are the tools the generated replies may call.
var offeredTools = map[string]bool{listEvents: true, getLogs: true}

func toolDefs() []providers.ToolDefinition {
	return []providers.ToolDefinition{
		{Name: listEvents, Description: "events", Parameters: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string","maxLength":63}}}`)},
		{Name: getLogs, Description: "logs", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
}

func testProvider(t testing.TB) *Provider {
	t.Helper()
	p, err := newProvider(Config{Endpoint: testEndpoint, Model: testModel, Tokens: &fakeTokens{tok: tokenMark}}, testHooks{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// genCall is what a generated function call part should come back as.
type genCall struct {
	wireID, name, args string
}

// genPart is a generated reply part and what it means, known by construction: whether the parser must accept
// it, the answer text it adds, and the call it is.
type genPart struct {
	json  string
	valid bool
	text  string
	// object says the part's text is, on its own, one JSON object.
	object bool
	call   *genCall
}

// idPool are the wire IDs generated calls get: empty is none, and two of them are local IDs, so a reply may
// repeat an ID or give one equal to the local ID of another position.
var idPool = []string{"", "", "c1", "c2", localID(0), localID(1)}

func genReplyPart(r *rand.Rand) genPart {
	id := idPool[r.IntN(len(idPool))]
	switch r.IntN(19) {
	case 0:
		return genPart{json: textPart(`{"a":1}`), valid: true, text: `{"a":1}`, object: true}
	case 1:
		return genPart{json: textPart("hello " + replyMark), valid: true, text: "hello " + replyMark}
	case 2:
		return genPart{json: `{"text":"` + replyMark + `","thought":true,"thoughtSignature":"c2lnMQ=="}`, valid: true}
	case 3:
		return genPart{json: callPart(id, listEvents, shopArgs), valid: true,
			call: &genCall{id, listEvents, shopArgs}}
	case 4:
		return genPart{json: callPart(id, getLogs, `{ "pod" : "web-1" , "n" : [1, 2] }`), valid: true,
			call: &genCall{id, getLogs, `{"pod":"web-1","n":[1,2]}`}}
	case 5:
		return genPart{json: `{"functionCall":{"name":"` + listEvents + `"}}`, valid: true, call: &genCall{"", listEvents, `{}`}}
	case 6:
		return genPart{json: callPart(id, "delete_all", `{}`)}
	case 7:
		return genPart{json: callPart(id, listEvents, `[1]`)}
	case 8:
		return genPart{json: callPart(id, listEvents, `null`)}
	case 9:
		return genPart{json: `{"functionCall":{"id":"c\u0007","name":"` + listEvents + `","args":{}}}`}
	case 10:
		return genPart{json: `{"functionCall":{"id":"c3","name":"list\u001bevents","args":{}}}`}
	case 11:
		return genPart{json: `{"functionCall":{"id":"a b","name":"` + listEvents + `","args":{}}}`}
	case 12:
		return genPart{json: `{"inlineData":{"mimeType":"text/plain","data":"eA=="}}`}
	case 13:
		return genPart{json: `{"text":"x","functionCall":{"name":"` + listEvents + `","args":{}}}`}
	case 14:
		return genPart{json: `{"functionCall":{"name":"` + listEvents + `","args":{},"partialArgs":[]}}`}
	case 15:
		return genPart{json: `{"functionCall":{"name":"` + listEvents + `","args":{}},"thought":true}`}
	case 16:
		return genPart{json: `{"text":1}`}
	case 17:
		return genPart{json: `{"text":"x","thoughtSignature":"c2lnMg=="}`, valid: true, text: "x"}
	default:
		return genPart{json: "{\"functionCall\":{\"name\":\"list\x7fevents\",\"args\":{}}}"}
	}
}

// genUsage is generated usage metadata and its meaning by construction.
type genUsage struct {
	json  string
	valid bool
	want  providers.Usage
}

func genUsageOf(r *rand.Rand) genUsage {
	p, tu, c, th := r.Int64N(1000), r.Int64N(50), r.Int64N(500), r.Int64N(500)
	cached := r.Int64N(p + 1)
	total := p + tu + c + th
	fields := map[string]int64{"promptTokenCount": p, "toolUsePromptTokenCount": tu, "candidatesTokenCount": c,
		"thoughtsTokenCount": th, "cachedContentTokenCount": cached, "totalTokenCount": total}
	valid := true
	switch r.IntN(8) {
	case 0:
		return genUsage{json: ""}
	case 1:
		fields["candidatesTokenCount"], valid = -1, false
	case 2:
		fields["totalTokenCount"], valid = total+1, false
	case 3:
		fields["cachedContentTokenCount"], valid = p+1, false
	case 4:
		fields["thoughtsTokenCount"], valid = maxUsageTokens+1, false
	}
	var parts []string
	for _, k := range []string{"promptTokenCount", "toolUsePromptTokenCount", "candidatesTokenCount",
		"thoughtsTokenCount", "cachedContentTokenCount", "totalTokenCount"} {
		// The API omits zero counts; half of the generated replies do too.
		if fields[k] == 0 && r.IntN(2) == 0 {
			continue
		}
		parts = append(parts, `"`+k+`":`+strconv.FormatInt(fields[k], 10))
	}
	g := genUsage{json: "{" + strings.Join(parts, ",") + "}", valid: valid}
	if valid {
		g.want = providers.Usage{InputTokens: p + tu, CachedInputTokens: cached, OutputTokens: c + th}
	}
	return g
}

// genReply is a generated reply and every choice it was made of.
type genReply struct {
	finish     string
	candidates int // -1: no candidates member
	role       string
	model      string
	parts      []genPart
	usage      genUsage
	structured bool
}

var (
	finishes   = []string{finishStop, finishStop, finishStop, "MAX_TOKENS", "SAFETY", "RECITATION", "BLOCKLIST", "MALFORMED_FUNCTION_CALL", "OTHER", ""}
	roles      = []string{roleModel, roleModel, roleModel, roleUser, ""}
	models     = []string{testModel, testModel, testModel, "gemini-2.5-flash-001", "", "bad model", strings.Repeat("m", maxModelName+1)}
	candCounts = []int{1, 1, 1, 0, 2, -1}
)

func (g genReply) body() string {
	parts := make([]string, len(g.parts))
	for i, p := range g.parts {
		parts[i] = p.json
	}
	cand := `{"content":{"role":"` + g.role + `","parts":[` + strings.Join(parts, ",") + `]},"finishReason":"` + g.finish + `"}`
	var members []string
	switch g.candidates {
	case -1:
	case 0:
		members = append(members, `"candidates":[]`)
	default:
		members = append(members, `"candidates":[`+strings.Repeat(cand+",", g.candidates-1)+cand+`]`)
	}
	if g.usage.json != "" {
		members = append(members, `"usageMetadata":`+g.usage.json)
	}
	members = append(members, `"modelVersion":`+jsonString(g.model))
	return "{" + strings.Join(members, ",") + "}"
}

// want is the outcome the choices give: the completion and whether the parser must accept the reply.
func (g genReply) want() (providers.Completion, bool) {
	if !g.usage.valid {
		return providers.Completion{UsageUnknown: true}, false
	}
	if !validModel(g.model) {
		return providers.Completion{Usage: g.usage.want}, false
	}
	billed := providers.Completion{Usage: g.usage.want, Model: g.model}
	ok := g.finish == finishStop && g.candidates == 1 && g.role == roleModel && len(g.parts) > 0
	var text strings.Builder
	var calls []providers.ToolCall
	objects, texts := 0, 0
	for _, p := range g.parts {
		ok = ok && p.valid
		text.WriteString(p.text)
		if p.text != "" {
			texts++
		}
		if p.object {
			objects++
		}
		if p.call != nil {
			id := p.call.wireID
			if id == "" {
				id = "vertex_call_" + strconv.Itoa(len(calls))
			}
			for _, c := range calls {
				ok = ok && c.ID != id
			}
			calls = append(calls, providers.ToolCall{ID: id, Name: p.call.name, Arguments: json.RawMessage(p.call.args)})
		}
	}
	ok = ok && len(calls) <= maxToolCalls
	if len(calls) == 0 {
		ok = ok && texts > 0 && (!g.structured || objects == 1 && texts == 1)
	}
	if !ok {
		return billed, false
	}
	return providers.Completion{Content: text.String(), ToolCalls: calls, Usage: g.usage.want, Model: g.model}, true
}

func pick[T any](r *rand.Rand, s []T) T { return s[r.IntN(len(s))] }

// mostly returns the valid choice three times in four, else a random one, so that every reply-level choice
// is valid in most generated replies and a good share of the corpus is accepted.
func mostly[T any](r *rand.Rand, valid T, all []T) T {
	if r.IntN(4) != 0 {
		return valid
	}
	return pick(r, all)
}

func genRandomReply(r *rand.Rand) genReply {
	g := genReply{finish: mostly(r, finishStop, finishes), candidates: mostly(r, 1, candCounts),
		role: mostly(r, roleModel, roles), model: mostly(r, testModel, models), usage: genUsageOf(r),
		structured: r.IntN(2) == 0}
	for !g.usage.valid && r.IntN(4) != 0 {
		g.usage = genUsageOf(r)
	}
	for range r.IntN(5) {
		p := genReplyPart(r)
		for !p.valid && r.IntN(4) != 0 {
			p = genReplyPart(r)
		}
		g.parts = append(g.parts, p)
	}
	return g
}

// replyCorpus is a seeded corpus of generated replies: random ones, and each reply-level choice varied alone
// from a valid reply with two calls.
func replyCorpus() []genReply {
	r := seeded(1167, 1)
	var out []genReply
	for range 6000 {
		out = append(out, genRandomReply(r))
	}
	base := genReply{finish: finishStop, candidates: 1, role: roleModel, model: testModel, structured: true,
		usage: genUsage{json: usageJSON, valid: true, want: wantUsage},
		parts: []genPart{
			{json: callPart("", listEvents, shopArgs), valid: true, call: &genCall{"", listEvents, shopArgs}},
			{json: callPart("", listEvents, `{"namespace":"web"}`), valid: true, call: &genCall{"", listEvents, `{"namespace":"web"}`}},
		}}
	out = append(out, base)
	for _, f := range finishes {
		g := base
		g.finish = f
		out = append(out, g)
	}
	for _, c := range candCounts {
		g := base
		g.candidates = c
		out = append(out, g)
	}
	for _, ro := range roles {
		g := base
		g.role = ro
		out = append(out, g)
	}
	for _, m := range models {
		g := base
		g.model = m
		out = append(out, g)
	}
	many := base
	many.parts = nil
	for i := range maxToolCalls + 1 {
		many.parts = append(many.parts, genPart{json: callPart("", getLogs, `{}`), valid: true, call: &genCall{"", getLogs, `{}`}})
		if i+1 == maxToolCalls {
			out = append(out, many)
			many.parts = slices.Clone(many.parts)
		}
	}
	out = append(out, many)
	return out
}

// The generated replies are parsed as their choices say: accepted with exactly the content, calls, usage and
// model built in, or refused as malformed output with the usage and model the reply's state allows, nothing
// else, and never a word of the reply in the error.
func TestParseResponseCorpus(t *testing.T) {
	accepted, refused := 0, 0
	for _, g := range replyCorpus() {
		body := g.body()
		got, err := parseResponse([]byte(body), testModel, offeredTools, g.structured)
		want, ok := g.want()
		blocks := got.ProviderBlocks
		got.ProviderBlocks = providers.Opaque{}
		if !ok {
			refused++
			if !providers.IsMalformedOutput(err) || !reflect.DeepEqual(got, want) || !blocks.IsZero() {
				t.Fatalf("reply %s:\nerr = %v\ngot  %+v\nwant %+v", body, err, got, want)
			}
			if strings.Contains(err.Error(), replyMark) || strings.Contains(err.Error(), "shop") {
				t.Fatalf("the error quotes the reply: %v", err)
			}
			continue
		}
		accepted++
		if err != nil || !reflect.DeepEqual(got, want) || blocks.IsZero() != (len(want.ToolCalls) == 0) {
			t.Fatalf("reply %s:\nerr = %v\ngot  %+v\nwant %+v", body, err, got, want)
		}
	}
	// The corpus exercises both outcomes in number.
	if accepted < 300 || refused < 3000 {
		t.Errorf("%d accepted and %d refused replies", accepted, refused)
	}
}

// The usage metadata maps to the three counts as ADR-0050 decision 6 says, and a reply whose usage cannot be
// used is malformed with UsageUnknown.
func TestUsageMapping(t *testing.T) {
	got, ok := usageOf(&usageMetadata{PromptTokenCount: 1000, ToolUsePromptTokenCount: 70, CachedContentTokenCount: 600,
		CandidatesTokenCount: 200, ThoughtsTokenCount: 300, TotalTokenCount: 1570})
	if want := (providers.Usage{InputTokens: 1070, CachedInputTokens: 600, OutputTokens: 500}); !ok || got != want {
		t.Errorf("usage = %+v, %v; want %+v", got, ok, want)
	}
	if _, ok := usageOf(&usageMetadata{}); !ok {
		t.Error("a zero report is refused")
	}
	for _, u := range []usageMetadata{
		{PromptTokenCount: 10, TotalTokenCount: 11},
		{PromptTokenCount: 10, CachedContentTokenCount: 11, TotalTokenCount: 10},
		{PromptTokenCount: -1, TotalTokenCount: -1},
		{PromptTokenCount: maxUsageTokens + 1, TotalTokenCount: maxUsageTokens + 1},
	} {
		if _, ok := usageOf(&u); ok {
			t.Errorf("usage %+v accepted", u)
		}
	}
	c, err := parseResponse([]byte(`{"candidates":[],"usageMetadata":{"promptTokenCount":"12"},"modelVersion":"m"}`),
		testModel, nil, false)
	if !providers.IsMalformedOutput(err) || !c.UsageUnknown {
		t.Errorf("a count of the wrong type: err = %v, completion %+v", err, c)
	}
}

// genJSON writes a random JSON value with objects whose member names are distinct, at depth at most depth,
// and records the objects it wrote so a test can add a repeated name to one.
type jsonGen struct {
	r       *rand.Rand
	objects int
	dupAt   int // the object that gets a repeated name, or -1
	dupFold bool
}

func (g *jsonGen) value(depth int) string {
	switch k := g.r.IntN(5); {
	case depth == 0 || k == 0:
		return pick(g.r, []string{`1`, `"s"`, `true`, `null`, `-2.5e3`, `"{\"a\":1,\"a\":2}"`})
	case k <= 2:
		return g.object(depth)
	default:
		var els []string
		for range g.r.IntN(3) {
			els = append(els, g.value(depth-1))
		}
		return "[" + strings.Join(els, ",") + "]"
	}
}

func (g *jsonGen) object(depth int) string {
	me := g.objects
	g.objects++
	keys := slices.Clone([]string{"a", "b", "s", "k", "e"})
	g.r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	keys = keys[:1+g.r.IntN(len(keys))]
	var members []string
	for _, k := range keys {
		members = append(members, jsonString(k)+":"+g.value(depth-1))
	}
	if me == g.dupAt {
		k := keys[g.r.IntN(len(keys))]
		if g.dupFold {
			// A name encoding/json reads as the same field: another case, or a letter that folds to it.
			k = map[string]string{"a": "A", "b": "B", "s": "ſ", "k": "K", "e": "E"}[k]
		}
		members = append(members, jsonString(k)+":"+g.value(0))
	}
	return "{" + strings.Join(members, ",") + "}"
}

// Generated JSON documents, half of them with a repeated member name (exact, by case or by Unicode folding)
// in one object at a random depth: uniqueKeys refuses exactly those.
func TestUniqueKeysCorpus(t *testing.T) {
	r := seeded(1167, 2)
	for i := range 4000 {
		// A first pass counts the objects of the document the seed gives; a second writes it with the repeat.
		seed := r.Uint64()
		probe := &jsonGen{r: seeded(seed, 0), dupAt: -1}
		probe.object(4)
		g := &jsonGen{r: seeded(seed, 0), dupAt: -1, dupFold: i%4 == 1}
		if i%2 == 1 {
			g.dupAt = r.IntN(probe.objects)
		}
		doc := g.object(4)
		if !json.Valid([]byte(doc)) {
			t.Fatalf("generated invalid JSON %s", doc)
		}
		if got, want := uniqueKeys([]byte(doc)), g.dupAt < 0; got != want {
			t.Fatalf("uniqueKeys(%s) = %v, want %v", doc, got, want)
		}
	}
	if uniqueKeys([]byte(`{"a":1`)) || uniqueKeys([]byte(`{"a" 1}`)) {
		t.Error("invalid JSON accepted")
	}
}

// The response schema loses its string and array bounds to the description, at every schema position and
// only there, in a deterministic form; other keywords stay.
func TestOutputSchema(t *testing.T) {
	in := `{"type":"object","properties":{` +
		`"a":{"type":"string","maxLength":10,"minLength":1,"pattern":"^x","description":"A."},` +
		`"list":{"type":"array","maxItems":5,"minItems":2,"uniqueItems":true,"items":{"type":"string","maxLength":3}},` +
		`"n":{"anyOf":[{"type":"string","maxLength":4},{"type":"null"}]},` +
		`"pattern":{"type":"integer","minimum":1,"maximum":9}},` +
		`"required":["a"],"additionalProperties":false}`
	want := `{"additionalProperties":false,"properties":{` +
		`"a":{"description":"A. Bounds: maxLength 10, minLength 1, pattern \"^x\".","type":"string"},` +
		`"list":{"description":"Bounds: maxItems 5, minItems 2, uniqueItems true.","items":{"description":"Bounds: maxLength 3.","type":"string"},"type":"array"},` +
		`"n":{"anyOf":[{"description":"Bounds: maxLength 4.","type":"string"},{"type":"null"}]},` +
		`"pattern":{"maximum":9,"minimum":1,"type":"integer"}},` +
		`"required":["a"],"type":"object"}`
	for range 20 {
		got, err := outputSchema(json.RawMessage(in))
		if err != nil || string(got) != want {
			t.Fatalf("outputSchema =\n%s, %v\nwant\n%s", got, err, want)
		}
	}
	for _, bad := range []string{`[]`, `"x"`, `{`, ``} {
		if _, err := outputSchema(json.RawMessage(bad)); err == nil {
			t.Errorf("outputSchema(%q) accepted", bad)
		}
	}
}

// requestBody decodes an encoded request generically.
func requestBody(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The conversation maps to turns: leading system messages to systemInstruction, adjacent user messages to
// one user turn in order, tools to functionDeclarations, and JSON mode only without tools.
func TestEncodeTurns(t *testing.T) {
	p := testProvider(t)
	req := providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "s1"}, {Role: providers.RoleSystem, Content: "s2"},
			{Role: providers.RoleUser, Content: "u1"}, {Role: providers.RoleUser, Content: "u2 <&>"},
		},
		ResponseSchema: structuredRequest().ResponseSchema,
	}
	data, offered, err := p.encode(req)
	if err != nil {
		t.Fatal(err)
	}
	m := requestBody(t, data)
	if fmt.Sprint(m["systemInstruction"]) != "map[parts:[map[text:s1] map[text:s2]]]" ||
		fmt.Sprint(m["contents"]) != "[map[parts:[map[text:u1] map[text:u2 <&>]] role:user]]" || len(offered) != 0 {
		t.Errorf("system %v, contents %v, offered %v", m["systemInstruction"], m["contents"], offered)
	}
	gc := m["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != mimeJSON || gc["responseJsonSchema"] == nil {
		t.Errorf("a request without tools does not ask for JSON: %v", gc)
	}
	req.Tools = toolDefs()
	data, offered, err = p.encode(req)
	if err != nil {
		t.Fatal(err)
	}
	m = requestBody(t, data)
	gc = m["generationConfig"].(map[string]any)
	if _, ok := gc["responseMimeType"]; ok {
		t.Errorf("a request with tools asks for JSON: %v", gc)
	}
	if _, ok := gc["responseJsonSchema"]; ok {
		t.Errorf("a request with tools sends a response schema: %v", gc)
	}
	if fmt.Sprint(m["tools"]) != "[map[functionDeclarations:[map[description:events name:list_events "+
		"parametersJsonSchema:map[properties:map[namespace:map[maxLength:63 type:string]] type:object]] "+
		"map[description:logs name:get_pod_logs parametersJsonSchema:map[type:object]]]]]" ||
		!offered[listEvents] || !offered[getLogs] {
		t.Errorf("tools %v, offered %v", m["tools"], offered)
	}
}

// Requests that break the turn rules or the tool rules are refused before anything is sent.
func TestEncodeRefusals(t *testing.T) {
	p := testProvider(t)
	sys := providers.Message{Role: providers.RoleSystem, Content: "s"}
	user := providers.Message{Role: providers.RoleUser, Content: "u"}
	for name, req := range map[string]providers.CompletionRequest{
		"no messages":            {},
		"only system":            {Messages: []providers.Message{sys}},
		"a late system message":  {Messages: []providers.Message{user, sys}},
		"an empty user message":  {Messages: []providers.Message{{Role: providers.RoleUser}}},
		"an empty system":        {Messages: []providers.Message{{Role: providers.RoleSystem}, user}},
		"starts with the model":  {Messages: []providers.Message{{Role: providers.RoleAssistant, Content: "a"}, user}},
		"ends with the model":    {Messages: []providers.Message{user, {Role: providers.RoleAssistant, Content: "a"}}},
		"an unknown role":        {Messages: []providers.Message{{Role: "developer", Content: "d"}}},
		"a user with a call ID":  {Messages: []providers.Message{{Role: providers.RoleUser, Content: "u", ToolCallID: "c"}}},
		"a stray tool result":    {Messages: []providers.Message{user, {Role: providers.RoleTool, ToolCallID: "c1", Content: "r"}}},
		"calls without blocks":   {Messages: []providers.Message{user, {Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{ID: localID(0), Name: listEvents, Arguments: json.RawMessage(`{}`)}}}, {Role: providers.RoleTool, ToolCallID: localID(0), Content: "r"}, user}},
		"blocks without calls":   {Messages: []providers.Message{user, {Role: providers.RoleAssistant, Content: "a", ProviderBlocks: providers.NewOpaque([]byte(`{}`))}, user}},
		"an invalid tool name":   {Messages: []providers.Message{user}, Tools: []providers.ToolDefinition{{Name: "a b", Parameters: json.RawMessage(`{}`)}}},
		"a repeated tool":        {Messages: []providers.Message{user}, Tools: []providers.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`{}`)}, {Name: "t", Parameters: json.RawMessage(`{}`)}}},
		"parameters not object":  {Messages: []providers.Message{user}, Tools: []providers.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`[]`)}}},
		"a schema not an object": {Messages: []providers.Message{user}, ResponseSchema: json.RawMessage(`[]`)},
	} {
		if _, _, err := p.encode(req); !providers.IsRejected(err) {
			t.Errorf("%s: err = %v, want rejected", name, err)
		}
	}
}

// toolTurn is a parsed reply with n function calls, two of them naming the same tool; wire says which calls
// the reply gives an ID.
func toolTurn(t *testing.T, wire []bool) (providers.Completion, string) {
	t.Helper()
	names := []string{listEvents, listEvents, getLogs}
	var parts []string
	for k, w := range wire {
		id := ""
		if w {
			id = "w" + strconv.Itoa(k)
		}
		parts = append(parts, callPart(id, names[k], `{"n":`+strconv.Itoa(k)+`}`))
	}
	if len(parts) > 0 {
		parts = append([]string{`{"text":"why","thought":true,"thoughtSignature":"dGhvdWdodA=="}`}, parts...)
	}
	body := replyJSON("[" + strings.Join(parts, ",") + "]")
	c, err := parseResponse([]byte(body), testModel, offeredTools, true)
	if err != nil {
		t.Fatal(err)
	}
	return c, body
}

// permutations returns every order of 0..n-1.
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := slices.Insert(slices.Clone(p), i, n-1)
			out = append(out, q)
		}
	}
	return out
}

// sequences are generated orders of tool results for n calls: every permutation, each call dropped, each
// repeated, a user message at each place, and each result answering a call that was not made. An entry is
// the call it answers, -1 for a user message and -2 for an unknown call.
func sequences(n int) [][]int {
	out := permutations(n)
	ident := out[0]
	for i := range ident {
		ident[i] = i
	}
	for i := range n {
		out = append(out, slices.Delete(slices.Clone(ident), i, i+1))
		out = append(out, slices.Insert(slices.Clone(ident), i+1, i))
		bad := slices.Clone(ident)
		bad[i] = -2
		out = append(out, bad)
		out = append(out, slices.Insert(slices.Clone(ident), i, -1))
	}
	return out
}

// Tool results generated in every order and with every fault, for one to three calls (two naming the same
// tool) with and without wire IDs: the request is encoded exactly when each call gets its result, in its
// order, before any other message. Then the user turn holds the function responses in that order, by name,
// with the reply's ID and never a local one, followed by the next user message.
func TestToolCorrelationCorpus(t *testing.T) {
	p := testProvider(t)
	for _, wire := range [][]bool{{false}, {true}, {false, false}, {true, true}, {true, false},
		{false, false, false}, {true, true, true}, {false, true, false}} {
		c, _ := toolTurn(t, wire)
		for _, seq := range sequences(len(wire)) {
			msgs := []providers.Message{
				{Role: providers.RoleSystem, Content: "s"}, {Role: providers.RoleUser, Content: ctxText},
				{Role: providers.RoleAssistant, ToolCalls: c.ToolCalls, ProviderBlocks: c.ProviderBlocks},
			}
			for _, k := range seq {
				switch k {
				case -1:
					msgs = append(msgs, providers.Message{Role: providers.RoleUser, Content: "interrupt"})
				case -2:
					msgs = append(msgs, providers.Message{Role: providers.RoleTool, ToolCallID: "zz", Content: "r"})
				default:
					msgs = append(msgs, providers.Message{Role: providers.RoleTool, ToolCallID: c.ToolCalls[k].ID,
						Content: "result " + strconv.Itoa(k)})
				}
			}
			msgs = append(msgs, providers.Message{Role: providers.RoleUser, Content: "last"})
			data, _, err := p.encode(providers.CompletionRequest{Messages: msgs, Tools: toolDefs(),
				ResponseSchema: structuredRequest().ResponseSchema})
			valid := len(seq) == len(wire)
			for i, k := range seq {
				valid = valid && k == i
			}
			if !valid {
				if !providers.IsRejected(err) {
					t.Errorf("wire %v, results %v: err = %v, want rejected", wire, seq, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("wire %v, results %v: %v", wire, seq, err)
			}
			var body struct {
				Contents []struct {
					Role  string           `json:"role"`
					Parts []map[string]any `json:"parts"`
				} `json:"contents"`
			}
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Contents) != 3 || body.Contents[1].Role != roleModel || body.Contents[2].Role != roleUser {
				t.Fatalf("wire %v: turns %+v", wire, body.Contents)
			}
			turn := body.Contents[2].Parts
			if len(turn) != len(wire)+1 || turn[len(wire)]["text"] != "last" {
				t.Fatalf("wire %v: user turn %v", wire, turn)
			}
			for k, w := range wire {
				fr, _ := turn[k]["functionResponse"].(map[string]any)
				id, hasID := fr["id"]
				if fr["name"] != c.ToolCalls[k].Name || fmt.Sprint(fr["response"]) != "map[result:result "+strconv.Itoa(k)+"]" ||
					hasID != w || w && id != c.ToolCalls[k].ID {
					t.Errorf("wire %v: response %d = %v", wire, k, fr)
				}
			}
		}
	}
}

// The parts of a reply go back exactly as they came, thought and signatures included, and only after their
// calls were checked against the message's tool calls.
func TestReplayKeepsParts(t *testing.T) {
	p := testProvider(t)
	c, body := toolTurn(t, []bool{true, false})
	var reply struct {
		Candidates []struct {
			Content struct {
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatal(err)
	}
	msgs := []providers.Message{{Role: providers.RoleUser, Content: ctxText},
		{Role: providers.RoleAssistant, ToolCalls: c.ToolCalls, ProviderBlocks: c.ProviderBlocks},
		{Role: providers.RoleTool, ToolCallID: c.ToolCalls[0].ID, Content: "r0"},
		{Role: providers.RoleTool, ToolCallID: c.ToolCalls[1].ID, Content: "r1"}}
	data, _, err := p.encode(providers.CompletionRequest{Messages: msgs, Tools: toolDefs()})
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Contents []struct {
			Parts []json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(data, &sent); err != nil {
		t.Fatal(err)
	}
	want := reply.Candidates[0].Content.Parts
	got := sent.Contents[1].Parts
	if len(got) != len(want) {
		t.Fatalf("%d parts sent back, want %d", len(got), len(want))
	}
	for i := range want {
		var w, g bytes.Buffer
		_ = json.Compact(&w, want[i])
		_ = json.Compact(&g, got[i])
		if w.String() != g.String() {
			t.Errorf("part %d sent back as %s, want %s", i, g.String(), w.String())
		}
	}
	if c.ToolCalls[0].ID != "w0" || c.ToolCalls[1].ID != localID(1) {
		t.Errorf("tool call IDs %q and %q", c.ToolCalls[0].ID, c.ToolCalls[1].ID)
	}
	if strings.Contains(string(data), "vertex_call_") {
		t.Error("a local ID was sent")
	}
}

// An assistant message whose provider blocks do not match its tool calls, or are not this adapter's for this
// model, is refused, and nothing is sent.
func TestReplayRefusesMismatch(t *testing.T) {
	c, _ := toolTurn(t, []bool{false, false})
	other, _ := parseResponse([]byte(replyJSON("["+callPart("", listEvents, `{"n":0}`)+","+
		callPart("", listEvents, `{"n":1}`)+"]")), "gemini-2.5-pro", offeredTools, true)
	blocks := string(c.ProviderBlocks.Bytes())
	opaque := func(s string) providers.Opaque { return providers.NewOpaque([]byte(s)) }
	cloneCalls := func() []providers.ToolCall { return slices.Clone(c.ToolCalls) }
	type msg = providers.Message
	cases := map[string]msg{
		"no blocks":         {ToolCalls: cloneCalls()},
		"another model":     {ToolCalls: cloneCalls(), ProviderBlocks: other.ProviderBlocks},
		"content":           {Content: "x", ToolCalls: cloneCalls(), ProviderBlocks: c.ProviderBlocks},
		"a renamed call":    {ToolCalls: func() []providers.ToolCall { s := cloneCalls(); s[0].Name = getLogs; return s }(), ProviderBlocks: c.ProviderBlocks},
		"other arguments":   {ToolCalls: func() []providers.ToolCall { s := cloneCalls(); s[1].Arguments = json.RawMessage(`{"n":0}`); return s }(), ProviderBlocks: c.ProviderBlocks},
		"swapped calls":     {ToolCalls: []providers.ToolCall{c.ToolCalls[1], c.ToolCalls[0]}, ProviderBlocks: c.ProviderBlocks},
		"swapped IDs":       {ToolCalls: func() []providers.ToolCall { s := cloneCalls(); s[0].ID, s[1].ID = s[1].ID, s[0].ID; return s }(), ProviderBlocks: c.ProviderBlocks},
		"a wire ID claimed": {ToolCalls: func() []providers.ToolCall { s := cloneCalls(); s[0].ID = "w0"; return s }(), ProviderBlocks: c.ProviderBlocks},
		"an extra call":     {ToolCalls: append(cloneCalls(), providers.ToolCall{ID: "vertex_call_2", Name: getLogs, Arguments: json.RawMessage(`{}`)}), ProviderBlocks: c.ProviderBlocks},
		"a missing call":    {ToolCalls: cloneCalls()[:1], ProviderBlocks: c.ProviderBlocks},
		"a tampered part":   {ToolCalls: cloneCalls(), ProviderBlocks: opaque(strings.Replace(blocks, `{"n":1}`, `{"n":7}`, 1))},
		"an unknown key":    {ToolCalls: cloneCalls(), ProviderBlocks: opaque(strings.Replace(blocks, `"thought":true`, `"thought":true,"executableCode":{}`, 1))},
		"a repeated key":    {ToolCalls: cloneCalls(), ProviderBlocks: opaque(strings.Replace(blocks, `"name":"list_events"`, `"name":"list_events","NAME":"get_pod_logs"`, 1))},
		"an unknown member": {ToolCalls: cloneCalls(), ProviderBlocks: opaque(strings.Replace(blocks, `{"model":`, `{"extra":1,"model":`, 1))},
		"trailing data":     {ToolCalls: cloneCalls(), ProviderBlocks: opaque(blocks + "{}")},
		"not JSON":          {ToolCalls: cloneCalls(), ProviderBlocks: opaque("[opaque]")},
		"no parts":          {ToolCalls: cloneCalls(), ProviderBlocks: opaque(`{"model":"` + testModel + `","parts":[]}`)},
	}
	for name, m := range cases {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			reply(w, replyJSON(`[{"text":"{}"}]`))
		}, nil)
		m.Role = providers.RoleAssistant
		msgs := []providers.Message{{Role: providers.RoleUser, Content: ctxText}, m}
		for _, tc := range m.ToolCalls {
			msgs = append(msgs, providers.Message{Role: providers.RoleTool, ToolCallID: tc.ID, Content: "r"})
		}
		_, err := h.p.Complete(t.Context(), providers.CompletionRequest{Messages: msgs, Tools: toolDefs()})
		if !providers.IsRejected(err) || !providers.IsNotSent(err) || h.requests.Load() != 0 {
			t.Errorf("%s: err = %v, %d requests", name, err, h.requests.Load())
		}
	}
	// The same blocks with the message's own calls, and with arguments the caller stored without control
	// characters, are sent.
	ctl, _ := parseResponse([]byte(replyJSON("["+callPart("", listEvents, `{"ns":"sh\u001bop"}`)+"]")),
		testModel, offeredTools, true)
	stored := providers.WithoutControls(ctl)
	for name, m := range map[string]msg{
		"as parsed":               {ToolCalls: c.ToolCalls, ProviderBlocks: c.ProviderBlocks},
		"stored without controls": {ToolCalls: stored.ToolCalls, ProviderBlocks: stored.ProviderBlocks},
	} {
		m.Role = providers.RoleAssistant
		msgs := []providers.Message{{Role: providers.RoleUser, Content: ctxText}, m}
		for _, tc := range m.ToolCalls {
			msgs = append(msgs, providers.Message{Role: providers.RoleTool, ToolCallID: tc.ID, Content: "r"})
		}
		if _, _, err := testProvider(t).encode(providers.CompletionRequest{Messages: msgs, Tools: toolDefs()}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if string(stored.ToolCalls[0].Arguments) != `{"ns":"shop"}` || !stored.ToolCalls[0].HeldControls {
		t.Fatalf("the stored call is %+v", stored.ToolCalls[0])
	}
}

func FuzzParseResponse(f *testing.F) {
	for i, g := range replyCorpus() {
		if i%60 == 0 {
			f.Add([]byte(g.body()), g.structured)
		}
	}
	f.Add([]byte(replyJSON("["+callPart("c1", listEvents, `{}`)+","+callPart("", getLogs, `{"a":[1]}`)+"]")), true)
	f.Add([]byte(`not json`), true)
	p := testProvider(f)
	f.Fuzz(func(t *testing.T, data []byte, structured bool) {
		c, err := parseResponse(data, testModel, offeredTools, structured)
		if err != nil {
			if !providers.IsMalformedOutput(err) {
				t.Fatalf("error of another class: %v", err)
			}
			if c.Content != "" || c.ToolCalls != nil || !c.ProviderBlocks.IsZero() {
				t.Fatal("content returned with an error")
			}
			if len(data) > 8 && strings.Contains(err.Error(), string(data)) {
				t.Fatal("the error quotes the body")
			}
			return
		}
		if !validModel(c.Model) || c.UsageUnknown {
			t.Fatalf("success with model %q, usage unknown %v", c.Model, c.UsageUnknown)
		}
		if c.Content == "" && len(c.ToolCalls) == 0 {
			t.Fatal("success without content or tool call")
		}
		if structured && len(c.ToolCalls) == 0 && !isJSONObject([]byte(c.Content)) {
			t.Fatal("structured success that is not a JSON object")
		}
		if c.Usage.InputTokens < 0 || c.Usage.OutputTokens < 0 || c.Usage.CachedInputTokens > c.Usage.InputTokens {
			t.Fatalf("usage out of range %+v", c.Usage)
		}
		if len(c.ToolCalls) == 0 {
			if !c.ProviderBlocks.IsZero() {
				t.Fatal("provider blocks without tool calls")
			}
			return
		}
		ids := map[string]bool{}
		for _, tc := range c.ToolCalls {
			if !offeredTools[tc.Name] || !isJSONObject(tc.Arguments) || !toolCallIDPattern.MatchString(tc.ID) || ids[tc.ID] {
				t.Fatalf("unchecked tool call %+v", tc)
			}
			ids[tc.ID] = true
		}
		// What the parser accepts, the replay check accepts back, as parsed and as the caller stores it.
		for _, comp := range []providers.Completion{c, providers.WithoutControls(c)} {
			msgs := []providers.Message{{Role: providers.RoleUser, Content: ctxText},
				{Role: providers.RoleAssistant, Content: comp.Content, ToolCalls: comp.ToolCalls,
					ProviderBlocks: comp.ProviderBlocks}}
			for _, tc := range comp.ToolCalls {
				msgs = append(msgs, providers.Message{Role: providers.RoleTool, ToolCallID: tc.ID, Content: "r"})
			}
			if _, _, err := p.encode(providers.CompletionRequest{Messages: msgs, Tools: toolDefs()}); err != nil {
				t.Fatalf("the replay of an accepted reply is refused: %v", err)
			}
		}
	})
}

// A document nested deeper than maxJSONDepth is refused, however it is shaped, without a frame for each byte
// of a body made of opening brackets.
func TestUniqueKeysBoundsDepth(t *testing.T) {
	nest := func(n int, open, close string) []byte {
		return []byte(strings.Repeat(open, n) + strings.Repeat(close, n))
	}
	if !uniqueKeys(nest(maxJSONDepth, "[", "]")) {
		t.Error("arrays at the depth bound are refused")
	}
	for name, doc := range map[string][]byte{
		"arrays":  nest(maxJSONDepth+1, "[", "]"),
		"objects": []byte(strings.Repeat(`{"a":`, maxJSONDepth+1) + "1" + strings.Repeat("}", maxJSONDepth+1)),
		"opening": []byte(strings.Repeat("[", 1<<20)),
	} {
		if uniqueKeys(doc) {
			t.Errorf("%s: a document deeper than %d is accepted", name, maxJSONDepth)
		}
	}
	obj := []byte(strings.Repeat(`{"a":`, maxJSONDepth) + "1" + strings.Repeat("}", maxJSONDepth))
	if !uniqueKeys(obj) {
		t.Error("objects at the depth bound are refused")
	}
}
