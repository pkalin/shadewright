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

package modules

// Symptom is what a module's rule found: a reason and a summary the rule wrote itself, and evidence read from
// the cluster.
type Symptom struct {
	// Reason is a CamelCase word written by the rule (^[A-Z][A-Za-z0-9]{0,127}$), never cluster text.
	Reason string
	// Summary is fixed text written by the rule, never cluster text, at most 256 bytes.
	Summary string
	// Evidence is what the rule read from the cluster, at most 8 entries.
	Evidence []Evidence
}

// Evidence is one value a rule read from the cluster.
type Evidence struct {
	// Field says where the value was read, written by the rule (^[A-Za-z0-9./_-]{1,128}$).
	Field string
	// Value is the value read. It is untrusted: the evaluator redacts it with fingerprint.Normalize and bounds
	// it to 256 bytes.
	Value string
}
