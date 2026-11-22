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

package redact_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/tools"
)

const (
	toolNS        = "shop"
	toolPod       = "web-abc-1"
	toolContainer = "app"
	toolKind      = "Pod"
	toolReason    = "BackOff"
	argNamespace  = "namespace"
	argPod        = "pod"
	argContainer  = "container"
	argTemplate   = "template"
	argKind       = "kind"
	argName       = "name"
)

// textReader answers every read of the tool set with objects that carry one text: a pod whose container
// argument it is, and one Event whose note it is. With missingKey set the pod also names the text as the key of
// toolSecret in its env, and its container waits on the kubelet's error that the key is not there, so that
// check_missing_reference answers "missing" and shows the text as the key.
type textReader struct {
	client.Reader
	text       string
	missingKey bool
}

func (r *textReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("unexpected kind %T", obj)
	}
	*p = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: toolContainer, Args: []string{r.text}}}},
	}
	if r.missingKey {
		p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: toolSecret}, Key: r.text},
		}}}
		// The kubelet's error (pkg/kubelet/kubelet_pods.go makeEnvironmentVariables), as refcheck compares it.
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: toolContainer, State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError",
				Message: fmt.Sprintf("couldn't find key %v in Secret %v/%v", r.text, key.Namespace, toolSecret)},
		}}}
	}
	return nil
}

func (r *textReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*eventsv1.EventList)
	if !ok {
		return fmt.Errorf("unexpected kind %T", list)
	}
	*l = eventsv1.EventList{Items: []eventsv1.Event{{
		ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: toolNS},
		Regarding:  corev1.ObjectReference{Kind: toolKind, Name: toolPod, Namespace: toolNS},
		Reason:     toolReason, Type: "Warning", Note: r.text,
	}}}
	return nil
}

// readTools is one tool set over a textReader and fake log, metrics and delivery sources; calls are
// serialized, since each sets the text first.
type readTools struct {
	sync.Mutex
	set      *tools.Set
	reader   *textReader
	logs     *fake.Logs
	metrics  *fake.Metrics
	delivery *fake.Delivery
}

// Arguments of the read tools in the entry points.
const (
	toolTemplate   = "container_restarts_total"
	toolDelivery   = "apps"
	toolMetricText = "message"
	toolSecret     = "db-creds"
)

func newReadTools() *readTools {
	reader, logs, metrics, delivery := &textReader{}, &fake.Logs{}, &fake.Metrics{}, &fake.Delivery{}
	set, err := tools.New(tools.Config{
		Pods: reader, Events: reader, Objects: reader, Logs: logs, Metrics: metrics, Delivery: delivery,
		Redactor: defaultRedactor(), Namespaces: []string{toolNS}, DeliveryNamespaces: []string{toolNS},
	})
	if err != nil {
		panic(err)
	}
	return &readTools{set: set, reader: reader, logs: logs, metrics: metrics, delivery: delivery}
}

// toolSeries is the series query_metrics answers with: a label of the pod and the given labels.
func toolSeries(labels map[string]string) []providers.Series {
	l := map[string]string{argContainer: toolContainer}
	maps.Copy(l, labels)
	return []providers.Series{{Labels: l, Samples: []providers.Sample{{Value: 1}}}}
}

// toolText calls one read tool of the model with in as the text the cluster holds, and returns its answer.
// Each entry point has its own tool set, so the entry points can run in parallel.
func toolText(name string, args map[string]any) func(in string) []string {
	readTools := sync.OnceValue(newReadTools)
	return func(in string) []string {
		rt := readTools()
		rt.Lock()
		defer rt.Unlock()
		rt.reader.text = in
		rt.logs.Set(fake.LogKey{Namespace: toolNS, Pod: toolPod, Container: toolContainer}, in)
		rt.metrics.Set(toolTemplate, toolSeries(map[string]string{toolMetricText: in}))
		rt.delivery.Set(providers.DeliveryUnit{Kind: "Kustomization", Namespace: toolNS, Name: toolDelivery},
			providers.DeliveryStatus{AppliedRevision: "main@sha1:0123456789abcdef0123456789abcdef01234567", Message: in})
		return []string{callTool(rt.set, name, args)}
	}
}

// missingKeyText calls check_missing_reference with in as the key the pod's env names in toolSecret and the
// kubelet reports missing, and returns its answer, which must be "missing" with that key as its evidence: an entry
// point whose answer does not show the text checks nothing.
func missingKeyText() func(in string) []string {
	readTools := sync.OnceValue(func() *readTools {
		rt := newReadTools()
		rt.reader.missingKey = true
		return rt
	})
	return func(in string) []string {
		rt := readTools()
		rt.Lock()
		defer rt.Unlock()
		rt.reader.text = in
		text := callTool(rt.set, tools.NameCheckMissingReference,
			map[string]any{argNamespace: toolNS, argPod: toolPod, argKind: "Secret", argName: toolSecret})
		if !strings.HasPrefix(text, "answer: missing\n") || !strings.Contains(text, "\nkey: ") {
			panic(fmt.Sprintf("check_missing_reference did not show the key as missing: %q", text))
		}
		return []string{text}
	}
}

// callTool calls one read tool and returns its answer, which must not be a refusal or a missing source: an
// entry point that reads nothing checks nothing.
func callTool(set *tools.Set, name string, args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	r, err := set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: name, Arguments: raw})
	if err != nil {
		panic(err)
	}
	if r.Status != tools.StatusOK {
		panic(fmt.Sprintf("%s answered %s: %s", name, r.Status, r.Text))
	}
	return r.Text
}

// labelsTools is one tool set whose query_metrics answers with the labels of a metric series input; calls are
// serialized, since each sets the labels first.
var labelsTools = sync.OnceValue(newReadTools)

// queryMetricsOfLabels returns the answer of query_metrics for a series with the given labels.
func queryMetricsOfLabels(labels [][2]string) string {
	rt := labelsTools()
	rt.Lock()
	defer rt.Unlock()
	m := map[string]string{}
	for _, l := range labels {
		m[l[0]] = l[1]
	}
	rt.metrics.Set(toolTemplate, toolSeries(m))
	return callTool(rt.set, tools.NameQueryMetrics, map[string]any{argNamespace: toolNS, argPod: toolPod, argTemplate: toolTemplate})
}
