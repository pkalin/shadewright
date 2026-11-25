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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// Bounds of a reply. A reply outside them is malformed.
const (
	// maxToolCalls is the most function calls one reply may hold: maxToolSteps is at most 16.
	maxToolCalls = 16
	// maxModelName bounds the model version a reply reports.
	maxModelName = 128
	// maxUsageTokens bounds each usage count, so their sums cannot overflow.
	maxUsageTokens = 1 << 40
	// maxJSONDepth bounds the nesting of a reply, and of the blocks sent back: a reply of deeper nesting is
	// malformed, so a body made of opening brackets cannot make the checks allocate a frame for each byte.
	maxJSONDepth = 100
)

// Roles of the turns of a conversation.
const (
	roleUser  = "user"
	roleModel = "model"
)

// finishStop is the only finish reason of a reply the adapter accepts.
const finishStop = "STOP"

// localIDPrefix starts the IDs the adapter gives, by position, to function calls the reply gave none. They
// are never sent back.
const localIDPrefix = "vertex_call_"

var (
	// toolNamePattern bounds the tool names offered and called, within the API's rule for function names.
	toolNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)
	// toolCallIDPattern bounds the IDs a reply gives its function calls; they are echoed in functionResponse.
	toolCallIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	// modelVersionPattern is what a reported model version may look like.
	modelVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
)

// The request body of generateContent.
type (
	generateRequest struct {
		SystemInstruction *content         `json:"systemInstruction,omitempty"`
		Contents          []content        `json:"contents"`
		Tools             []toolSet        `json:"tools,omitempty"`
		GenerationConfig  generationConfig `json:"generationConfig"`
	}
	content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	// part is a text or functionResponse part. A part with raw set is a part of an earlier reply, sent back
	// exactly as it came.
	part struct {
		raw              json.RawMessage
		Text             string            `json:"text,omitempty"`
		FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	}
	functionResponse struct {
		// ID is the ID the reply gave the call; empty, and not sent, for a local ID.
		ID       string         `json:"id,omitempty"`
		Name     string         `json:"name"`
		Response responseResult `json:"response"`
	}
	responseResult struct {
		Result string `json:"result"`
	}
	toolSet struct {
		FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
	}
	functionDeclaration struct {
		Name                 string          `json:"name"`
		Description          string          `json:"description,omitempty"`
		ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
	}
	generationConfig struct {
		MaxOutputTokens    int             `json:"maxOutputTokens"`
		ThinkingConfig     thinkingConfig  `json:"thinkingConfig"`
		ResponseMIMEType   string          `json:"responseMimeType,omitempty"`
		ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
	}
	thinkingConfig struct {
		// ThinkingBudget is always sent, zero included.
		ThinkingBudget int `json:"thinkingBudget"`
	}
)

// MarshalJSON writes a part of an earlier reply exactly as it came, and any other part from its fields.
func (p part) MarshalJSON() ([]byte, error) {
	if p.raw != nil {
		return p.raw, nil
	}
	type plain part // without this method
	return json.Marshal(plain(p))
}

