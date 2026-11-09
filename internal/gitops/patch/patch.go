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
package patch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/gityaml"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// MaxBytes is the largest input accepted, as for files read from Git.
const MaxBytes = 1 << 20

// MaxNodes is the most YAML nodes (scalars, collections and documents) a file may hold. The patcher reads a
// file one document at a time and keeps at most the target document besides the one it reads, so the nodes it
// holds at once stay under this bound: about 22 MiB of node trees. A file of MaxBytes packed with nodes holds
// about 500,000, and their tree alone takes about 85 MiB of the executor's 128 MiB (#595).
const MaxNodes = 1 << 17

var wholeNumber = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})$`)

// SetContainerMemoryLimit replaces resources.limits.memory of the container named in p in the one Deployment
// or StatefulSet of data that declares target. The current value must equal p.Previous as a quantity.
func SetContainerMemoryLimit(data []byte, target v1alpha1.ObjectRef, p v1alpha1.SetContainerMemoryLimit) (out []byte, err error) {
	defer failClosed(&err)
	if p.Desired.Sign() <= 0 || p.Previous.Sign() <= 0 {
		return nil, refuse(ReasonInvalidInput, "memory limits must be positive")
	}
	field, err := sourcemap.FieldFor(v1alpha1.Action{
		Type:                    v1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &p,
	}, target)
	if err != nil {
		return nil, refuse(ReasonInvalidInput, err.Error())
	}
	return splice(data, target, field, p.Previous.String(), p.Desired.String())
}

// AdjustKedaMaxReplicas replaces spec.maxReplicaCount of the one ScaledObject of data that declares target.
// The current value must be the plain integer p.PreviousValue.
func AdjustKedaMaxReplicas(data []byte, target v1alpha1.ObjectRef, p v1alpha1.AdjustKedaMaxReplicas) (out []byte, err error) {
	defer failClosed(&err)
	if p.DesiredValue < 0 || p.PreviousValue < 0 {
		return nil, refuse(ReasonInvalidInput, "replica counts must not be negative")
	}
	field, err := sourcemap.FieldFor(v1alpha1.Action{
		Type:                  v1alpha1.ActionAdjustKedaMaxReplicas,
		AdjustKedaMaxReplicas: &p,
	}, target)
	if err != nil {
		return nil, refuse(ReasonInvalidInput, err.Error())
	}
	return splice(data, target, field, fmt.Sprint(p.PreviousValue), fmt.Sprint(p.DesiredValue))
}

// failClosed turns a panic in the YAML library into a refusal, so no input can crash the caller.
func failClosed(err *error) {
	if recover() != nil {
		*err = refuse(ReasonInvalidYAML, "the parser failed on this input")
	}
}

// splice does the work for both actions. previous and desired are the canonical texts of the values.
func splice(data []byte, target v1alpha1.ObjectRef, field sourcemap.Field, previous, desired string) ([]byte, error) {
	if len(data) > MaxBytes {
		return nil, refuse(ReasonInvalidYAML, "the file is too large")
	}
	if err := check(data); err != nil {
		return nil, err
	}
	found, err := findDocument(data, target)
	if err != nil {
		return nil, err
	}
	node, err := locate(found.doc, field)
	if err != nil {
		return nil, err
	}
	if err := checkScalar(node, field, previous); err != nil {
		return nil, err
	}
	start, end, err := tokenBounds(data, node)
	if err != nil {
		return nil, err
	}
	quote := quoteOf(node)
	out := make([]byte, 0, len(data)+len(desired))
	out = append(out, data[:start]...)
	out = append(out, quote+desired+quote...)
	out = append(out, data[end:]...)
	if err := verify(found, out, field, desired); err != nil {
		return nil, err
	}
	return out, nil
}

// check runs the fail-closed check every YAML file read from Git passes (internal/gitops/gityaml) before
// anything decodes data, so anchors, aliases, merge keys and the forms that hide them are refused before a
// decode could expand them (#595, invariant 4). The source mapping and the semantic diff refuse the same files.
func check(data []byte) error {
	n, err := gityaml.Objects(data, sourcemap.MaxDocuments)
	if err != nil {
		// The text of a check error names a line and a cause, never text from the file.
		var ge *gityaml.Error
		if errors.As(err, &ge) && ge.Line == 0 {
			return refuse(ReasonInvalidYAML, "the file "+err.Error())
		}
		return refuse(ReasonInvalidYAML, err.Error())
	}
	if n > sourcemap.MaxDocuments {
		return refuse(ReasonInvalidYAML, fmt.Sprintf("the file holds more than %d YAML documents", sourcemap.MaxDocuments))
	}
	return nil
}

// documents reads a file one document at a time, so that only the node tree of the document read last is held,
// and counts the nodes of the file against MaxNodes.
type documents struct {
	dec   *yaml.Decoder
	nodes int
}

func newDocuments(data []byte) *documents {
	return &documents{dec: yaml.NewDecoder(bytes.NewReader(data))}
}

// next returns the next document as nodes and decoded, so invalid YAML and duplicate keys are refused before
// anything is located, or io.EOF after the last one. It refuses the document that takes the file over
// MaxNodes before decoding it.
func (d *documents) next() (*yaml.Node, any, error) {
	var n yaml.Node
	err := d.dec.Decode(&n)
	if errors.Is(err, io.EOF) {
		return nil, nil, io.EOF
	}
	if err != nil {
		return nil, nil, refuse(ReasonInvalidYAML, "the file is not valid YAML")
	}
	if d.nodes += countNodes(&n, MaxNodes-d.nodes); d.nodes > MaxNodes {
		return nil, nil, refuse(ReasonInvalidYAML, fmt.Sprintf("the file holds more than %d YAML nodes", MaxNodes))
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, nil, refuse(ReasonInvalidYAML, "the file is not valid YAML")
	}
	return &n, v, nil
}

// countNodes returns the nodes of the tree under n, n included; it stops counting once the count is over limit.
func countNodes(n *yaml.Node, limit int) int {
	c := 1
	for _, ch := range n.Content {
		if c > limit {
			break
		}
		c += countNodes(ch, limit-c)
	}
	return c
}

// original is what findDocument keeps of the file to patch: the document that declares the target, and what
// verify compares the patched file with.
type original struct {
	// idx is the index of the document.
	idx int
	// doc is the node tree of the document, the only one kept.
	doc *yaml.Node
	// values holds every document of the file decoded, in order.
	values []any
}

// findDocument returns the only document of data that declares ref, reading data one document at a time and
// keeping the node tree of that one alone. As in the source mapping, a document with the same group, kind and
// name but another version, or no namespace, makes it ambiguous.
func findDocument(data []byte, ref v1alpha1.ObjectRef) (original, error) {
	want, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return original{}, refuse(ReasonInvalidInput, "invalid apiVersion")
	}
	exact, near, nExact := -1, 0, 0
	var (
		doc    *yaml.Node
		values []any
	)
	docs := newDocuments(data)
	for i := 0; ; i++ {
		d, v, err := docs.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return original{}, err
		}
		values = append(values, v)
		root := documentRoot(d)
		if root == nil || root.Kind != yaml.MappingNode {
			continue
		}
		if err := checkIdentity(root); err != nil {
			return original{}, err
		}
		apiVersion, _ := scalarAt(root, "apiVersion")
		kind, _ := scalarAt(root, "kind")
		var name, namespace string
		if meta := mapAt(root, "metadata"); meta != nil {
			name, _ = scalarAt(meta, "name")
			namespace, _ = scalarAt(meta, "namespace")
		}
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil || gv.Group != want.Group || kind != ref.Kind || name != ref.Name {
			continue
		}
		switch {
		case namespace == ref.Namespace && gv.Version == want.Version:
			exact, doc = i, d
			nExact++
		case namespace == ref.Namespace || namespace == "":
			near++
		}
	}
	what := fmt.Sprintf("%s %s/%s", ref.Kind, ref.Namespace, ref.Name)
	switch {
	case nExact == 1 && near == 0:
		return original{idx: exact, doc: doc, values: values}, nil
	case nExact == 0 && near == 0:
		return original{}, refuse(ReasonTargetNotFound, "no document declares "+what)
	default:
		return original{}, refuse(ReasonAmbiguousTarget, what+" may be declared more than once")
	}
}

// checkIdentity refuses a document whose apiVersion, kind or metadata.name and namespace come from an
// anchor, alias or merge key, since reading the source text would then miss what the object really is.
func checkIdentity(root *yaml.Node) error {
	unsupported := refuse(ReasonUnsupported, "a document takes its identity from an anchor, alias, merge key or explicit tag")
	// An explicit tag changes what a scalar decodes to (!!binary turns base64 text into the name), so the
	// source text the matching reads would not be the identity Kubernetes sees.
	tagged := func(n *yaml.Node) bool { return n.Style&yaml.TaggedStyle != 0 }
	merged := func(m *yaml.Node) bool {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if k := m.Content[i]; k.Tag == "!!merge" || k.Kind != yaml.ScalarNode || k.Anchor != "" || tagged(k) {
				return true
			}
		}
		return false
	}
	indirect := func(n *yaml.Node) bool { return n.Kind == yaml.AliasNode || n.Anchor != "" || tagged(n) }
	if merged(root) {
		return unsupported
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "apiVersion", "kind":
			if indirect(root.Content[i+1]) {
				return unsupported
			}
		case "metadata":
			meta := root.Content[i+1]
			if indirect(meta) {
				return unsupported
			}
			if meta.Kind != yaml.MappingNode {
				continue
			}
			if merged(meta) {
				return unsupported
			}
			for j := 0; j+1 < len(meta.Content); j += 2 {
				if k := meta.Content[j].Value; (k == "name" || k == "namespace") && indirect(meta.Content[j+1]) {
					return unsupported
				}
			}
		}
	}
	return nil
}

func documentRoot(d *yaml.Node) *yaml.Node {
	if d.Kind != yaml.DocumentNode || len(d.Content) != 1 {
		return nil
	}
	return d.Content[0]
}

// mapAt returns the mapping under key, or nil.
func mapAt(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalarAt(m *yaml.Node, key string) (string, bool) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.ScalarNode {
			return m.Content[i+1].Value, true
		}
	}
	return "", false
}

// locate walks the field's steps from the document root and returns the target scalar. Every node on the
// path must be a block collection without anchor, alias, tag or merge key.
func locate(doc *yaml.Node, field sourcemap.Field) (*yaml.Node, error) {
	cur := documentRoot(doc)
	for _, step := range field.Steps {
		if err := checkCollection(cur, yaml.MappingNode); err != nil {
			return nil, err
		}
		next, err := child(cur, step.Key)
		if err != nil {
			return nil, err
		}
		if step.Name == "" {
			cur = next
			continue
		}
		if err := checkCollection(next, yaml.SequenceNode); err != nil {
			return nil, err
		}
		var found []*yaml.Node
		for _, item := range next.Content {
			if item.Kind == yaml.MappingNode {
				if v, ok := scalarAt(item, "name"); ok && v == step.Name {
					found = append(found, item)
				}
			}
		}
		switch len(found) {
		case 0:
			return nil, refuse(ReasonFieldMissing, "no container named "+step.Name)
		case 1:
			cur = found[0]
		default:
			return nil, refuse(ReasonAmbiguousContainer, "several containers are named "+step.Name)
		}
	}
	return cur, nil
}

// checkCollection refuses nodes the splice cannot reason about on the path to the value.
func checkCollection(n *yaml.Node, kind yaml.Kind) error {
	switch {
	case n.Kind == yaml.AliasNode || n.Anchor != "":
		return refuse(ReasonUnsupported, "an anchor or alias is on the path to the value")
	case n.Kind != kind:
		return refuse(ReasonFieldMissing, "a value on the path has the wrong type")
	case n.Style&yaml.FlowStyle != 0:
		return refuse(ReasonUnsupported, "a flow collection is on the path to the value")
	case n.Style&yaml.TaggedStyle != 0:
		return refuse(ReasonUnsupported, "a tag is on the path to the value")
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i]; k.Tag == "!!merge" || k.Kind != yaml.ScalarNode || k.Anchor != "" {
				return refuse(ReasonUnsupported, "a merge key or complex key is on the path to the value")
			}
		}
	}
	return nil
}

// child returns the value of key in a mapping that checkCollection accepted; a repeated key is refused.
func child(m *yaml.Node, key string) (*yaml.Node, error) {
	var found *yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			if found != nil {
				return nil, refuse(ReasonInvalidYAML, "duplicate key "+key)
			}
			found = m.Content[i+1]
		}
	}
	if found == nil {
		return nil, refuse(ReasonFieldMissing, "missing "+key)
	}
	return found, nil
}

// checkScalar requires a plain, single- or double-quoted scalar without anchor or tag, holding previous.
func checkScalar(n *yaml.Node, field sourcemap.Field, previous string) error {
	switch {
	case n.Kind == yaml.AliasNode || n.Anchor != "":
		return refuse(ReasonUnsupported, "the value has an anchor or is an alias")
	case n.Kind != yaml.ScalarNode:
		return refuse(ReasonUnsupported, "the value is not a scalar")
	case n.Style&yaml.TaggedStyle != 0:
		return refuse(ReasonUnsupported, "the value has a tag")
	case n.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.FlowStyle) != 0:
		return refuse(ReasonUnsupported, "the value is a block scalar")
	}
	if err := checkPlainNumber(n); err != nil {
		return err
	}
	if field.Quantity {
		if !field.Equals(n.Value, previous) {
			return refuse(ReasonPreviousValueDiffers, "the file holds another value than the plan")
		}
		return nil
	}
	// A replica count is a plain decimal integer; a quoted or non-decimal number is not what a ScaledObject holds.
	if n.Style != 0 || !wholeNumber.MatchString(n.Value) {
		return refuse(ReasonUnsupported, "the value is not a plain decimal integer")
	}
	if n.Value != previous {
		return refuse(ReasonPreviousValueDiffers, "the file holds another value than the plan")
	}
	return nil
}

// checkPlainNumber refuses a plain scalar that YAML reads as something other than text, unless it is a
// decimal integer written the way Go formats it. 010000000 is octal 2097152 to YAML but 10M to a quantity,
// and 0x, 0o, underscores and exponents have the same problem, so what the file means is not what its text
// says. Quoted scalars are strings and are fine.
func checkPlainNumber(n *yaml.Node) error {
	if n.Style != 0 || n.ShortTag() == "!!str" {
		return nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return refuse(ReasonUnsupported, "the value cannot be decoded")
	}
	var text string
	switch x := v.(type) {
	case int:
		text = strconv.Itoa(x)
	case int64:
		text = strconv.FormatInt(x, 10)
	case uint64:
		text = strconv.FormatUint(x, 10)
	}
	if text == "" || text != n.Value {
		return refuse(ReasonUnsupported, "the value is a number whose text and meaning may differ")
	}
	return nil
}

func quoteOf(n *yaml.Node) string {
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		return `"`
	case n.Style&yaml.SingleQuotedStyle != 0:
		return `'`
	}
	return ""
}

