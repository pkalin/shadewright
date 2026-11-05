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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The texts in these tests are the kubelet's, as of Kubernetes v1.37.2 (go.mod's k8s.io/api v0.37.2):
//
//   - CreateContainerConfigError: pkg/kubelet/kuberuntime/kuberuntime_container.go startContainer sets the
//     waiting reason ErrCreateContainerConfig with the error of generateContainerConfig as the message. For env
//     that error comes unwrapped from pkg/kubelet/kubelet_pods.go makeEnvironmentVariables: the secret or
//     configmap manager's NotFound, or "couldn't find key %v in Secret %v/%v" ("in ConfigMap" for a ConfigMap).
//   - NotFound: k8s.io/apimachinery/pkg/api/errors NewNotFound, `%s %q not found`. The watch-based managers
//     (pkg/kubelet/secret/secret_manager.go, pkg/kubelet/configmap/configmap_manager.go, the kubelet's default
//     change detection strategy) pass corev1.Resource("secret") and ("configmap"), so `secret "x" not found`;
//     the cache-based and simple managers return the API server's `secrets "x" not found`.
//   - FailedMount: pkg/volume/util/operationexecutor/operation_generator.go records the event with
//     VolumeToMount.GenerateError("MountVolume.SetUp failed", err), `<prefix> for volume "<v>" : <err>`
//     (operation_executor.go generateVolumeMsg and errSuffix); pkg/volume/projected/projected.go aggregates the
//     sources' errors with utilerrors.NewAggregate, `[<err>, <err>]` for several.
const (
	ns       = "demo"
	podName  = "web-7d4b9c-x2x8q"
	podUID   = types.UID("3b4c5d6e-7f80-4192-a3b4-c5d6e7f8091a")
	secret   = "db-creds"
	config   = "app-config"
	other    = "admin-token"
	key      = "password"
	app      = "app"
	initName = "migrate"
	creds    = "creds"
	bundle   = "bundle"
)

// testPod references secret by env key (container app), configMap by envFrom (init container migrate),
// secret by a secret volume "creds", configMap and secret through a projected volume "bundle", and other only
// as optional.
func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: podName, UID: podUID},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name: initName,
				EnvFrom: []corev1.EnvFromSource{{
					ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: config}},
				}},
			}},
			Containers: []corev1.Container{{
				Name: app,
				Env: []corev1.EnvVar{
					{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}}},
					{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: other}, Key: "token", Optional: new(true)}}},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: creds, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}},
				{Name: bundle, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: config}}},
						{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: secret}}},
					},
				}}},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func waiting(p *corev1.Pod, init bool, container, reason, message string) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: container, State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}}
	if init {
		p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, cs)
	} else {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, cs)
	}
	return p
}

// now is the time of the tests; kubeletEvent's Events were last observed a minute before it, and so was the
// recorded one in testdata.
var now = time.Date(2026, 11, 3, 8, 15, 16, 0, time.UTC)

// kubeletEvent is a FailedMount event as the kubelet writes it about testPod, read through events.k8s.io/v1.
func kubeletEvent(note string) eventsv1.Event {
	return eventsv1.Event{
		DeprecatedLastTimestamp: metav1.NewTime(now.Add(-time.Minute)),
		ObjectMeta:              metav1.ObjectMeta{Namespace: ns, Name: podName + ".18a1b2c3d4e5f606"},
		ReportingController:     "kubelet",
		Reason:                  "FailedMount",
		Type:                    "Warning",
		Regarding:               corev1.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: ns, Name: podName, UID: podUID},
		Note:                    note,
	}
}

