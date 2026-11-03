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

package cloudflared

import (
	"slices"
	"strconv"
	"strings"

	"github.com/pkalin/shadewright/internal/modules"
)

// ReasonTunnelDown is the reason of the one symptom the module writes.
const ReasonTunnelDown = "TunnelDown"

// summaryTunnelDown is the fixed summary of the symptom, written here and never taken from the cluster.
const summaryTunnelDown = "A cloudflared container is not ready: it is not running or reports no connection to " +
	"Cloudflare's edge. When no replica of the tunnel is ready, the applications it publishes are unreachable."

// Bounds on what one pod is read for. Kubernetes sets no limit on either, so a forged pod cannot make a source
// do unbounded work.
const (
	// maxContainers is how many entries of spec.containers and status.containerStatuses are read.
	maxContainers = 64
	// maxImageLength is the longest image reference read; a longer one is not cloudflared.
	maxImageLength = 512
)

// Images and labels that mark cloudflared (package documentation for the sources).
const (
	officialRepository       = "cloudflare/cloudflared"
	officialDockerRepository = "docker.io/" + officialRepository
	mirrorSuffix             = "/cloudflared"
	labelPod                 = "pod"
	labelAppName             = "app.kubernetes.io/name"
)

// labelValues are the label values, per key, that let a copy of the image in another registry count as
// cloudflared.
var labelValues = map[string][]string{
	labelPod:     {"cloudflared"},
	labelAppName: {"cloudflared", "cloudflare-tunnel"},
}

// Values Kubernetes writes (Kubernetes v1.37.2): the pod phases that can still serve
// (staging/src/k8s.io/api/core/v1/types.go), the kubelet's waiting reasons before a container first starts
// (pkg/kubelet/kubelet_pods.go) and the pod condition Ready.
const (
	phasePending      = "Pending"
	phaseRunning      = "Running"
	waitingCreating   = "ContainerCreating"
	waitingPodInit    = "PodInitializing"
	conditionPodReady = "Ready"
)

// Evidence fields.
const (
	fieldContainerName     = "spec.containers.name"
	fieldContainerImage    = "spec.containers.image"
	fieldRestartCount      = "status.containerStatuses.restartCount"
	fieldState             = "status.containerStatuses.state"
	fieldWaitingReason     = "status.containerStatuses.state.waiting.reason"
	fieldTerminatedReason  = "status.containerStatuses.state.terminated.reason"
	fieldLastReason        = "status.containerStatuses.lastState.terminated.reason"
	fieldLastExitCode      = "status.containerStatuses.lastState.terminated.exitCode"
	fieldPodReadyCondition = "status.conditions.Ready.reason"
)

// podSymptoms gives one TunnelDown symptom for a Pending or Running pod, not being deleted, whose first
// cloudflared container is not ready and not still being created. Only the image, the labels, the phase, the
// deletion timestamp and the status of the container are read; command, args, env, messages and annotations
// never are.
func podSymptoms(obj map[string]any) []modules.Symptom {
	if av, _ := obj["apiVersion"].(string); av != CoreVersion {
		return nil
	}
	if k, _ := obj["kind"].(string); k != KindPod {
		return nil
	}
	meta, _ := obj["metadata"].(map[string]any)
	if _, deleting := meta["deletionTimestamp"]; deleting {
		return nil
	}
	status, _ := obj["status"].(map[string]any)
	if phase, _ := status["phase"].(string); phase != phasePending && phase != phaseRunning {
		return nil
	}
	labels, _ := meta["labels"].(map[string]any)
	spec, _ := obj["spec"].(map[string]any)
	containers, _ := spec["containers"].([]any)
	statuses, _ := status["containerStatuses"].([]any)
	for i, c := range containers {
		if i == maxContainers {
			break
		}
		container, _ := c.(map[string]any)
		name, _ := container["name"].(string)
		image, _ := container["image"].(string)
		if name == "" || !IsCloudflared(image, labels) {
			continue
		}
		st := containerStatus(statuses, name)
		if st == nil {
			continue
		}
		if ready, ok := st["ready"].(bool); !ok || ready {
			continue
		}
		state, _ := st["state"].(map[string]any)
		waiting, _ := state["waiting"].(map[string]any)
		if reason, _ := waiting["reason"].(string); reason == waitingCreating || reason == waitingPodInit {
			continue
		}
		return []modules.Symptom{tunnelDown(name, image, st, state, status)}
	}
	return nil
}

