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

package refcheck

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
)

// Kind is the kind of the referenced object.
type Kind string

// The kinds the check knows.
const (
	KindSecret    Kind = "Secret"
	KindConfigMap Kind = "ConfigMap"
)

// Answer is the result of the check. There is no answer for "exists" (package doc).
type Answer string

// The answers.
const (
	// Missing: the kubelet reported the object, or a key of it, as not found.
	Missing Answer = "missing"
	// Unknown: no evidence either way.
	Unknown Answer = "unknown"
)

// What says what the kubelet did not find.
type What string

// The things that can be missing.
const (
	// WhatObject: the Secret or ConfigMap itself.
	WhatObject What = "object"
	// WhatKey: a key the container's env names, in an object that exists.
	WhatKey What = "key"
)

// Source says where the evidence was read. Both are the kubelet's fixed reasons.
type Source string

// The sources of evidence.
const (
	// SourceContainerStatus: a container status waiting with reason CreateContainerConfigError.
	SourceContainerStatus Source = "CreateContainerConfigError"
	// SourceEvent: a Warning FailedMount Event about the pod.
	SourceEvent Source = "FailedMount"
)

// Result is the answer for one reference. Every string in it is either a constant of this package or was
// copied from the pod spec after it was checked against its API pattern, so its size is bounded: at most
// four names of 253 bytes. It never holds text from a status message or an Event.
type Result struct {
	Answer Answer
	Kind   Kind
	// Name is the object's name when the pod spec references it, "" otherwise.
	Name string
	// Referenced says whether the pod spec references the object (env, envFrom or a volume).
	Referenced bool
	// The fields below are set only when Answer is Missing.
	What   What
	Source Source
	// Key is the key the container's env names, when What is WhatKey.
	Key string
	// Container is the container whose status is the evidence (SourceContainerStatus).
	Container string
	// Volume is the pod volume the Event names (SourceEvent).
	Volume string
}

// maxMessage is the longest status message or Event note the check compares, in bytes. The texts it looks
// for are far shorter: a longer message is never one of them, and is not scanned.
const maxMessage = 4096

// Fixed kubelet texts, as of Kubernetes v1.37.2 (go.mod's k8s.io/api v0.37.2).
const (
	// reasonCreateContainerConfig is ErrCreateContainerConfig in pkg/kubelet/kuberuntime/kuberuntime_container.go,
	// the waiting reason of a container whose config could not be generated.
	reasonCreateContainerConfig = "CreateContainerConfigError"
	// eventReasonFailedMount is kevents.FailedMountVolume (pkg/kubelet/events/event.go).
	eventReasonFailedMount = "FailedMount"
	eventTypeWarning       = "Warning"
	// reportingKubelet is the kubelet recorder's component (pkg/kubelet/server/server.go ComponentKubelet),
	// which client-go's tools/record writes as reportingController.
	reportingKubelet = "kubelet"
	kindPod          = "Pod"
	// combinedPrefix is what client-go's tools/record/events_cache.go puts before an aggregated message.
	combinedPrefix = "(combined from similar events): "
)