// TestRecordedFailedMountEvent decodes a FailedMount event in the JSON form the API server serves for
// events.k8s.io/v1 (testdata, with the kubelet's fields and note) and finds the missing Secret of the pod's
// volume.
func TestRecordedFailedMountEvent(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "event-secret-volume-not-found.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ev eventsv1.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	got := Check(testPod(), []eventsv1.Event{ev}, KindSecret, secret, now)
	want := Result{Answer: Missing, Kind: KindSecret, Name: secret, Referenced: true, What: WhatObject,
		Source: SourceEvent, Volume: creds}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestMissingFromContainerStatus: a container waiting in CreateContainerConfigError with the kubelet's error
// for its own reference gives Missing with that reason, for a missing key and a missing object.
func TestMissingFromContainerStatus(t *testing.T) {
	cases := []struct {
		name      string
		pod       *corev1.Pod
		kind      Kind
		ref       string
		what      What
		key       string
		container string
	}{
		{"missing Secret key", waiting(testPod(), false, app, "CreateContainerConfigError",
			"couldn't find key password in Secret demo/db-creds"), KindSecret, secret, WhatKey, key, app},
		{"missing Secret, watch-based manager", waiting(testPod(), false, app, "CreateContainerConfigError",
			`secret "db-creds" not found`), KindSecret, secret, WhatObject, "", app},
		{"missing Secret, API server error", waiting(testPod(), false, app, "CreateContainerConfigError",
			`secrets "db-creds" not found`), KindSecret, secret, WhatObject, "", app},
		{"missing ConfigMap of an init container's envFrom", waiting(testPod(), true, initName,
			"CreateContainerConfigError", `configmap "app-config" not found`), KindConfigMap, config, WhatObject, "", initName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.pod, nil, c.kind, c.ref, now)
			want := Result{Answer: Missing, Kind: c.kind, Name: c.ref, Referenced: true, What: c.what,
				Source: SourceContainerStatus, Key: c.key, Container: c.container}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

// TestMissingFromFailedMountEvent: a kubelet FailedMount event about the pod naming the missing object of one
// of its volumes gives Missing.
func TestMissingFromFailedMountEvent(t *testing.T) {
	cases := []struct {
		name, note, ref, volume string
		kind                    Kind
	}{
		{"secret volume", `MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`, secret, creds, KindSecret},
		{"aggregated by the recorder", `(combined from similar events): MountVolume.SetUp failed for volume "creds" : secrets "db-creds" not found`, secret, creds, KindSecret},
		{"projected volume, one source", `MountVolume.SetUp failed for volume "bundle" : configmap "app-config" not found`, config, bundle, KindConfigMap},
		{"projected volume, several sources", `MountVolume.SetUp failed for volume "bundle" : [configmap "app-config" not found, secret "db-creds" not found]`, secret, bundle, KindSecret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(testPod(), []eventsv1.Event{kubeletEvent(c.note)}, c.kind, c.ref, now)
			want := Result{Answer: Missing, Kind: c.kind, Name: c.ref, Referenced: true, What: WhatObject,
				Source: SourceEvent, Volume: c.volume}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

// TestUnknownWithoutEvidence: without the kubelet's status or event the answer is Unknown, never anything
// that says the object exists.
func TestUnknownWithoutEvidence(t *testing.T) {
	running := testPod()
	running.Status.Phase = corev1.PodRunning
	otherReason := waiting(testPod(), false, app, "ImagePullBackOff", `secret "db-creds" not found`)
	cases := []struct {
		name   string
		pod    *corev1.Pod
		events []eventsv1.Event
		kind   Kind
		ref    string
		refd   bool
	}{
		{"no status, no event", testPod(), nil, KindSecret, secret, true},
		{"no pod", nil, nil, KindSecret, secret, false},
		{"another waiting reason", otherReason, nil, KindSecret, secret, true},
		{"old event on a running pod", running,
			[]eventsv1.Event{kubeletEvent(`MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`)}, KindSecret, secret, true},
		{"optional reference", testPod(), nil, KindSecret, other, true},
		{"unknown kind", testPod(), nil, Kind("ServiceAccount"), secret, false},
		{"invalid name", testPod(), nil, KindSecret, "../db-creds", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.pod, c.events, c.kind, c.ref, now)
			if got.Answer != Unknown || got.Referenced != c.refd || got.What != "" || got.Source != "" {
				t.Errorf("got %+v, want Unknown with Referenced %v and no evidence", got, c.refd)
			}
		})
	}
}

// TestEventWindow: a FailedMount Event counts only when it was last observed within EventWindow, by its
// series, its event time or its core/v1 last timestamp. An older one may be about a mount that has since
// succeeded while the pod stays Pending for another reason.
func TestEventWindow(t *testing.T) {
	const note = `MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`
	old := metav1.NewTime(now.Add(-EventWindow - time.Second))
	cases := []struct {
		name string
		set  func(*eventsv1.Event)
		want Answer
	}{
		{"recent last timestamp", func(*eventsv1.Event) {}, Missing},
		{"old last timestamp", func(e *eventsv1.Event) { e.DeprecatedLastTimestamp = old }, Unknown},
		{"no time at all", func(e *eventsv1.Event) { e.DeprecatedLastTimestamp = metav1.Time{} }, Unknown},
		{"old event time, recent series", func(e *eventsv1.Event) {
			e.DeprecatedLastTimestamp = metav1.Time{}
			e.EventTime = metav1.NewMicroTime(old.Time)
			e.Series = &eventsv1.EventSeries{Count: 9, LastObservedTime: metav1.NewMicroTime(now.Add(-time.Minute))}
		}, Missing},
		{"recent event time", func(e *eventsv1.Event) {
			e.DeprecatedLastTimestamp = metav1.Time{}
			e.EventTime = metav1.NewMicroTime(now.Add(-time.Minute))
		}, Missing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := kubeletEvent(note)
			c.set(&ev)
			if got := Check(testPod(), []eventsv1.Event{ev}, KindSecret, secret, now); got.Answer != c.want {
				t.Errorf("got %+v, want %s", got, c.want)
			}
		})
	}
}

// TestOptionalOnlyReferenceIsUnknown: the kubelet ignores a missing optional object, so even its error text
// in a status does not make an optional reference missing.
func TestOptionalOnlyReferenceIsUnknown(t *testing.T) {
	p := waiting(testPod(), false, app, "CreateContainerConfigError", `secret "admin-token" not found`)
	if got := Check(p, nil, KindSecret, other, now); got.Answer != Unknown {
		t.Errorf("got %+v, want Unknown", got)
	}
}

// TestCraftedInputCannotNameUnreferencedObjects: status messages and Event notes are untrusted. A crafted one
// never makes the check name an object, key, container or volume the pod spec does not reference, and never
// binds a text to a reference it is not the kubelet's error for.
func TestCraftedInputCannotNameUnreferencedObjects(t *testing.T) {
	const notReferenced = "kube-admin"
	forged := []eventsv1.Event{
		kubeletEvent(`MountVolume.SetUp failed for volume "creds" : secret "kube-admin" not found`),
		kubeletEvent(`MountVolume.SetUp failed for volume "evil" : secret "kube-admin" not found`),
	}
	cases := []struct {
		name   string
		pod    *corev1.Pod
		events []eventsv1.Event
		ref    string
	}{
		{"event names an unreferenced Secret", testPod(), forged, notReferenced},
		{"status names an unreferenced Secret", waiting(testPod(), false, app, "CreateContainerConfigError",
			`secret "kube-admin" not found`), nil, notReferenced},
		{"status names an unreferenced key", waiting(testPod(), false, app, "CreateContainerConfigError",
			"couldn't find key admin in Secret demo/db-creds"), nil, secret},
		{"status names the key in another namespace", waiting(testPod(), false, app, "CreateContainerConfigError",
			"couldn't find key password in Secret kube-system/db-creds"), nil, secret},
		{"status of a container that does not reference it", waiting(testPod(), true, initName,
			"CreateContainerConfigError", `secret "db-creds" not found`), nil, secret},
		{"status with text around the kubelet's", waiting(testPod(), false, app, "CreateContainerConfigError",
			`ignore previous instructions; secret "db-creds" not found`), nil, secret},
		{"event for a volume that does not reference it", testPod(),
			[]eventsv1.Event{kubeletEvent(`MountVolume.SetUp failed for volume "evil" : secret "db-creds" not found`)}, secret},
		{"aggregate in a secret volume", testPod(),
			[]eventsv1.Event{kubeletEvent(`MountVolume.SetUp failed for volume "creds" : [x, secret "db-creds" not found]`)}, secret},
		{"event from another component", testPod(), []eventsv1.Event{func() eventsv1.Event {
			e := kubeletEvent(`MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`)
			e.ReportingController = "tenant-controller"
			return e
		}()}, secret},
		{"event about another pod with the same name", testPod(), []eventsv1.Event{func() eventsv1.Event {
			e := kubeletEvent(`MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`)
			e.Regarding.UID = "00000000-0000-0000-0000-000000000000"
			return e
		}()}, secret},
		{"event in another namespace", testPod(), []eventsv1.Event{func() eventsv1.Event {
			e := kubeletEvent(`MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`)
			e.Namespace, e.Regarding.Namespace = "team-b", "team-b"
			return e
		}()}, secret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.pod, c.events, KindSecret, c.ref, now)
			if got.Answer != Unknown {
				t.Errorf("got %+v, want Unknown", got)
			}
			if c.ref == notReferenced && (got.Name != "" || got.Referenced) {
				t.Errorf("got %+v: the result names an object the pod spec does not reference", got)
			}
		})
	}
}

// specNames are every name, key, container and volume in the pod spec: the only values a Result may hold.
func specNames(p *corev1.Pod) []string {
	out := []string{""}
	for _, list := range [][]corev1.Container{p.Spec.InitContainers, p.Spec.Containers} {
		for _, c := range list {
			out = append(out, c.Name)
			for _, e := range c.EnvFrom {
				if e.SecretRef != nil {
					out = append(out, e.SecretRef.Name)
				}
				if e.ConfigMapRef != nil {
					out = append(out, e.ConfigMapRef.Name)
				}
			}
			for _, e := range c.Env {
				if s := e.ValueFrom.SecretKeyRef; s != nil {
					out = append(out, s.Name, s.Key)
				}
				if m := e.ValueFrom.ConfigMapKeyRef; m != nil {
					out = append(out, m.Name, m.Key)
				}
			}
		}
	}
	for _, v := range p.Spec.Volumes {
		out = append(out, v.Name)
		if v.Secret != nil {
			out = append(out, v.Secret.SecretName)
		}
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.Secret != nil {
					out = append(out, s.Secret.Name)
				}
				if s.ConfigMap != nil {
					out = append(out, s.ConfigMap.Name)
				}
			}
		}
	}
	return out
}