// The reply of generateContent, only the fields the adapter reads.
type (
	generateResponse struct {
		Candidates    json.RawMessage `json:"candidates"`
		UsageMetadata *usageMetadata  `json:"usageMetadata"`
		ModelVersion  json.RawMessage `json:"modelVersion"`
	}
	candidate struct {
		Content      *replyContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	}
	replyContent struct {
		Role  string            `json:"role"`
		Parts []json.RawMessage `json:"parts"`
	}
	usageMetadata struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		TotalTokenCount         int64 `json:"totalTokenCount"`
	}
	// replyPart is one part of a reply's content. Only the keys in partKeys may appear.
	replyPart struct {
		Text             *string       `json:"text"`
		Thought          *bool         `json:"thought"`
		ThoughtSignature *string       `json:"thoughtSignature"`
		FunctionCall     *functionCall `json:"functionCall"`
	}
	// functionCall is the call of a part. Only the keys in callKeys may appear.
	functionCall struct {
		ID   *string         `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	// blocks is what Completion.ProviderBlocks holds: the parts of a reply with function calls, as they came,
	// and the configured model they came from.
	blocks struct {
		Model string            `json:"model"`
		Parts []json.RawMessage `json:"parts"`
	}
)

// The keys a reply part and its function call may have.
var (
	partKeys = map[string]bool{"text": true, "thought": true, "thoughtSignature": true, "functionCall": true}
	callKeys = map[string]bool{"id": true, "name": true, "args": true}
)

// encode checks a request and builds its body. It returns the names of the tools offered, against which the
// reply's function calls are checked. Errors are fixed text matching providers.ErrRejected (or ErrTooLarge),
// and never quote the request.
func (p *Provider) encode(req providers.CompletionRequest) ([]byte, map[string]bool, error) {
	rejected := func(msg string) error { return fmt.Errorf("vertexai: %s: %w", msg, providers.ErrRejected) }
	if len(req.Messages) == 0 {
		return nil, nil, rejected("the request has no messages")
	}
	maxOut := req.MaxOutputTokens
	if maxOut == 0 {
		maxOut = p.maxOutput
	}
	if maxOut < 1 || maxOut > MaxOutputTokens {
		return nil, nil, rejected("the output token limit is out of range")
	}
	// Thinking tokens count against maxOutputTokens: the allowance on top of the request's limit, also sent as
	// the thinking budget, leaves the answer its limit while one cap bounds both.
	body := generateRequest{GenerationConfig: generationConfig{
		MaxOutputTokens: p.OutputSpend(maxOut),
		ThinkingConfig:  thinkingConfig{ThinkingBudget: p.thinkingAllowance},
	}}
	offered := map[string]bool{}
	var decls []functionDeclaration
	for _, t := range req.Tools {
		if !toolNamePattern.MatchString(t.Name) || offered[t.Name] {
			return nil, nil, rejected("a tool name is invalid or repeated")
		}
		if !isJSONObject(t.Parameters) {
			return nil, nil, rejected("the parameters of a tool are not a JSON schema object")
		}
		offered[t.Name] = true
		decls = append(decls, functionDeclaration{Name: t.Name, Description: t.Description,
			ParametersJSONSchema: json.RawMessage(bytes.TrimSpace(t.Parameters))})
	}
	if len(decls) > 0 {
		body.Tools = []toolSet{{FunctionDeclarations: decls}}
	}
	if len(req.ResponseSchema) > 0 {
		schema, err := outputSchema(req.ResponseSchema)
		if err != nil {
			return nil, nil, rejected("the response schema is not a JSON object")
		}
		// Gemini 2.5 refuses function declarations with a JSON response type: a request that offers tools
		// sends neither, and the caller's checks of the answer stay the only ones.
		if len(req.Tools) == 0 {
			body.GenerationConfig.ResponseMIMEType = mimeJSON
			body.GenerationConfig.ResponseJSONSchema = schema
		}
	}
	system, contents, err := encodeMessages(req.Messages, p.model)
	if err != nil {
		return nil, nil, rejected(err.Error())
	}
	body.SystemInstruction, body.Contents = system, contents
	// Without HTML escaping, so the parts sent back keep their strings as the reply wrote them.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, nil, rejected("the request could not be encoded")
	}
	data := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(data) > maxRequestBytes {
		return nil, nil, fmt.Errorf("vertexai: the request is larger than %d bytes: %w", maxRequestBytes,
			providers.ErrTooLarge)
	}
	return data, offered, nil
}

// encodeMessages maps the conversation. The leading system messages become the system instruction. User and
// tool messages become user turns and assistant messages model turns; adjacent messages of one role are
// merged in order, so the results of one assistant turn and the message after them go in one user turn.
// The tool messages after an assistant message with tool calls must answer those calls, one each, in their
// order, by ID, before any other message. Error text is fixed.
func encodeMessages(in []providers.Message, model string) (*content, []content, error) {
	var system *content
	i := 0
	for ; i < len(in) && in[i].Role == providers.RoleSystem; i++ {
		m := in[i]
		if m.Content == "" || len(m.ToolCalls) > 0 || m.ToolCallID != "" || !m.ProviderBlocks.IsZero() {
			return nil, nil, errors.New("a system message is empty or carries tool calls")
		}
		if system == nil {
			system = &content{}
		}
		system.Parts = append(system.Parts, part{Text: m.Content})
	}
	if i == len(in) {
		return nil, nil, errors.New("the request has only system messages")
	}
	b := turnBuilder{model: model}
	for _, m := range in[i:] {
		if err := b.message(m); err != nil {
			return nil, nil, err
		}
	}
	if b.next < len(b.pending) {
		return nil, nil, errUnanswered
	}
	if b.out[0].Role != roleUser || b.out[len(b.out)-1].Role != roleUser {
		return nil, nil, errors.New("the conversation does not start and end with a user turn")
	}
	return system, b.out, nil
}

// errUnanswered is the refusal of a conversation in which a tool call has no result where it must have one.
var errUnanswered = errors.New("a tool call has no result before the next message")

// turnBuilder maps the messages after the system messages, in order, to turns.
type turnBuilder struct {
	model string
	out   []content
	// pending are the tool calls of the last assistant message, wire whether the reply gave each its ID, and
	// next the position of the call the next tool message must answer.
	pending []providers.ToolCall
	wire    []bool
	next    int
}

// add appends parts to the last turn when it has the role, else starts a turn.
func (b *turnBuilder) add(role string, parts ...part) {
	if n := len(b.out); n > 0 && b.out[n-1].Role == role {
		b.out[n-1].Parts = append(b.out[n-1].Parts, parts...)
		return
	}
	b.out = append(b.out, content{Role: role, Parts: parts})
}

func (b *turnBuilder) message(m providers.Message) error {
	switch m.Role {
	case providers.RoleSystem:
		return errors.New("a system message follows the start of the conversation")
	case providers.RoleUser:
		return b.user(m)
	case providers.RoleTool:
		return b.toolResult(m)
	case providers.RoleAssistant:
		return b.assistant(m)
	}
	return errors.New("a message has an unknown role")
}

func (b *turnBuilder) user(m providers.Message) error {
	if b.next < len(b.pending) {
		return errUnanswered
	}
	if len(m.ToolCalls) > 0 || m.ToolCallID != "" || !m.ProviderBlocks.IsZero() {
		return errors.New("only assistant and tool messages carry tool calls")
	}
	if m.Content == "" {
		return errors.New("a user message is empty")
	}
	b.add(roleUser, part{Text: m.Content})
	return nil
}

// toolResult answers the call at the next position of the last assistant message, by ID. The ID is sent back
// only when the reply gave it.
func (b *turnBuilder) toolResult(m providers.Message) error {
	if len(m.ToolCalls) > 0 || !m.ProviderBlocks.IsZero() {
		return errors.New("a tool message carries tool calls")
	}
	if b.next >= len(b.pending) {
		return errors.New("a tool result answers no call, or one already answered")
	}
	call := b.pending[b.next]
	if m.ToolCallID != call.ID {
		return errors.New("a tool result is out of order or answers another call")
	}
	fr := &functionResponse{Name: call.Name, Response: responseResult{Result: m.Content}}
	if b.wire[b.next] {
		fr.ID = call.ID
	}
	b.next++
	b.add(roleUser, part{FunctionResponse: fr})
	return nil
}

func (b *turnBuilder) assistant(m providers.Message) error {
	if b.next < len(b.pending) {
		return errUnanswered
	}
	if m.ToolCallID != "" {
		return errors.New("an assistant message has a tool call ID")
	}
	if n := len(b.out); n > 0 && b.out[n-1].Role == roleModel {
		// Only user-side messages merge: text must never join a replayed turn, whose parts are signed.
		return errors.New("an assistant message follows another assistant message")
	}
	if len(m.ToolCalls) == 0 {
		if m.Content == "" || !m.ProviderBlocks.IsZero() {
			return errors.New("an assistant message without tool calls is empty or carries provider blocks")
		}
		b.add(roleModel, part{Text: m.Content})
		return nil
	}
	parts, wire, err := replayParts(m, b.model)
	if err != nil {
		return err
	}
	b.pending, b.wire, b.next = m.ToolCalls, wire, 0
	b.add(roleModel, parts...)
	return nil
}

// replayParts returns the parts of the reply an assistant message with tool calls stands for, from its
// ProviderBlocks, to be sent back exactly as they came, thought signatures included. The blocks must come
// from this adapter for the configured model, hold only the part types parseResponse accepts, and their
// function calls must be the message's: the same number, in order, with the same IDs (the reply's, or the
// local ID of the position), names and arguments. Arguments are compared after removing control characters
// from both, since the caller keeps a call's arguments without them (providers.WithoutControls) while the
// blocks hold them as the model wrote them. It also returns, for each call, whether its ID came from the
// reply. Error text is fixed and never quotes the blocks.
func replayParts(m providers.Message, model string) ([]part, []bool, error) {
	invalid := errors.New("the provider blocks of an assistant message do not match its tool calls")
	if m.ProviderBlocks.IsZero() || len(m.ToolCalls) > maxToolCalls {
		return nil, nil, invalid
	}
	raw := m.ProviderBlocks.Bytes()
	var b blocks
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if !uniqueKeys(raw) || dec.Decode(&b) != nil || b.Model != model || len(b.Parts) == 0 {
		return nil, nil, invalid
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, invalid
	}
	parts := make([]part, 0, len(b.Parts))
	var text strings.Builder
	wire := make([]bool, 0, len(m.ToolCalls))
	seen := map[string]bool{}
	for _, r := range b.Parts {
		pt, err := decodePart(r)
		if err != nil {
			return nil, nil, invalid
		}
		if pt.FunctionCall == nil && (pt.Thought == nil || !*pt.Thought) {
			text.WriteString(*pt.Text)
		}
		if pt.FunctionCall != nil {
			k := len(wire)
			if k >= len(m.ToolCalls) {
				return nil, nil, invalid
			}
			got, fromWire, err := callOf(pt.FunctionCall, k)
			want := m.ToolCalls[k]
			if err != nil || seen[got.ID] || got.ID != want.ID || got.Name != want.Name ||
				!bytes.Equal(providers.StripControlsJSON(got.Arguments),
					providers.StripControlsJSON(bytes.TrimSpace(want.Arguments))) {
				return nil, nil, invalid
			}
			seen[got.ID] = true
			wire = append(wire, fromWire)
		}
		parts = append(parts, part{raw: r})
	}
	// The message's text is the reply's text: the caller may hold it without control characters.
	if len(wire) != len(m.ToolCalls) ||
		providers.StripControls(m.Content) != providers.StripControls(text.String()) {
		return nil, nil, invalid
	}
	return parts, wire, nil
}

// unsupportedBounds are the string and array bounds that are not sent in responseJsonSchema. They are
// removed and stated in the description instead; the caller validates the reply against its full schema.
var unsupportedBounds = map[string]bool{
	"minLength": true, "maxLength": true, "pattern": true, "minItems": true, "maxItems": true, "uniqueItems": true,
}

// outputSchema rewrites a response schema for responseJsonSchema. It walks schema positions only
// (properties, items, anyOf, allOf, oneOf, prefixItems, $defs, definitions, not, additionalProperties), so a
// property named like a keyword is kept. The output is deterministic (sorted keys and notes).
func outputSchema(raw json.RawMessage) (json.RawMessage, error) {
	if !isJSONObject(raw) {
		return nil, errors.New("not a JSON object")
	}
	var s map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	rewriteSchema(s)
	return json.Marshal(s)
}

func rewriteSchema(s map[string]any) {
	var moved []string
	for k, v := range s {
		if unsupportedBounds[k] {
			b, _ := json.Marshal(v)
			moved = append(moved, k+" "+string(b))
			delete(s, k)
		}
	}
	if len(moved) > 0 {
		slices.Sort(moved)
		note := "Bounds: " + strings.Join(moved, ", ") + "."
		if d, ok := s["description"].(string); ok && d != "" {
			note = d + " " + note
		}
		s["description"] = note
	}
	for _, k := range []string{"items", "not", "additionalProperties"} {
		if sub, ok := s[k].(map[string]any); ok {
			rewriteSchema(sub)
		}
	}
	for _, k := range []string{"anyOf", "allOf", "oneOf", "prefixItems"} {
		if list, ok := s[k].([]any); ok {
			for _, e := range list {
				if sub, ok := e.(map[string]any); ok {
					rewriteSchema(sub)
				}
			}
		}
	}
	for _, k := range []string{"properties", "$defs", "definitions"} {
		if m, ok := s[k].(map[string]any); ok {
			for _, e := range m {
				if sub, ok := e.(map[string]any); ok {
					rewriteSchema(sub)
				}
			}
		}
	}
}

// parseResponse decodes and checks the body of a 200 reply. model is the configured model the provider blocks
// are bound to; offered names the tools the request offered; structured says a response schema was sent. The
// reply was billed, so on ErrMalformedOutput the Completion carries Usage and Model only, or UsageUnknown when
// no usable usage could be read. Error text is fixed and never quotes the reply.
func parseResponse(data []byte, model string, offered map[string]bool, structured bool) (providers.Completion, error) {
	malformed := func(msg string) error { return fmt.Errorf("vertexai: %s: %w", msg, providers.ErrMalformedOutput) }
	unknown := providers.Completion{UsageUnknown: true}
	if !uniqueKeys(data) {
		return unknown, malformed("the reply is not JSON, or repeats a member name")
	}
	var r generateResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return unknown, malformed("the reply is not a generateContent reply")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return unknown, malformed("the reply has data after the object")
	}
	u, ok := usageOf(r.UsageMetadata)
	if !ok {
		return unknown, malformed("the reply has no usage, or its usage is out of range or inconsistent")
	}
	version := modelVersionOf(r.ModelVersion)
	if !validModel(version) {
		return providers.Completion{Usage: u}, malformed("the reply names no valid model version")
	}
	billed := providers.Completion{Usage: u, Model: version}
	fail := func(msg string) (providers.Completion, error) { return billed, malformed(msg) }

	var cands []candidate
	if err := json.Unmarshal(r.Candidates, &cands); err != nil || len(cands) != 1 {
		return fail("the reply does not have exactly one candidate")
	}
	cand := cands[0]
	switch cand.FinishReason {
	case finishStop:
	case "MAX_TOKENS":
		return fail("the reply was cut off at the output token limit")
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return fail("the reply was filtered")
	case "MALFORMED_FUNCTION_CALL":
		return fail("the reply has a malformed function call")
	default:
		return fail("the reply ended for a reason other than STOP")
	}
	if cand.Content == nil || cand.Content.Role != roleModel || len(cand.Content.Parts) == 0 {
		return fail("the candidate has no parts of the model")
	}
	var text strings.Builder
	var calls []providers.ToolCall
	seen := map[string]bool{}
	for _, raw := range cand.Content.Parts {
		pt, err := decodePart(raw)
		if err != nil {
			return fail(err.Error())
		}
		if pt.FunctionCall == nil {
			if pt.Thought == nil || !*pt.Thought {
				text.WriteString(*pt.Text)
			}
			// A thought is not part of the answer: it reaches the caller only inside ProviderBlocks, opaque.
			continue
		}
		if len(calls) >= maxToolCalls {
			return fail("the reply has too many function calls")
		}
		tc, _, err := callOf(pt.FunctionCall, len(calls))
		switch {
		case err != nil:
			return fail(err.Error())
		case seen[tc.ID]:
			return fail("a function call ID is repeated")
		case !offered[tc.Name]:
			return fail("a function call names a tool the request did not offer")
		}
		seen[tc.ID] = true
		calls = append(calls, tc)
	}
	answer := text.String()
	if len(calls) == 0 {
		switch {
		case answer == "":
			return fail("the reply has no text and no function call")
		case structured && !isJSONObject([]byte(answer)):
			return fail("the reply is not the JSON object the response schema asks for")
		}
	}
	c := providers.Completion{Content: answer, ToolCalls: calls, Usage: u, Model: version}
	if len(calls) > 0 {
		// Every part goes back on the next turn of the tool loop, thoughts and signatures included and unchanged.
		c.ProviderBlocks = providers.NewOpaque(encodeBlocks(model, cand.Content.Parts))
	}
	return c, nil
}

// encodeBlocks writes the provider blocks of a reply: the configured model and the reply's parts, each the
// exact bytes of a JSON value the reply held. It writes them itself because json.Marshal would escape HTML
// characters inside the parts.
func encodeBlocks(model string, parts []json.RawMessage) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":`)
	b.WriteString(strconv.Quote(model)) // a model ID: lower-case letters, digits, dots and hyphens only
	b.WriteString(`,"parts":[`)
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(p)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// decodePart decodes one part of a reply, or of provider blocks: a JSON object with only the keys in
// partKeys, that is either text (possibly a thought) or a function call with only the keys in callKeys.
// Error text is fixed.
func decodePart(raw json.RawMessage) (replyPart, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
		return replyPart{}, errors.New("a part is not a JSON object")
	}
	for k := range keys {
		if !partKeys[k] {
			return replyPart{}, errors.New("a part has a field the adapter does not accept")
		}
	}
	if fc, ok := keys["functionCall"]; ok {
		var callFields map[string]json.RawMessage
		if err := json.Unmarshal(fc, &callFields); err != nil || callFields == nil {
			return replyPart{}, errors.New("a function call is not a JSON object")
		}
		for k := range callFields {
			if !callKeys[k] {
				return replyPart{}, errors.New("a function call has a field the adapter does not accept")
			}
		}
	}
	var pt replyPart
	if err := json.Unmarshal(raw, &pt); err != nil {
		return replyPart{}, errors.New("a part has a field of the wrong type")
	}
	if (pt.Text == nil) == (pt.FunctionCall == nil) {
		return replyPart{}, errors.New("a part is not either text or a function call")
	}
	if pt.ThoughtSignature != nil && !validSignature(*pt.ThoughtSignature) {
		return replyPart{}, errors.New("a thought signature is not base64")
	}
	if pt.FunctionCall != nil && pt.Thought != nil && *pt.Thought {
		return replyPart{}, errors.New("a function call is marked as a thought")
	}
	return pt, nil
}

