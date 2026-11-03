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

// Package keda is the KEDA cluster integration module (ADR-0018, design 2.5 and 7): failing ScaledObjects and
// ScaledJobs, and ScaledObjects whose HPA runs at its replica ceiling, become symptoms with evidence read from
// their status.
//
// Detection: the cluster serves keda.sh/v1alpha1, the only group version of ScaledObject, ScaledJob and
// TriggerAuthentication in KEDA 2.x (kedacore/keda v2.21.0 apis/keda/v1alpha1/groupversion_info.go).
//
// Rules, at most one symptom per object, read only from the object itself:
//
//   - The object must have the watched apiVersion and kind. A paused ScaledObject or ScaledJob (Paused=True)
//     shows nothing: pausing is a person's choice. KEDA sets the same condition for a pause of scale-in or
//     scale-out only, which is hidden too.
//   - Ready=False is reported first, with the KEDA reason when it is one KEDA sets for the kind (see
//     [notReadyReasons] for the sources and tag), and NotReady otherwise. TriggerError means every trigger
//     fails and no fallback is set.
//   - Otherwise, for a ScaledObject, Fallback=True is reported as FallbackExists: a trigger fails and KEDA
//     scales to spec.fallback.replicas instead of on the metric (KEDA does not set TriggerError then).
//   - Otherwise Ready=Unknown with reason PartialTriggerError is reported: some triggers fail.
//   - Active says only whether a trigger is active, which is no failure, and shows nothing; so do Ready=True
//     and Ready=Unknown for any other reason (not reconciled yet). KEDA's conditions carry no
//     observedGeneration, so none is compared.
//   - Evidence is the condition's reason and message, status.triggersTypes (the trigger type names),
//     status.authenticationsTypes (the names of the TriggerAuthentications the triggers reference), and, when
//     status.health marks triggers Failing, how many and their highest numberOfFailures.
//   - A replica ceiling is read from the HorizontalPodAutoscaler a ScaledObject controls (KEDA creates one per
//     ScaledObject and sets itself as its controller): ScalingLimited=True with reason TooManyReplicas,
//     status.currentReplicas at or above spec.maxReplicas, status.observedGeneration equal to
//     metadata.generation when both are integers, and no ScalingActive=False. Its symptom ReplicaCeiling
//     carries the ScaledObject's name, spec.maxReplicas, status.currentReplicas, the ScalingLimited reason and,
//     for up to two external metrics, the current value (the backlog the HPA sees) and the target of the
//     matching entry of spec.metrics. An HPA no ScaledObject controls shows nothing.
//
// A trigger error is never answered by raising replicas (design 7): the TriggerError, PartialTriggerError and
// FallbackExists symptoms carry no replica count and no metric, and no summary says what to change. The
// ceiling symptom is read from the HPA, which does not show whether the ScaledObject's triggers fail; with a
// fallback above maxReplicaCount, the HPA reports TooManyReplicas while the triggers fail. Whatever proposes a
// capacity change from it (AdjustKedaMaxReplicas, #18) must check that the ScaledObject is Ready and not on
// fallback, for example with [KedaReady].
//
// Trigger metadata and TriggerAuthentications are never read: spec.triggers[].metadata may hold addresses,
// queries and credentials, and a TriggerAuthentication names Secrets and keys. Metric names (the keys of
// status.health, status.externalMetricNames and the HPA's metric names) are derived by KEDA from trigger
// metadata, so they are not evidence either. A TriggerAuthentication has no conditions (its status lists only
// the ScaledObjects and ScaledJobs that use it), so it is not watched: a broken authentication shows as a
// trigger error of the ScaledObject or ScaledJob that uses it.
//
// The symptom's reason and summary are written here, never taken from the cluster. Evidence is untrusted and
// copied as it is: the evaluator redacts and bounds it. Symptoms meet [modules.ValidSymptom].
//
// The module contributes the KedaReady verification check ([KedaReady]) and no tools. It lists no action yet:
// AdjustKedaMaxReplicas comes with its patcher (#18). Nothing here writes to the cluster or holds a client.
//
// This package imports only the standard library and this module (ADR-0013): KEDA and HPA objects are read as
// plain JSON maps, so no KEDA or Kubernetes API package is needed.
package keda