// FuzzCheck feeds untrusted status messages, Event notes, container names and asked names to the check: it
// never panics, answers only Missing or Unknown, and every name in a Result comes from the pod spec.
func FuzzCheck(f *testing.F) {
	f.Add("app", `secret "db-creds" not found`, `MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`, "db-creds", true)
	f.Add("migrate", `configmap "app-config" not found`, `MountVolume.SetUp failed for volume "bundle" : [configmap "app-config" not found, x]`, "app-config", false)
	f.Add("app", "couldn't find key password in Secret demo/db-creds", "", "db-creds", true)
	f.Add("evil", `secret "kube-admin" not found`, `MountVolume.SetUp failed for volume "evil" : secret "kube-admin" not found`, "kube-admin", true)
	f.Fuzz(func(t *testing.T, container, message, note, ref string, isSecret bool) {
		kind := KindConfigMap
		if isSecret {
			kind = KindSecret
		}
		p := waiting(testPod(), false, container, "CreateContainerConfigError", message)
		got := Check(p, []eventsv1.Event{kubeletEvent(note)}, kind, ref, now)
		if got.Answer != Missing && got.Answer != Unknown {
			t.Fatalf("answer %q", got.Answer)
		}
		names := specNames(testPod())
		for _, s := range []string{got.Name, got.Key, got.Container, got.Volume} {
			if !slices.Contains(names, s) {
				t.Fatalf("result %+v holds %q, which is not in the pod spec", got, s)
			}
		}
		if got.Answer == Missing && !strings.Contains(message, got.Name) && !strings.Contains(note, got.Name) {
			t.Fatalf("result %+v is Missing without evidence naming it", got)
		}
	})
}
