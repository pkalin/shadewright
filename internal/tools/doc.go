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
//     and values of the schema's types, then each value is checked (names, enums, ranges);
//   - a kind allow list where it names objects: the kinds the analyzer already reads (Pod, Deployment,
//     StatefulSet, ReplicaSet), Secret and ConfigMap for the missing reference check (which never reads
//     either), Flux's Kustomization and HelmRelease for the delivery status;
//   - the namespace check: a namespace outside the watched set is refused before any read, and an empty
//     set refuses every namespace;
//   - a timeout for its reads (Config.Timeout), and a byte bound on its result.
//
// Arguments come from the model and are untrusted (invariant 4, and invariant 1: model output is never
// authorization): they choose what to read within those bounds, never how. A refusal says which rule refused
// in a fixed text and never echoes an argument. Results are cluster data, untrusted too: every piece of text
// goes through the [Redactor] before any cut, values are left out whole when longer than 1 KiB and their
// control characters become spaces, the assembled text is redacted again, and only whole lines are cut from
// redacted text. A log excerpt first loses the leading lines that may continue an entry whose first line the
// source left out, before redaction (as in package incidentcontext). The error text of a failed read never
// reaches the result. [Result.Content] puts the text in a delimited block that says it is data, never
// instructions, and that the text cannot close.
//
// query_metrics names one pod (or, for the Deployment template, one Deployment) that the tool reads first in
// the requested namespace, so a query only ever selects an object that exists there, never a name pattern.
package tools
