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
// Rules, at most one symptom per object, read only from the object itself (conditions and reasons of
// kedacore/keda v2.21.0, see [notReadyReasons] for the files):
//
//   - The object must have the watched apiVersion and kind. A paused ScaledObject or ScaledJob (Paused=True)
//     shows nothing: pausing is a person's choice. KEDA sets the same condition for a pause of scale-in or
//     scale-out only, which is hidden too.
//   - Ready=False is reported first, with the KEDA reason when it is one KEDA sets for the kind, and NotReady
//     otherwise. TriggerError means no trigger is active and at least one fails (for a ScaledObject, with no
//     fallback set).
//   - Otherwise, for a ScaledObject, Fallback=True is reported as FallbackExists: a trigger fails and KEDA
//     scales to spec.fallback.replicas instead of on the metric (KEDA does not set TriggerError then).
//   - Otherwise, for a ScaledObject, HPAActive=False is reported as HPAMetricsUnavailable: the HPA cannot
//     read its metrics and does not scale, which KEDA does not reflect in Ready.
//   - Otherwise Ready=Unknown with reason PartialTriggerError is reported: a trigger fails while another one
//     is active.
//   - Active says only whether a trigger is active, which is no failure, and shows nothing; so do Ready=True
//     and Ready=Unknown for any other reason (not reconciled yet). KEDA's conditions carry no
//     observedGeneration, so none is compared.
//   - Evidence is the condition's reason and message, status.triggersTypes (the trigger type names), and,
//     when status.health marks triggers Failing, how many and their highest numberOfFailures.
//   - A replica ceiling is read from the HorizontalPodAutoscaler a ScaledObject controls (KEDA creates one per
//     ScaledObject and sets itself as its controller): ScalingLimited=True with reason TooManyReplicas,
//     status.currentReplicas at or above spec.maxReplicas, status.observedGeneration equal to
//     metadata.generation when both are integers, and no ScalingActive=False. Its symptom ReplicaCeiling
//     carries the ScaledObject's name, spec.maxReplicas, status.currentReplicas, the ScalingLimited reason and,
//     for up to two external metrics, the current value (the backlog the HPA sees) and the target of the
//     matching entry of spec.metrics. An HPA no ScaledObject controls shows nothing.
//
// A trigger error is never answered by raising replicas (design 7): the TriggerError, PartialTriggerError,
// FallbackExists and HPAMetricsUnavailable symptoms carry no replica count and no metric, and no summary says
// what to change. The ceiling symptom is only a candidate: the source sees the HPA, not the owning
// ScaledObject, and a ScaledObject paused for scale-in or scale-out only, or on fallback (with a fallback
// above maxReplicaCount the HPA reports TooManyReplicas while the triggers fail), keeps an HPA that may report
// the limit. A full pause deletes the HPA. The deterministic gate for AdjustKedaMaxReplicas refuses the action
// unless the ScaledObject itself is ready ([KedaReady]), in the policy check and again before every write.
//
// Trigger metadata and TriggerAuthentications are never read: spec.triggers[].metadata may hold addresses,
// queries and credentials, and a TriggerAuthentication names Secrets and keys. status.authenticationsTypes,
// the names people gave their TriggerAuthentications, is left out, and so are the metric names KEDA derives
// from trigger metadata (the keys of status.health, status.externalMetricNames, the HPA's metric names) as
// separate evidence. A condition message is evidence as it is, though: KEDA copies raw scaler and HPA errors
// into messages (ScaledObjectCheckFailed, HPAActive), which can name metrics or quote metadata. Like all
// evidence it is untrusted, and the evaluator redacts and bounds it. A TriggerAuthentication has no
// conditions (its status lists only the ScaledObjects and ScaledJobs that use it), so it is not watched: a
// broken authentication shows as a trigger error of the ScaledObject or ScaledJob that uses it.
//
// The symptom's reason and summary are written here, never taken from the cluster. Symptoms meet
// [modules.ValidSymptom].
//
// The module contributes the KedaReady verification check ([KedaReady]), the AdjustKedaMaxReplicas action and
// no tools. The policy check refuses that action unless the ScaledObject is ready by the same rule
// (internal/kedaready), at the decision and before every write, so whether a trigger error may be answered by
// raising replicas is never left to model output. Nothing here writes to the cluster or holds a client.
//
// This package imports only the standard library and this module (ADR-0013): KEDA and HPA objects are read as
// plain JSON maps, so no KEDA or Kubernetes API package is needed.
package keda
