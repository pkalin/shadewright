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
	"slices"
	"strings"
	"time"

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
// copied from the pod spec, whose names and keys the API server validated, so its size is bounded: at most
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

// EventWindow is how recent the kubelet's FailedMount evidence must be, by its last observation, to show that
// a volume still fails to mount. While a mount fails the kubelet writes two FailedMount Events about the pod:
// the SetUp error, at each retry of the mount (at least every 2m2s, pkg/util/goroutinemap/exponentialbackoff
// maxDurationBeforeRetry), and "Unable to attach or mount volumes: unmounted volumes=[...]" each time its
// wait for the pod's volumes times out (2m3s, pkg/kubelet/volumemanager/volume_manager.go
// podAttachAndMountTimeout, recorded by pkg/kubelet/kubelet.go SyncPod). client-go's recorder spends one spam
// budget on all Warning Events about the same object, whatever their reason or message (tools/record/
// events_cache.go getSpamKey: source, object and type), and once its burst of 25 is gone lets one through
// every 5 minutes (defaultSpamBurst, defaultSpamQPS). So either Event may go unrefreshed for a long time, but
// one of the two is refreshed at least every 5 minutes; 10 minutes leave one spare refill. A SetUp error older
// than the window still counts while a fresh "unmounted volumes" Event lists its volume (fromEvents).
const EventWindow = 10 * time.Minute

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
	// unmountedPrefix starts the note of the kubelet's FailedMount Event when its wait for the pod's volumes
	// times out: pkg/kubelet/kubelet.go SyncPod records "Unable to attach or mount volumes: %v" around
	// pkg/kubelet/volumemanager/volume_manager.go WaitForAttachAndMount's error, "unmounted volumes=%v,
	// unattached volumes=%v, failed to process volumes=%v: %w", where %v of a []string of volume names (DNS
	// labels, so no blanks or brackets) is "[a b]".
	unmountedPrefix = "Unable to attach or mount volumes: unmounted volumes=["
	// The default waiting reasons of a container whose pod the kubelet has not started yet
	// (pkg/kubelet/kubelet_pods.go ContainerCreating and PodInitializing).
	waitingContainerCreating = "ContainerCreating"
	waitingPodInitializing   = "PodInitializing"
)

// subdomainPattern is the API's pattern of an object name, a DNS subdomain of at most 253 bytes. Only the
// asked name, which is untrusted, is checked against it: the names in the pod spec were validated by the API
// server.
var subdomainPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func validName(s string) bool { return len(s) <= 253 && subdomainPattern.MatchString(s) }

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
// Events the analyzer read in the pod's namespace; those about other objects are skipped, and an Event counts
// only while it is fresh ([EventWindow]) and the pod has not got past volume setup. name is untrusted (it may
// come from a model) and is only compared with the names in the pod spec.
func Check(pod *corev1.Pod, events []eventsv1.Event, kind Kind, name string, now time.Time) Result {
	if kind != KindSecret && kind != KindConfigMap {
		return Result{Answer: Unknown}
	}
	unknown := Result{Answer: Unknown, Kind: kind}
	if pod == nil || !validName(name) {
		return unknown
	}
	refs := references(pod, kind, name)
	if len(refs.containers) == 0 && len(refs.volumes) == 0 && !refs.anyOptional {
		return unknown
	}
	unknown.Name, unknown.Referenced = name, true
	texts := notFoundTexts(kind, name)
	if r, ok := fromStatus(pod, kind, name, texts, refs); ok {
		return r
	}
	if r, ok := fromEvents(pod, events, kind, name, texts, refs, now); ok {
		return r
	}
	return unknown
}

// refs are the places the pod spec references one object without marking it optional: containers (by
// name) with the keys their env names in it, and volumes in spec order.
type refs struct {
	containers map[string][]string
	volumes    []volumeRef
	// anyOptional is set when at least one reference is marked optional. The kubelet ignores a missing object
	// there, so such a reference is never evidence; it still makes the object referenced.
	anyOptional bool
}

func references(pod *corev1.Pod, kind Kind, name string) refs {
	r := refs{containers: map[string][]string{}}
	for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for _, c := range list {
			r.addContainer(c, kind, name)
		}
	}
	for _, v := range pod.Spec.Volumes {
		r.addVolume(v, kind, name)
	}
	return r
}

func isOptional(p *bool) bool { return p != nil && *p }