// validSignature reports whether s is base64, in the standard or the URL-safe alphabet with or without padding:
// the thought signature is a bytes field, and a value Vertex cannot decode would be rejected on the next turn.
func validSignature(s string) bool {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if _, err := enc.DecodeString(s); err == nil {
			return true
		}
	}
	return false
}

// callOf checks a function call at position k of its reply and returns it as a tool call: the reply's ID, or
// the local ID of the position when the reply gave none, and whether the ID came from the reply. A name or ID
// with a control character, an invalid name or ID, or arguments that are not a JSON object are refused (absent
// arguments are an empty object). Error text is fixed.
func callOf(fc *functionCall, k int) (providers.ToolCall, bool, error) {
	id := ""
	if fc.ID != nil {
		id = *fc.ID
	}
	fromWire := fc.ID != nil
	switch {
	case strings.ContainsFunc(fc.Name, unicode.IsControl):
		return providers.ToolCall{}, false, errors.New("a function call name holds a control character")
	case strings.ContainsFunc(id, unicode.IsControl):
		return providers.ToolCall{}, false, errors.New("a function call ID holds a control character")
	case !toolNamePattern.MatchString(fc.Name):
		return providers.ToolCall{}, false, errors.New("a function call name is invalid")
	case fromWire && !toolCallIDPattern.MatchString(id):
		return providers.ToolCall{}, false, errors.New("a function call ID is invalid")
	}
	if !fromWire {
		id = localID(k)
	}
	args := bytes.TrimSpace(fc.Args)
	if len(args) == 0 {
		args = []byte("{}")
	}
	// Compact, so that the arguments read from the reply and from the provider blocks, which the request
	// encoder compacts, compare equal byte for byte. Compacting leaves every string as it is.
	var compact bytes.Buffer
	if !isJSONObject(args) || json.Compact(&compact, args) != nil {
		return providers.ToolCall{}, false, errors.New("the arguments of a function call are not a JSON object")
	}
	return providers.ToolCall{ID: id, Name: fc.Name, Arguments: json.RawMessage(compact.Bytes())}, fromWire, nil
}

