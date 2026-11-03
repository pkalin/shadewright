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

// Package cloudflared is the cloudflared cluster integration module (ADR-0018, design 2.5): a cloudflared
// container that is not ready becomes a TunnelDown symptom, the reason an application published through a
// Cloudflare Tunnel is unreachable. It diagnoses only: no actions, no tools.
//
// Detection: cloudflared has no API of its own, no CRD and no group version. The module declares the core
// group version v1, the one of the Pods it reads, so it is detected wherever Pods are served, which is every
// cluster: detection does not narrow it, and it is active wherever the cluster admin enables it in
// RemediationSettings, which stays the only gate (ADR-0018 amendment of 2026-11-04). Such a module may only
// diagnose; [modules.New] refuses tools, actions and checks for it ([modules.CoreOnly]). Recognising cloudflared is the job of the symptom source, pod by pod (see below), not of
// detection: detection by workloads would need the detector to read Pods or Deployments, which it does not
// today, and listing Deployments would need a grant the analyzer does not have.
//
// Symptom source, Pods (core v1), read only from the object itself. The module owns the registry's one Pod
// source: a later module that reads Pods must share it. Its reporting controller is the kubelet, which writes
// pod status, the one the collector gives its pod observations too. A container of spec.containers (init and
// ephemeral containers are not read) is cloudflared when its image is:
//
//   - Cloudflare's own image, cloudflare/cloudflared alone or under one of Docker Hub's host names (docker.io,
//     index.docker.io, registry-1.docker.io), with any tag or digest (cloudflared 2026.10.7 .docker-images: the
//     registry docker.io/cloudflare, image cloudflared; Cloudflare's charts default to cloudflare/cloudflared), or
//   - a copy, an image whose repository is cloudflared or ends in /cloudflared, in a pod that carries a label
//     Cloudflare's own manifests set: pod=cloudflared (helm-charts tag cloudflare-tunnel-remote-0.1.2,
//     templates/deployment.yaml) or app.kubernetes.io/name=cloudflare-tunnel (tag cloudflare-tunnel-0.3.2, the
//     chart's default name in its selector labels), or app.kubernetes.io/name=cloudflared, the recommended
//     label with the application's name.
//
// A label alone never makes a pod cloudflared, whatever the image, and a copy that is not Cloudflare's own
// image needs the label. Images, labels and status are untrusted: whoever
// can create a pod in a watched namespace can make it look like cloudflared. A false match only adds a symptom
// about that pod; the module proposes nothing.
//
// A pod that is Pending or Running, not being deleted and whose status is for its current spec (a set
// status.observedGeneration not below metadata.generation, written by the kubelet since Kubernetes 1.35) gives
// one TunnelDown symptom about the first of its cloudflared containers whose containerStatuses entry says
// ready false and that is not waiting with the kubelet's ContainerCreating or PodInitializing (Kubernetes v1.37.2 pkg/kubelet/kubelet_pods.go): those
// are the states before a container first starts, and the events that explain a pod stuck there are collected
// like every Warning event. Ready false means the container is not running (CrashLoopBackOff,
// CreateContainerConfigError for a missing token Secret key, an image pull failure) or that its readiness probe
// fails. cloudflared serves /ready on its metrics address with 200 only while it holds at least one connection
// to Cloudflare's edge and 503 otherwise (cloudflared 2026.10.7 metrics/readiness.go, "Intended for k8s
// readiness checks"). Cloudflare's charts probe /ready as a liveness probe instead, so there a tunnel without
// connections is restarted, and shows as not ready while the kubelet backs off. Known limit: a container without
// any probe on /ready stays ready while it has no connection, and is not seen; the analyzer reads no logs (it
// has no grant on pods/log, and logs are free text).
//
// Evidence is the container's name and image, its restart count, its state (and the waiting or terminated
// reason), the reason and exit code of its last termination (cloudflared exits with 1 on a failed command,
// cmd/cloudflared/cliutil/errors.go WithErrorHandler; the reason Error is the container runtime's, containerd
// v2.4.2 internal/cri/server/helpers.go) and the reason of the pod's Ready condition (ContainersNotReady,
// Kubernetes v1.37.2 pkg/kubelet/status/generate.go). Never read: the command, args and env, which hold the
// tunnel token (--token, TUNNEL_TOKEN), every message (the waiting message, the termination message, which can
// hold the tail of the log, and condition messages) and annotations. Evidence values are untrusted and copied
// as they are: the evaluator redacts and bounds them. The reason and summary are written here. Symptoms meet
// [modules.ValidSymptom].
//
// One symptom is about one replica: a tunnel with another ready replica still serves, which the summary says.
// Known limits, for the wiring of symptom sources (design 2.5): a running container is not ready until its
// first readiness probe passes, so every rollout shows the symptom for a moment, and a source has no clock to
// wait out; a crash-looping container gives this symptom and the collector's termination observation too.
//
// This package imports only the standard library and this module (ADR-0013): Pods are read as plain JSON maps.
package cloudflared