// addContainer notes the env and envFrom references of one container to the object.
func (r *refs) addContainer(c corev1.Container, kind Kind, name string) {
	for _, ef := range c.EnvFrom {
		switch {
		case kind == KindSecret && ef.SecretRef != nil && ef.SecretRef.Name == name:
			r.noteContainer(c.Name, "", isOptional(ef.SecretRef.Optional))
		case kind == KindConfigMap && ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == name:
			r.noteContainer(c.Name, "", isOptional(ef.ConfigMapRef.Optional))
		}
	}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			continue
		}
		switch s, m := e.ValueFrom.SecretKeyRef, e.ValueFrom.ConfigMapKeyRef; {
		case kind == KindSecret && s != nil && s.Name == name:
			r.noteContainer(c.Name, s.Key, isOptional(s.Optional))
		case kind == KindConfigMap && m != nil && m.Name == name:
			r.noteContainer(c.Name, m.Key, isOptional(m.Optional))
		}
	}
}

// addVolume notes a secret, configMap or projected volume that references the object.
func (r *refs) addVolume(v corev1.Volume, kind Kind, name string) {
	switch {
	case kind == KindSecret && v.Secret != nil && v.Secret.SecretName == name:
		r.noteVolume(v.Name, false, isOptional(v.Secret.Optional))
	case kind == KindConfigMap && v.ConfigMap != nil && v.ConfigMap.Name == name:
		r.noteVolume(v.Name, false, isOptional(v.ConfigMap.Optional))
	case v.Projected != nil:
		for _, s := range v.Projected.Sources {
			switch {
			case kind == KindSecret && s.Secret != nil && s.Secret.Name == name:
				r.noteVolume(v.Name, true, isOptional(s.Secret.Optional))
			case kind == KindConfigMap && s.ConfigMap != nil && s.ConfigMap.Name == name:
				r.noteVolume(v.Name, true, isOptional(s.ConfigMap.Optional))
			}
		}
	}
}

func (r *refs) noteContainer(container, key string, optional bool) {
	if optional {
		r.anyOptional = true
		return
	}
	keys := r.containers[container]
	if key != "" {
		keys = append(keys, key)
	}
	r.containers[container] = keys
}