// localID is the ID of the function call at position k of a reply that gave it none.
func localID(k int) string { return localIDPrefix + strconv.Itoa(k) }

// usageOf converts usage metadata, and reports false when there is none, a count is out of range, the cached
// count is above the prompt's, or the total is not the sum of the counts. Absent counts are zero, as the API
// omits zero values.
func usageOf(u *usageMetadata) (providers.Usage, bool) {
	if u == nil {
		return providers.Usage{}, false
	}
	for _, n := range []int64{u.PromptTokenCount, u.CandidatesTokenCount, u.ThoughtsTokenCount,
		u.ToolUsePromptTokenCount, u.CachedContentTokenCount, u.TotalTokenCount} {
		if n < 0 || n > maxUsageTokens {
			return providers.Usage{}, false
		}
	}
	input := u.PromptTokenCount + u.ToolUsePromptTokenCount
	output := u.CandidatesTokenCount + u.ThoughtsTokenCount
	if u.CachedContentTokenCount > u.PromptTokenCount || u.TotalTokenCount != input+output {
		return providers.Usage{}, false
	}
	return providers.Usage{InputTokens: input, CachedInputTokens: u.CachedContentTokenCount, OutputTokens: output}, true
}

// modelVersionOf reads a modelVersion that was kept raw, so that one of the wrong type costs the reply its model
// and not the usage it reported. It returns "" unless the value is a JSON string.
func modelVersionOf(raw json.RawMessage) string {
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v
}

