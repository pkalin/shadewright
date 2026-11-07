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

// Package collector watches events, pods and the objects of module symptom sources in the namespaces the
// analyzer is started with (--watch-namespaces) and turns them into observations for the Incident controller
// (design 3.1, 3.2).
//
// What it watches: events.k8s.io/v1 Events of type Warning, and Pods with an abnormal container
// termination (OOMKilled or a non-zero exit code). OOMKilled comes from container status, never from an
// event reason, so the pod watch is how it is seen. There is no core/v1 Event watch: those are the same
// objects in an older version, and watching both would count each event twice. Both watches use the
// manager's cache, which the analyzer restricts to the watched namespaces. A termination OOMKilled and a
// symptom state of a module's source are marked Immediate, so their Incident opens at the first sighting;
// every other symptom, events included whatever their reason, waits to be seen again, unless its total
// already shows a repeat (package incidents).
//
// Observation-level deduplication: per event the collector remembers the UID and the last handled
// occurrence count (the series count, or the count of an older recorder, or one), and emits only the
// increase, at most MaxCount per observation. Per pod it remembers the IDs of handled terminations. Both
// are LRU maps with a fixed capacity. When the collector cannot tell how much of an occurrence is new
// (anything older than the first reconcile, a restart or a new leader, or as old as state that was dropped
// to stay within the capacity), it asks a HandledFunc from the Incident controller how much of that
// occurrence the Incident already counted; without one it emits the observation with Count 0, the
// cluster's Total and MayRepeat, and the consumer decides. Restarts are thus absorbed by state the Incident
// controller persists per occurrence, not by memory alone.
//
// Owner correlation follows controller owner references from Pod to ReplicaSet to Deployment and from Pod
// to StatefulSet. Owner references are untrusted: each hop is a get through the API reader, with a
// deadline, and must exist with the referenced UID and a selector that matches the labels of the object
// below it, or the chain stops at the last verified object. Kinds the analyzer cannot read (DaemonSets,
// Jobs) are not followed. Results are kept in a bounded cache, verified owners for OwnerTTL and the others
// for 5 seconds, and the owner reference of each pod looked up is remembered (up to MaxPods), so an event
// that arrives after its pod was deleted still reaches the workload.
//
// Untrusted input: notes go through package fingerprint, which redacts before it normalizes, and only the
// fingerprint and the normalized symptom leave the collector; termination messages are never read.
// Reasons, references and timestamps are checked against the Incident schema and the collector's clock.
// Observations go to a bounded channel; when it stays full, the reconcile retries later without
// committing its state, so nothing is lost or counted twice within one process.
//
// Its markers generate the ClusterRole analyzer-watch-role (config/rbac/analyzer/watch). That role is only a
// catalogue of verbs: it is bound by one RoleBinding per watched namespace (config/watch), never by a
// ClusterRoleBinding, so the analyzer can read nothing outside the namespaces it was given. Only namespaced
// reads belong here; anything cluster-scoped or written goes in package analyzer. Workloads are only read
// one at a time by name, so they get get and nothing else. get on pods/log is for the pod logs source of the
// context builder (internal/providers/logs/kube, wired in cmd/analyzer), not for the collector, which reads no
// logs; it is here because this package's markers make the one watch role.
//
// Module symptom sources (sources.go, design 2.5.1 and 3.1): when it is given the active modules
// (Config.Modules), the collector also watches each kind in SourceKinds and runs on each object the symptom
// source that an active module declares for the kind, through modules.EvaluateSource, which drops a symptom
// ValidSymptom refuses and redacts and bounds every evidence value. The add-ons' own kinds (KEDA, Flux,
// Kyverno) are watched lazily (lazy.go): only while a module that reads them is active, the API server serves
// them and the analyzer may list them, a grant the chart gives per module (package modulerbac); a kind that is
// not served or not granted is reported (Collector.SourcesNotWatched, the gauge
// shadewright_analyzer_symptom_source_watched and a log line), and starts no informer. The watches of the
// built-in kinds (autoscaling/v2 HorizontalPodAutoscalers and core Pods) run whenever Config.Modules is set, so the cache
// holds the HPAs of the watched namespaces even while no module uses them (Pods it holds anyway, read as typed
// objects from the pod collector's informer); a module that is not enabled and detected has no active source,
// so its source does not run and its owner kind is not read: no request reaches keda.sh while the KEDA module
// is not active. Only creates and updates the active source shows a symptom on, before or after the change,
// are reconciled; when a module becomes active for a kind (checked every Config.SelectionPoll), the cached
// objects its source shows a symptom on are enqueued. A symptom is about the owner kind the source declares
// (the ScaledObject of a KEDA HPA) once that owner is verified: the object's controller reference names it, a
// get with a deadline finds it with the referenced UID, and the module's Claims accepts it (KEDA's
// status.hpaName); while Claims says the claim is pending, nothing is recorded and the owner is read again
// with a bounded backoff; otherwise the symptom is about the object itself. A Pod's symptom is about its
// verified workload, as for the collector's container terminations. Each symptom state is one occurrence
// ("condition:"), identified by the object, its kind, the reason, when the state began as reported and, for a
// state the collector saw end and come back with the same start, when it came back, so a lasting state counts
// once and a state that comes back counts again; the fingerprint hashes the module's fixed summary, not the
// evidence, whose values change while the state lasts. A state of any kind but the HPA is emitted only once
// the collector has seen it last SettleAfter (2 minutes, the sightings gate's rule), so one that ends sooner,
// such as a cloudflared replica not ready for the seconds of a rollout, opens nothing.
//
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=replicasets;deployments;statefulsets,verbs=get
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
package collector