var (
	// The API's patterns: an object name is a DNS subdomain of at most 253 bytes, a container or volume name a
	// DNS label of at most 63, a ConfigMap or Secret key [-._a-zA-Z0-9]+ of at most 253.
	subdomainPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	labelPattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	keyPattern       = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

func validName(s string) bool  { return len(s) <= 253 && subdomainPattern.MatchString(s) }
func validLabel(s string) bool { return len(s) <= 63 && labelPattern.MatchString(s) }
func validKey(s string) bool   { return len(s) <= 253 && keyPattern.MatchString(s) }

// notFoundTexts are the kubelet's errors for a missing object, built for one name from the spec.
//
// apimachinery's errors.NewNotFound writes `<resource> "<name>" not found`. The kubelet's default watch-based
// managers (pkg/kubelet/secret/secret_manager.go NewWatchingSecretManager and
// pkg/kubelet/configmap/configmap_manager.go, corev1.Resource("secret") and ("configmap"), through
// pkg/kubelet/util/manager/watch_based_manager.go Get) name the resource in the singular; the cache-based and
// simple managers return the API server's error, which uses the plural.
func notFoundTexts(kind Kind, name string) []string {
	singular, plural := "secret", "secrets"
	if kind == KindConfigMap {
		singular, plural = "configmap", "configmaps"
	}
	return []string{fmt.Sprintf("%s %q not found", singular, name), fmt.Sprintf("%s %q not found", plural, name)}
}

// missingKeyText is the kubelet's error for a key the env names that is not in the object:
// pkg/kubelet/kubelet_pods.go makeEnvironmentVariables, "couldn't find key %v in ConfigMap %v/%v" and
// "couldn't find key %v in Secret %v/%v" with the pod's namespace.
func missingKeyText(kind Kind, namespace, name, key string) string {
	return fmt.Sprintf("couldn't find key %v in %s %v/%v", key, kind, namespace, name)
}

// Check answers whether the object of kind and name, as the pod references it, is missing. events are the
// Events the analyzer read in the pod's namespace; those about other objects are skipped. name is untrusted
// (it may come from a model) and is only compared with the names in the pod spec.
func Check(pod *corev1.Pod, events []eventsv1.Event, kind Kind, name string) Result {
	if kind != KindSecret && kind != KindConfigMap {
		return Result{Answer: Unknown}
	}
	unknown := Result{Answer: Unknown, Kind: kind}
	if pod == nil || !validName(name) || !validLabel(pod.Namespace) {
		return unknown
	}
	refs := references(pod, kind, name)
	if len(refs.containers) == 0 && len(refs.volumes) == 0 && !refs.optionalOnly {
		return unknown
	}
	unknown.Name, unknown.Referenced = name, true
	if r, ok := fromStatus(pod, kind, name, refs); ok {
		return r
	}
	if r, ok := fromEvents(pod, events, kind, name, refs); ok {
		return r
	}
	return unknown
}

// refs are the places the pod spec references one object without marking it optional: containers (by
// name) with the keys their env names in it, and volumes in spec order.
type refs struct {
	containers map[string][]string
	volumes    []volumeRef
	// optionalOnly is set when the object is referenced, but only as optional.
	optionalOnly bool
}

func references(pod *corev1.Pod, kind Kind, name string) refs {
	r := refs{containers: map[string][]string{}}
	opt := func(p *bool) bool { return p != nil && *p }
	note := func(c, key string, optional bool) {
		if optional {
			r.optionalOnly = true
			return
		}
		keys := r.containers[c]
		if key != "" {
			keys = append(keys, key)
		}
		r.containers[c] = keys
	}
	for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for _, c := range list {
			for _, ef := range c.EnvFrom {
				switch {
				case kind == KindSecret && ef.SecretRef != nil && ef.SecretRef.Name == name:
					note(c.Name, "", opt(ef.SecretRef.Optional))
				case kind == KindConfigMap && ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == name:
					note(c.Name, "", opt(ef.ConfigMapRef.Optional))
				}
			}
			for _, e := range c.Env {
				if e.ValueFrom == nil {
					continue
				}
				switch s, m := e.ValueFrom.SecretKeyRef, e.ValueFrom.ConfigMapKeyRef; {
				case kind == KindSecret && s != nil && s.Name == name:
					note(c.Name, s.Key, opt(s.Optional))
				case kind == KindConfigMap && m != nil && m.Name == name:
					note(c.Name, m.Key, opt(m.Optional))
				}
			}
		}
	}
	for _, v := range pod.Spec.Volumes {
		switch {
		case kind == KindSecret && v.Secret != nil && v.Secret.SecretName == name:
			r.noteVolume(v.Name, false, opt(v.Secret.Optional))
		case kind == KindConfigMap && v.ConfigMap != nil && v.ConfigMap.Name == name:
			r.noteVolume(v.Name, false, opt(v.ConfigMap.Optional))
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				switch {
				case kind == KindSecret && s.Secret != nil && s.Secret.Name == name:
					r.noteVolume(v.Name, true, opt(s.Secret.Optional))
				case kind == KindConfigMap && s.ConfigMap != nil && s.ConfigMap.Name == name:
					r.noteVolume(v.Name, true, opt(s.ConfigMap.Optional))
				}
			}
		}
	}
	return r
}

func (r *refs) noteVolume(volume string, projected, optional bool) {
	if optional {
		r.optionalOnly = true
		return
	}
	r.volumes = append(r.volumes, volumeRef{name: volume, projected: projected})
}