func validModel(m string) bool { return len(m) <= maxModelName && modelVersionPattern.MatchString(m) }

// billedUsage reads only the usage and model version of a 200 reply that was refused before it was parsed (a
// Content-Type other than JSON, a body too large to read, a read that failed). data is the body, or nil when
// it was not read whole. Without a usable usage the Completion says UsageUnknown.
func billedUsage(data []byte) providers.Completion {
	var r struct {
		ModelVersion  json.RawMessage `json:"modelVersion"`
		UsageMetadata *usageMetadata  `json:"usageMetadata"`
	}
	if data == nil || !uniqueKeys(data) || json.Unmarshal(data, &r) != nil {
		return providers.Completion{UsageUnknown: true}
	}
	u, ok := usageOf(r.UsageMetadata)
	if !ok {
		return providers.Completion{UsageUnknown: true}
	}
	c := providers.Completion{Usage: u}
	if v := modelVersionOf(r.ModelVersion); validModel(v) {
		c.Model = v
	}
	return c
}

// uniqueKeys reports whether data is JSON in which no object has two member names that encoding/json would
// read as the same field (it matches names without regard to case), so what the adapter reads from a reply,
// and checks of the parts it sends back, is what any reader of them reads.
func uniqueKeys(data []byte) bool {
	type frame struct {
		keys      map[string]bool
		object    bool
		expectKey bool
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var stack []frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return len(stack) == 0
		}
		if err != nil {
			return false
		}
		n := len(stack)
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if n > 0 && stack[n-1].object {
					// The container is the value of a member: a name comes next once it closes.
					stack[n-1].expectKey = true
				}
				if len(stack) >= maxJSONDepth {
					return false
				}
				f := frame{object: d == '{', expectKey: d == '{'}
				if f.object {
					f.keys = map[string]bool{}
				}
				stack = append(stack, f)
			default:
				stack = stack[:n-1]
			}
			continue
		}
		if n == 0 || !stack[n-1].object {
			continue
		}
		f := &stack[n-1]
		if !f.expectKey {
			f.expectKey = true
			continue
		}
		k, _ := tok.(string)
		k = strings.ToLower(strings.ToUpper(k))
		if f.keys[k] {
			return false
		}
		f.keys[k] = true
		f.expectKey = false
	}
}

// isJSONObject reports whether b is one valid JSON object, surrounded by white space at most.
func isJSONObject(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}