// tokenBounds turns the node's line and column (characters, not bytes) into the byte range of its token and
// checks that the bytes there are exactly the value in its quoting, so an escape or a multi-line scalar is
// refused instead of guessed.
func tokenBounds(data []byte, n *yaml.Node) (int, int, error) {
	notInPlace := refuse(ReasonUnsupported, "the value is not written in a form that can be replaced in place")
	start := 0
	for l := 1; l < n.Line; l++ {
		i := bytes.IndexByte(data[start:], '\n')
		if i < 0 {
			return 0, 0, notInPlace
		}
		start += i + 1
	}
	for c := 1; c < n.Column; c++ {
		if start >= len(data) || data[start] == '\n' {
			return 0, 0, notInPlace
		}
		_, size := utf8.DecodeRune(data[start:])
		start += size
	}
	q := quoteOf(n)
	token := []byte(q + n.Value + q)
	if !bytes.HasPrefix(data[start:], token) {
		return 0, 0, notInPlace
	}
	end := start + len(token)
	if end < len(data) {
		switch data[end] {
		case ' ', '\t', '\r', '\n':
		default:
			return 0, 0, notInPlace
		}
	}
	return start, end, nil
}

// verify decodes the patched file and requires that it differs from the original only in the target value,
// which must be desired and found by the same rules as in the original. It compares with the original's
// decoded values that findDocument kept instead of parsing the original again, and reads the patched file one
// document at a time, so it holds one node tree at a time (#595).
func verify(before original, after []byte, field sourcemap.Field, desired string) error {
	fail := refuse(ReasonVerificationFailed, "the patched file differs from the original in more than the target value")
	docs := newDocuments(after)
	for i := 0; ; i++ {
		na, a, err := docs.next()
		if errors.Is(err, io.EOF) {
			if i != len(before.values) {
				return fail
			}
			return nil
		}
		if err != nil || i >= len(before.values) {
			return fail
		}
		b := before.values[i]
		if i == before.idx {
			node, err := locate(na, field)
			if err != nil || node.Kind != yaml.ScalarNode || node.Value != desired || !copyTarget(b, a, field) {
				return fail
			}
		}
		if !equal(b, a) {
			return fail
		}
	}
}

