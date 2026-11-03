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

// Package kyverno is the Kyverno cluster integration module (ADR-0018, design 2.5): failing policy report
// results, policies that are not Ready and admission requests Kyverno refused become symptoms that name the
// policy, the rule and the resource. It diagnoses only.
//
// Detection: the cluster serves kyverno.io/v1 (ClusterPolicy, Policy), read at Kyverno tag v1.19.2. The
// policy report group wgpolicyk8s.io/v1alpha2 (PolicyReport, ClusterPolicyReport) is not a detection signal:
// other engines (Trivy, Kubewarden, kube-bench and more) serve and write it too. Its kinds are read only once
// Kyverno is detected, and then only Kyverno's results. Not read: reports.kyverno.io/v1, which holds only
// Kyverno's short-lived ephemeral reports that the reports controller merges into policy reports;
// openreports.io/v1alpha1, which Kyverno writes instead only when its openreportsEnabled flag is set
// (cmd/internal/flag.go, default false); and the policies.kyverno.io policy kinds themselves, whose results
// still reach the policy reports.
//
// Symptom sources, read only from the object itself. Their reporting controllers are the event sources Kyverno
// writes (kyverno-admission, kyverno-scan), so a report result and the event about it fingerprint alike:
//
//   - PolicyReport and ClusterPolicyReport: one symptom per result of a Kyverno policy (results[].source, see
//     [kyvernoSource]; Kubernetes' own ValidatingAdmissionPolicy and MutatingAdmissionPolicy results and other
//     engines' results are not read) with result fail (reason PolicyViolation) or error (PolicyError), in the
//     report's order, at most [MaxReportResults]. pass, warn and skip show nothing. Evidence is the policy, the
//     rule, the resource the result applies to (results[].resources[0], or the report's scope, where Kyverno
//     names it), the message and the severity.
//   - ClusterPolicy and Policy: the first Ready condition, when it is False, gives PolicyNotReady with the
//     condition's reason and message as evidence.
//
// Diagnosis rule [RuleAdmissionRefused], on events: a Warning event with reason PolicyViolation from the
// kyverno-admission controller, regarding a ClusterPolicy (in "default") or a Policy (in its own namespace),
// whose note says the request was blocked, gives AdmissionRefused. The event regards the policy, not the
// refused resource, so the finding's subject names the policy. Evidence is the rule's name read from the note,
// the refused resource (the event's related object, or the note's resource part when the related object has
// no valid name), and the event's reason, controller and note. Refusals by the policies.kyverno.io kinds
// regard those kinds and are not matched yet.
//
// Reasons and summaries are written here, never taken from the cluster, and no summary suggests changing,
// excepting or removing a policy. Evidence is untrusted and copied as it is: the evaluator redacts and bounds
// it. Symptoms meet [modules.ValidSymptom].
//
// The module has no actions and no tools: Kyverno guards security, so Shadewright explains its decisions and
// never proposes to relax a policy, add an exception or suppress a finding (design 2.5). A fix, if any, is a
// change to the refused or failing resource in Git. Nothing here writes to the cluster or holds a client.
//
// This package imports only the standard library and this module (ADR-0013): Kyverno objects are read as
// plain JSON maps, so no Kyverno API package is needed.
package kyverno