// tunnelDown builds the symptom of one container: at most 8 evidence entries.
func tunnelDown(name, image string, st, state, status map[string]any) modules.Symptom {
	ev := []modules.Evidence{
		{Field: fieldContainerName, Value: name},
		{Field: fieldContainerImage, Value: image},
	}
	ev = appendInteger(ev, fieldRestartCount, st["restartCount"])
	waiting, _ := state["waiting"].(map[string]any)
	terminated, _ := state["terminated"].(map[string]any)
	switch {
	case waiting != nil:
		ev = append(ev, modules.Evidence{Field: fieldState, Value: "waiting"})
		ev = appendString(ev, fieldWaitingReason, waiting["reason"])
	case terminated != nil:
		ev = append(ev, modules.Evidence{Field: fieldState, Value: "terminated"})
		ev = appendString(ev, fieldTerminatedReason, terminated["reason"])
	case state["running"] != nil:
		ev = append(ev, modules.Evidence{Field: fieldState, Value: "running"})
	}
	last, _ := st["lastState"].(map[string]any)
	lastTerminated, _ := last["terminated"].(map[string]any)
	ev = appendString(ev, fieldLastReason, lastTerminated["reason"])
	ev = appendInteger(ev, fieldLastExitCode, lastTerminated["exitCode"])
	// The kubelet sets a reason on the Ready condition only when it is False (Kubernetes v1.37.2
	// pkg/kubelet/status/generate.go), so the reason is read whatever the status says.
	ev = appendString(ev, fieldPodReadyCondition, modules.FirstCondition(status, conditionPodReady)["reason"])
	return modules.Symptom{Reason: ReasonTunnelDown, Summary: summaryTunnelDown, Evidence: ev}
}

// containerStatus returns the entry of status.containerStatuses for the named container, among the first
// maxContainers, or nil.
func containerStatus(statuses []any, name string) map[string]any {
	for i, s := range statuses {
		if i == maxContainers {
			break
		}
		st, _ := s.(map[string]any)
		if n, _ := st["name"].(string); n == name {
			return st
		}
	}
	return nil
}

// IsCloudflared reports whether a container image, in a pod with the given labels (as decoded from JSON),
// is cloudflared: Cloudflare's own image (cloudflare/cloudflared or docker.io/cloudflare/cloudflared, any tag
// or digest), or an image whose last path element is cloudflared in a pod that carries one of the labels of
// Cloudflare's manifests (pod=cloudflared, app.kubernetes.io/name=cloudflare-tunnel) or
// app.kubernetes.io/name=cloudflared. Labels alone are never enough. Both inputs are untrusted.
func IsCloudflared(image string, labels map[string]any) bool {
	repo, ok := repository(image)
	if !ok {
		return false
	}
	if repo == officialRepository || repo == officialDockerRepository {
		return true
	}
	return strings.HasSuffix(repo, mirrorSuffix) && hasCloudflaredLabel(labels)
}

// repository returns an image reference without its digest and tag, and false for an empty or too long one.
// The tag is the part after the last ':' that follows the last '/', so a registry port stays.
func repository(image string) (string, bool) {
	if image == "" || len(image) > maxImageLength {
		return "", false
	}
	name, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name = name[:i]
	}
	return name, name != ""
}

// hasCloudflaredLabel reports whether the labels hold one of labelValues.
func hasCloudflaredLabel(labels map[string]any) bool {
	for key, values := range labelValues {
		if v, _ := labels[key].(string); slices.Contains(values, v) {
			return true
		}
	}
	return false
}

// appendString adds an evidence entry when v is a non-empty string.
func appendString(ev []modules.Evidence, field string, v any) []modules.Evidence {
	if s, _ := v.(string); s != "" {
		ev = append(ev, modules.Evidence{Field: field, Value: s})
	}
	return ev
}

// appendInteger adds an evidence entry when v is a JSON integer.
func appendInteger(ev []modules.Evidence, field string, v any) []modules.Evidence {
	if n, ok := modules.JSONInteger(v); ok {
		ev = append(ev, modules.Evidence{Field: field, Value: strconv.FormatInt(n, 10)})
	}
	return ev
}