// equal reports whether two decoded YAML values are deeply equal. It walks the maps and lists itself and
// compares only the other values with reflect.DeepEqual, whose record of visited maps, lists and interfaces
// would otherwise grow with every item of a large document (#595).
func equal(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if w, ok := y[k]; !ok || !equal(v, w) {
				return false
			}
		}
		return true
	case map[any]any:
		y, ok := b.(map[any]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if w, ok := y[k]; !ok || !equal(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// copyTarget copies the value at the field in src over the one in dst, in place, so that a following
// comparison ignores exactly that value.
func copyTarget(dst, src any, field sourcemap.Field) bool {
	for i, step := range field.Steps {
		dm, ok1 := dst.(map[string]any)
		sm, ok2 := src.(map[string]any)
		if !ok1 || !ok2 {
			return false
		}
		if i == len(field.Steps)-1 {
			if _, ok := dm[step.Key]; !ok {
				return false
			}
			dm[step.Key] = sm[step.Key]
			return true
		}
		dst, src = dm[step.Key], sm[step.Key]
		if step.Name != "" {
			dst, src = pick(dst, step.Name), pick(src, step.Name)
		}
	}
	return false
}

// pick returns the only element of a list of maps whose name equals name, or nil.
func pick(list any, name string) any {
	items, _ := list.([]any)
	var found any
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["name"] == name {
			if found != nil {
				return nil
			}
			found = m
		}
	}
	return found
}