// volumeRef is a pod volume that references the object, and whether it is a projected volume.
type volumeRef struct {
	name      string
	projected bool
}

// fromStatus looks for a container waiting with CreateContainerConfigError whose message is the kubelet's
// error for one of that container's own references.
func fromStatus(pod *corev1.Pod, kind Kind, name string, r refs) (Result, bool) {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range list {
			keys, ok := r.containers[cs.Name]
			if !ok || !validLabel(cs.Name) || cs.State.Waiting == nil {
				continue
			}
			w := cs.State.Waiting
			if w.Reason != reasonCreateContainerConfig || len(w.Message) > maxMessage {
				continue
			}
			base := Result{Answer: Missing, Kind: kind, Name: name, Referenced: true,
				Source: SourceContainerStatus, Container: cs.Name}
			for _, t := range notFoundTexts(kind, name) {
				if w.Message == t {
					base.What = WhatObject
					return base, true
				}
			}
			for _, k := range keys {
				if validKey(k) && w.Message == missingKeyText(kind, pod.Namespace, name, k) {
					base.What, base.Key = WhatKey, k
					return base, true
				}
			}
		}
	}
	return Result{}, false
}

// fromEvents looks for a kubelet FailedMount Event about this pod whose note is the kubelet's error for one
// of the pod's own volumes that reference the object. Only a Pending pod is checked: a volume that failed to
// mount keeps the pod Pending, so on a running pod such an Event is old.
//
// The note's shape: pkg/volume/util/operationexecutor/operation_generator.go GenerateMountVolumeFunc records
// VolumeToMount.GenerateError("MountVolume.SetUp failed", err), which operation_executor.go generateVolumeMsg
// and errSuffix write as `MountVolume.SetUp failed for volume "<volume>" : <err>`. For a secret or configMap
// volume err is the manager's NotFound unchanged (pkg/volume/secret/secret.go and
// pkg/volume/configmap/configmap.go SetUpAt); for a projected volume it is
// apimachinery's errors.NewAggregate of the sources' errors (pkg/volume/projected/projected.go collectData),
// which is the error itself when there is one, or `[<err>, <err>]` when there are several different ones.
func fromEvents(pod *corev1.Pod, events []eventsv1.Event, kind Kind, name string, r refs) (Result, bool) {
	if len(r.volumes) == 0 || pod.Status.Phase != corev1.PodPending || pod.UID == "" {
		return Result{}, false
	}
	texts := notFoundTexts(kind, name)
	for i := range events {
		ev := &events[i]
		if !aboutPod(ev, pod) || len(ev.Note) > maxMessage {
			continue
		}
		note := strings.TrimPrefix(ev.Note, combinedPrefix)
		for _, v := range r.volumes {
			if !validLabel(v.name) {
				continue
			}
			rest, ok := strings.CutPrefix(note, fmt.Sprintf("MountVolume.SetUp failed for volume %q : ", v.name))
			if !ok || !matchesError(rest, texts, v.projected) {
				continue
			}
			return Result{Answer: Missing, Kind: kind, Name: name, Referenced: true, What: WhatObject,
				Source: SourceEvent, Volume: v.name}, true
		}
	}
	return Result{}, false
}

// aboutPod reports whether ev is a kubelet Warning FailedMount Event about pod, by namespace, name and UID.
func aboutPod(ev *eventsv1.Event, pod *corev1.Pod) bool {
	g := ev.Regarding
	return ev.Reason == eventReasonFailedMount && ev.Type == eventTypeWarning &&
		ev.ReportingController == reportingKubelet && ev.Namespace == pod.Namespace &&
		g.Kind == kindPod && (g.APIVersion == "v1" || g.APIVersion == "") &&
		g.Namespace == pod.Namespace && g.Name == pod.Name && g.UID == pod.UID
}

// matchesError reports whether err is one of texts, or, for a projected volume, an aggregate that lists one.
func matchesError(err string, texts []string, projected bool) bool {
	for _, t := range texts {
		if err == t {
			return true
		}
	}
	if !projected {
		return false
	}
	inner, ok := strings.CutPrefix(err, "[")
	if !ok {
		return false
	}
	if inner, ok = strings.CutSuffix(inner, "]"); !ok {
		return false
	}
	for part := range strings.SplitSeq(inner, ", ") {
		for _, t := range texts {
			if part == t {
				return true
			}
		}
	}
	return false
}
