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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Refusals shown to the model. They name the rule, never the value.
const (
	refuseShape     refusal = "the arguments do not match the schema"
	refuseSize      refusal = "the arguments are larger than allowed"
	refuseNamespace refusal = "the namespace is not one the analyzer watches"
	refuseKind      refusal = "the kind is not in the tool's allow list"
	refuseName      refusal = "a name is not a valid Kubernetes object name"
	refuseRange     refusal = "a number is outside the range the schema allows"
	refusePath      refusal = "the path is not a clean relative path within the bounds"
	refuseRef       refusal = "the ref is not a full commit ID"
	refuseTarget    refusal = "the template needs the other target: a pod for a pod template, a deployment for the deployment template"
)

// decode parses raw into dst, a pointer to a struct whose json tags are the schema's property names. It
// accepts exactly one JSON object of at most MaxArgumentBytes, whose keys are among those names
// (case-sensitive, each at most once), with values of the fields' types and nothing after it. encoding/json
// alone would match keys case-insensitively, keep the last of duplicate keys and accept trailing data.
func decode(raw []byte, dst any) error {
	if len(raw) > MaxArgumentBytes {
		return refuseSize
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	allowed := fieldNames(dst)
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return refuseShape
	}
	var seen []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return refuseShape
		}
		key, ok := tok.(string)
		if !ok || !slices.Contains(allowed, key) || slices.Contains(seen, key) {
			return refuseShape
		}
		seen = append(seen, key)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return refuseShape
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return refuseShape
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return refuseShape
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(dst); err != nil {
		return refuseShape
	}
	return nil
}

// fieldNames returns the json names of the fields of the struct dst points to.
func fieldNames(dst any) []string {
	t := reflect.TypeOf(dst).Elem()
	names := make([]string, 0, t.NumField())
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		names = append(names, name)
	}
	return names
}

// namespace checks a namespace argument: a DNS-1123 label in the watched set.
func (s *Set) namespace(ns string) error {
	if len(validation.IsDNS1123Label(ns)) > 0 || !s.watched(ns) {
		return refuseNamespace
	}
	return nil
}

// objectName checks the name of an object: a DNS-1123 subdomain.
func objectName(n string) error {
	if len(validation.IsDNS1123Subdomain(n)) > 0 {
		return refuseName
	}
	return nil
}

// labelName checks a name that must be a DNS-1123 label, such as a container's.
func labelName(n string) error {
	if len(validation.IsDNS1123Label(n)) > 0 {
		return refuseName
	}
	return nil
}

// inRange checks an optional number: zero means unset; otherwise it is within lo and hi.
func inRange(v, lo, hi int) error {
	if v != 0 && (v < lo || v > hi) {
		return refuseRange
	}
	return nil
}

// Bounds of a Git path argument.
const maxGitPath = 512

var gitPathPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// gitPath checks a repository path: relative, clean, no "." or ".." element, characters from a small set.
func gitPath(p string) error {
	if p == "" || len(p) > maxGitPath || !gitPathPattern.MatchString(p) || strings.HasPrefix(p, "/") ||
		path.Clean(p) != p || p == "." {
		return refusePath
	}
	for e := range strings.SplitSeq(p, "/") {
		if e == "." || e == ".." {
			return refusePath
		}
	}
	return nil
}
