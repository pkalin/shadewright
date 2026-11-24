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
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/tools"
)

// containerReader answers get_resource with a pod of one container with the command and arguments given.
type containerReader struct {
	client.Reader
	command, args []string
}

func (r *containerReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("unexpected kind %T", obj)
	}
	*p = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: toolContainer, Image: "db:1", Command: r.command, Args: r.args,
		}}},
	}
	return nil
}

// get_resource shows a pod whose container holds a password flag of #1075, with its command (a tool of the
// table, or "sh -c") or with none: the secret is hidden and the words around it stay. The answer is JSON, each
// item of the command and the arguments on a line of its own.
func TestShortFlagThroughGetResource(t *testing.T) {
	reader := &containerReader{}
	set, err := tools.New(tools.Config{Pods: reader, Events: reader, Objects: reader, Redactor: defaultRedactor(),
		Namespaces: []string{toolNS}})
	if err != nil {
		t.Fatal(err)
	}
	cases := redact.ShortContainers()
	leaked := 0
	for _, c := range cases {
		reader.command, reader.args = c.Command, c.Args
		out := callTool(set, tools.NameGetResource, map[string]any{argKind: toolKind, argNamespace: toolNS, argName: toolPod})
		if strings.Contains(out, c.Secret) {
			leaked++
			if leaked <= 5 {
				t.Errorf("%s: the secret %q is left:\n%s", c.Form, c.Secret, out)
			}
			continue
		}
		for _, k := range c.Kept {
			if !strings.Contains(out, k) {
				t.Errorf("%s: %q is gone:\n%s", c.Form, k, out)
			}
		}
	}
	if leaked > 0 {
		t.Errorf("%d of %d containers leak their secret through get_resource", leaked, len(cases))
	}
}

// podSpecReader answers get_resource with a pod, a Deployment or a StatefulSet whose pod spec is spec.
type podSpecReader struct {
	client.Reader
	spec corev1.PodSpec
}

func (r *podSpecReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	meta := metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}
	switch o := obj.(type) {
	case *corev1.Pod:
		*o = corev1.Pod{ObjectMeta: meta, Spec: r.spec}
	case *appsv1.Deployment:
		*o = appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: r.spec}}}
	case *appsv1.StatefulSet:
		*o = appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: r.spec}}}
	default:
		return fmt.Errorf("unexpected kind %T", obj)
	}
	return nil
}

// get_resource shows a pod, a Deployment and a StatefulSet whose container of a table image (no command) holds
// its tool's password flag in a form the arguments with no command word show: the image decides, the secret is
// hidden, and a container of another image next to it keeps its port (#1075, the image row).
func TestShortFlagImageThroughGetResource(t *testing.T) {
	reader := &podSpecReader{}
	set, err := tools.New(tools.Config{Pods: reader, Events: reader, Objects: reader, Redactor: defaultRedactor(),
		Namespaces: []string{toolNS}})
	if err != nil {
		t.Fatal(err)
	}
	cases := redact.ShortImagePods()
	leaked := 0
	for i, c := range cases {
		kind := []string{toolKind, "Deployment", "StatefulSet"}[i%3]
		reader.spec = c.Spec
		out := callTool(set, tools.NameGetResource, map[string]any{argKind: kind, argNamespace: toolNS, argName: toolPod})
		if strings.Contains(out, c.Secret) {
			leaked++
			if leaked <= 5 {
				t.Errorf("%s (%s): the secret %q is left:\n%s", c.Form, kind, c.Secret, out)
			}
			continue
		}
		for _, k := range c.Kept {
			if !strings.Contains(out, k) {
				t.Errorf("%s (%s): %q is gone:\n%s", c.Form, kind, k, out)
			}
		}
	}
	if leaked > 0 {
		t.Errorf("%d of %d pod specs leak their secret through get_resource", leaked, len(cases))
	}
}
