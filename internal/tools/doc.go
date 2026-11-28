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

// Package tools holds the read tools the analyzer offers the model when an analysis escalates (design
// section 4, ADR-0023 decisions 4 and 6). They are provider-neutral: each has a name, a description and a
// JSON schema of its arguments ([providers.ToolDefinition]), and the analysis loop maps them to the format of
// the model provider in use.
//
// The tools are get_resource, list_events, get_pod_logs, query_metrics, get_delivery_status, read_git_file,
// get_recent_changes and check_missing_reference (the missing reference check of design 4.3, package
// refcheck). There is no proposal tool: a proposal is only the structured output of the call (ADR-0023
// decision 5, design 4.4), so every tool here reads and none writes to the cluster or to Git (invariants 2, 3
// and 6). A tool whose source is not configured answers "not available" and is left out of [Set.Definitions];
// the Git tools always are for now, because the analyzer has no Git identity (#373).
//
// Every tool has:
//
//   - an argument schema, enforced in Go before any read: the arguments are one JSON object of at most
//     [MaxArgumentBytes] with exactly the schema's property names (case-sensitive, no duplicates, no others)
//     and values of the schema's types (an integer in any JSON form with a zero fractional part; a fraction
//     is refused, and an explicit 0 is a value, not an absent argument), then each value is checked (names,
//     enums, ranges);
//   - a kind allow list where it names objects: the kinds the analyzer already reads (Pod, Deployment,
//     StatefulSet, ReplicaSet), Secret and ConfigMap for the missing reference check (which never reads
//     either), Flux's Kustomization and HelmRelease for the delivery status;
//   - the namespace check: a namespace outside the watched set is refused before any read, and an empty
//     set refuses every namespace; get_delivery_status checks against the delivery namespaces instead
//     (Config.DeliveryNamespaces, none unless set), which widen no other read;
//   - a timeout for its reads (Config.Timeout), and a byte bound on its result.
//
// Arguments come from the model and are untrusted (invariant 4, and invariant 1: model output is never
// authorization): they choose what to read within those bounds, never how. A refusal says which rule refused in a
// fixed text and never echoes an argument. Results are cluster data, untrusted too: every piece of text goes through
// the [Redactor] before any cut; in an answer written as lines, a value longer than 1 KiB once decided is left out
// whole and its control characters become spaces, and only whole lines are cut from redacted text (get_resource
// bounds its JSON instead, by leaveOut below), except that a log excerpt whose last line alone is longer than what
// is left keeps the tail of that line, and that a log excerpt keeps the first line of an entry ahead of its newest
// continuation lines with the lines between left out (package logcut); the assembled text is not redacted again
// (#845). A
// log excerpt is rendered by package incidentcontext's RenderLog, as in the context: it first loses the leading
// lines that may continue an entry whose first line the source left out, before redaction, its header is budgeted
// first, and every cut keeps the header and the newest lines. get_resource leaves out annotations, managed fields,
// literal environment values, the metadata of volume claim templates and the containers' termination messages. Every
// tool decides its answer once, on the text it returns (layout.go, #845): the object's JSON for get_resource, whose
// string tokens are walked in one pass and are the pieces, the lines for the others but get_pod_logs, whose excerpt
// RenderLog decides as above, each value written as the answer writes it; a key in one field or list item and its value
// in a later one are seen together, a value under a secret key the object gives (a map key, a field name) is
// hidden whole, line breaks included, and keys that are decided or written alike are renamed apart, so a map keeps
// every entry, unless the answer is over its bound: values, then long keys, then the whole answer may be left out,
// each replaced by a note of its size (leaveOut). The error text of a failed read never reaches the result.
// [Result.Content] puts the text in a delimited block that says it is data, never instructions, and that the
// text cannot close: every angle bracket and look-alike in it is escaped.
//
// query_metrics writes each label of a series as "name=value", the value under its name, so a secret label name
// hides its whole value (#845). It names one pod (or, for the Deployment template, one Deployment) that the tool reads first in
// the requested namespace, so a query only ever selects an object that exists there, never a name pattern.
package tools
