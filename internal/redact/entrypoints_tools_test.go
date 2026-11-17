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
	argKind       = "kind"
	argName       = "name"
)

// textReader answers every read of the tool set with objects that carry one text: a pod whose container
// argument it is, and one Event whose note it is.
type textReader struct {
	client.Reader
	text string
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

// readTools is one tool set over a textReader and a fake log source; calls are serialized, since each
// sets the text first.
type readTools struct {
	sync.Mutex
	set    *tools.Set
	reader *textReader
	logs   *fake.Logs
}

func newReadTools() *readTools {
	reader, logs := &textReader{}, &fake.Logs{}
	set, err := tools.New(tools.Config{
		Pods: reader, Events: reader, Objects: reader, Logs: logs,
		Redactor: defaultRedactor(), Namespaces: []string{toolNS},
	})
	if err != nil {
		panic(err)
	}
	return &readTools{set: set, reader: reader, logs: logs}
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
		raw, err := json.Marshal(args)
		if err != nil {
			panic(err)
		}
		r, err := rt.set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: name, Arguments: raw})
		if err != nil {
			panic(err)
		}
		return []string{r.Text}
	}
}
