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

// Package detection wires platform and integration detection (package modules) into the analyzer (design
// 2.4, 2.5 and 5.4, ADR-0024).
//
// A [Detector] is a controller on the one RemediationSettings object, named default. It runs when the
// analyzer becomes leader, when the settings spec changes (a new generation: its own status writes do not
// wake it), and again every [DefaultInterval] while the settings exist, so a node pool or an add-on that
// appears later is found within that time. A failed run is retried with the controller's backoff. Each run:
//
//  1. lists the nodes (provider ID and label keys only are kept, at most [MaxNodes]; more give generic) and
//     the group versions the discovery API serves, each with a deadline;
//  2. reads the settings from the API server, not the cache, and works out the active modules from
//     spec.modules and that detection with [modules.Registry.Select]. No field of the status is ever read for
//     this or for anything else: it is a report, so a stale or forged status enables nothing;
//  3. writes status.platform, status.integrations, observedGeneration and the conditions UnknownModules (True
//     when spec.modules names a module this binary does not have) and DetectionFailed with an update of the
//     status subresource, read again and retried on a conflict, and only when something changed. The read and
//     the write of steps 2 and 3, retries included, have a deadline of their own.
//
// When detection fails, the active modules are worked out from the current spec and the last detection that
// succeeded, and DetectionFailed is True. With no detection that succeeded yet, only generic is active and the
// report says so (platform active generic, no integrations), so a report from a former leader is not left
// standing. When the settings do not exist, only the generic platform is active, nothing is written and no
// further run is scheduled until they are created. When they cannot be read for another reason, the
// previous selection is kept and the run is retried. Followers, which do not run the controller, keep generic
// only; they run no collector either.
//
// [Detector.Selection] is the current answer, for the analysis context (design 3.3): the rules of the active
// modules run on events and nodes through [modules.Selection.EvaluateEvent] and
// [modules.Selection.EvaluateNode], and [EventOf] and [NodeOf] turn API objects into their input.
//
// Permissions (in package analyzer's markers): list on Nodes cluster-wide, and update on the
// remediationsettings/status subresource, the analyzer's only write outside the operator namespace (AGENTS.md
// invariant 6, ADR-0024). The discovery API needs no grant.
package detection