func (r *refs) noteVolume(volume string, projected, optional bool) {
	if optional {
		r.anyOptional = true
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
func fromStatus(pod *corev1.Pod, kind Kind, name string, texts []string, r refs) (Result, bool) {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range list {
			keys, ok := r.containers[cs.Name]
			if !ok || cs.State.Waiting == nil {
				continue
			}
			w := cs.State.Waiting
			if w.Reason != reasonCreateContainerConfig || len(w.Message) > maxMessage {
				continue
			}
			base := Result{Answer: Missing, Kind: kind, Name: name, Referenced: true,
				Source: SourceContainerStatus, Container: cs.Name}
			if slices.Contains(texts, w.Message) {
				base.What = WhatObject
				return base, true
			}
			for _, k := range keys {
				if w.Message == missingKeyText(kind, pod.Namespace, name, k) {
					base.What, base.Key = WhatKey, k
					return base, true
				}
			}
		}
	}
	return Result{}, false
}

// fromEvents looks for the kubelet's FailedMount Events about this pod that show one of the pod's own volumes
// that reference the object failing to mount because the object is not found. Events count only while the
// pod is Pending and no container has got past volume setup ([pastVolumeSetup]): once the volumes are
// mounted, such an Event is old.
//
// For one volume, the evidence is the latest SetUp error Event of that volume (by last observation, so an
// error the kubelet wrote later for another cause wins), when its note is the kubelet's not found error for
// the object, and the volume still fails now: that Event, or an "unmounted volumes" Event that lists the
// volume, was last observed within [EventWindow]. Events with no time at all are skipped.
//
// The SetUp note's shape: pkg/volume/util/operationexecutor/operation_generator.go GenerateMountVolumeFunc
// records VolumeToMount.GenerateError("MountVolume.SetUp failed", err), which operation_executor.go
// generateVolumeMsg and errSuffix write as `MountVolume.SetUp failed for volume "<volume>" : <err>`. For a
// secret or configMap volume err is the manager's NotFound unchanged (pkg/volume/secret/secret.go and
// pkg/volume/configmap/configmap.go SetUpAt); for a projected volume it is apimachinery's errors.NewAggregate
// of the sources' errors (pkg/volume/projected/projected.go collectData), which is the error itself when
// there is one, or `[<err>, <err>]` when there are several different ones.
func fromEvents(pod *corev1.Pod, events []eventsv1.Event, kind Kind, name string, texts []string, r refs,
	now time.Time) (Result, bool) {
	if len(r.volumes) == 0 || pod.Status.Phase != corev1.PodPending || pod.UID == "" || pastVolumeSetup(pod) {
		return Result{}, false
	}
	cutoff := now.Add(-EventWindow)
	type seen struct {
		latest   time.Time // last observation of the latest SetUp error Event of the volume
		missing  bool      // whether that Event is the not found error for the object
		fresh    bool      // whether the volume was reported failing within the window
		setUpPfx string
	}
	vols := make([]seen, len(r.volumes))
	for i, v := range r.volumes {
		vols[i].setUpPfx = fmt.Sprintf("MountVolume.SetUp failed for volume %q : ", v.name)
	}
	for i := range events {
		ev := &events[i]
		t := lastObserved(ev)
		if !aboutPod(ev, pod) || len(ev.Note) > maxMessage || t.IsZero() {
			continue
		}
		note := strings.TrimPrefix(ev.Note, combinedPrefix)
		unmounted, isUnmounted := unmountedVolumes(note)
		for j, v := range r.volumes {
			sv := &vols[j]
			if isUnmounted {
				if !t.Before(cutoff) && slices.Contains(unmounted, v.name) {
					sv.fresh = true
				}
				continue
			}
			rest, ok := strings.CutPrefix(note, sv.setUpPfx)
			if !ok {
				continue
			}
			match := matchesError(rest, texts, v.projected)
			switch {
			case t.After(sv.latest):
				sv.latest, sv.missing = t, match
			case t.Equal(sv.latest) && !match:
				// Two errors seen at the same time: not sure which is current.
				sv.missing = false
			}
			if match && !t.Before(cutoff) {
				sv.fresh = true
			}
		}
	}
	for j, v := range r.volumes {
		if vols[j].missing && vols[j].fresh {
			return Result{Answer: Missing, Kind: kind, Name: name, Referenced: true, What: WhatObject,
				Source: SourceEvent, Volume: v.name}, true
		}
	}
	return Result{}, false
}

// unmountedVolumes returns the volume names listed after "unmounted volumes=" in the kubelet's timeout note,
// and false for any other note. The names are only compared with those of the pod spec.
func unmountedVolumes(note string) ([]string, bool) {
	rest, ok := strings.CutPrefix(note, unmountedPrefix)
	if !ok {
		return nil, false
	}
	list, _, ok := strings.Cut(rest, "]")
	if !ok {
		return nil, false
	}
	return strings.Fields(list), true
}

// pastVolumeSetup reports whether any container or init container of the pod got past volume setup. The
// kubelet waits for all of the pod's volumes to mount (pkg/kubelet/kubelet.go SyncPod,
// volumeManager.WaitForAttachAndMount) before it pulls an image or starts any container
// (containerRuntime.SyncPod), and until then a container shows only the default waiting reason
// (ContainerCreating, or PodInitializing for the containers of a pod with init containers,
// pkg/kubelet/kubelet_pods.go convertToAPIContainerStatuses). So a
// container that runs, ran, or waits for any other reason (ImagePullBackOff, CreateContainerConfigError, ...)
// means the volumes are mounted, and an older FailedMount Event is stale.
func pastVolumeSetup(pod *corev1.Pod) bool {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range list {
			st := cs.State
			if st.Running != nil || st.Terminated != nil || cs.LastTerminationState.Terminated != nil {
				return true
			}
			if w := st.Waiting; w != nil && w.Reason != "" && w.Reason != waitingContainerCreating &&
				w.Reason != waitingPodInitializing {
				return true
			}
		}
	}
	return false
}

// lastObserved is the latest time the Event says it was seen: its series' last observation, its event time,
// or the time the core/v1 API kept (deprecatedLastTimestamp). It is the zero time when it has none.
func lastObserved(ev *eventsv1.Event) time.Time {
	t := ev.EventTime.Time
	if ev.Series != nil && ev.Series.LastObservedTime.After(t) {
		t = ev.Series.LastObservedTime.Time
	}
	if ev.DeprecatedLastTimestamp.After(t) {
		t = ev.DeprecatedLastTimestamp.Time
	}
	return t
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
	if slices.Contains(texts, err) {
		return true
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
		if slices.Contains(texts, part) {
			return true
		}
	}
	return false
}
