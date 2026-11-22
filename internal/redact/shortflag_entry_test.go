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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
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
