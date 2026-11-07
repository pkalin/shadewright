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

// Package eval is the evaluation harness of the analysis loop (design 11.1 point 7, ADR-0025 decision 3,
// #78). Every lab scenario of the fleet repository has a recorded context under testdata/scenarios: the
// Incident and the cluster objects the context builder and the read tools read, for some the series their
// Prometheus holds (metrics.yaml, replayed through the MetricsSource interface), the scenario's
// expected.yaml as vendored from a named fleet commit, a rubric that says how each expected fact is scored,
// and the model replies recorded for it.
//
// A run builds each scenario's context with the real context builder (package incidentcontext) over a fake
// client that holds the recorded objects, and runs the real analysis loop (package analysis) with the real
// read tools. In CI the model is a fake that replays the recorded replies; on demand (`make eval-model`, by
// hand; the Evaluation workflow is planned) it is the Foundry deployment the run names. Each scenario is scored
// against its expected.yaml: the outcome
// (a refusal is no plan and a reason that the result itself gave, NoProposal or MissingData; a pull request
// is a plan of the expected action type with a desired value inside the expected bounds) and the diagnosis
// (each scored fact's terms in the result's summary, hypotheses and evidence quotes).
//
// The harness only reads the outcome the loop returned and the reply the loop accepted: nothing it computes
// authorizes anything (invariant 1), and it runs no executor, no policy decision and no Git write.
//
// The report (JSON and Markdown) and its evaluation manifest are documented in docs/evaluation.md.
package eval
