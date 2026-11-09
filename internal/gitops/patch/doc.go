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
// Package patch changes one value of a catalog action in a manifest, in place, as ADR-0020 decides.
//
// The document bytes (one or more YAML documents) go in, the new bytes come out. The node parser
// go.yaml.in/yaml/v3 only locates the target scalar; the patch replaces just that scalar's bytes, so comments,
// key order, blank lines, quoting and every other document stay byte for byte. Before replacing, the current
// value must equal the expected previous one (quantities compare as quantities, so 256Mi equals 268435456).
// After replacing, the patched file is decoded and must differ from the decoded original only in the target
// value; otherwise the patch fails closed.
//
// Before anything decodes it, the file passes the fail-closed check of internal/gitops/gityaml, as in the
// source mapping and the semantic diff: input that is not UTF-8, YAML anchors, aliases and merge keys anywhere
// in the file, text after "---" on a separator line other than a comment, a line break other than LF or CRLF,
// YAML directives and more than sourcemap.MaxDocuments objects are refused with [ReasonInvalidYAML], so no
// decode expands an alias (#595). The file is then read one document at a time, keeping only the node tree of
// the target document and the decoded values of the others, and a file of more than [MaxNodes] YAML nodes is
// refused with [ReasonInvalidYAML]; the verification reuses those values instead of parsing the original
// again, so the patcher holds about one node tree at a time.
//
// Anything the splice cannot do safely is refused with an [Error] and never guessed: a flow collection on the
// path, a tag on the value, a value that is not a plain, single-quoted or double-quoted scalar, a duplicate key,
// an identity field (apiVersion, kind, metadata, name, namespace) of any document with an explicit tag, no or
// several matching objects or containers, and input over [MaxBytes]. Errors never contain text from the
// document, which may be untrusted.
package patch
