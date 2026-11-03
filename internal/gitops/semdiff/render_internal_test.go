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
package semdiff

import (
	"context"
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/kustomize/api/types"
)

const rootKust = "kustomization.yaml"

func TestOptionsAreLockedDown(t *testing.T) {
	o := options()
	if o.PluginConfig.PluginRestrictions != types.PluginRestrictionsBuiltinsOnly {
		t.Fatalf("plugins are not restricted to built-ins: %v", o.PluginConfig.PluginRestrictions)
	}
	if o.PluginConfig.HelmConfig.Enabled {
		t.Fatal("Helm is enabled")
	}
	if o.PluginConfig.FnpLoadingOptions.EnableExec || o.PluginConfig.FnpLoadingOptions.Network {
		t.Fatal("KRM functions may exec or use the network")
	}
	if o.AddManagedbyLabel {
		t.Fatal("the managed-by label changes the output")
	}
}

// TestRenderRefusesPluginsEvenWithoutTheWalk shows the second layer: kustomize itself, with these options,
// refuses an exec function and a Helm chart that the allow-list would have stopped first.
func TestRenderRefusesPluginsEvenWithoutTheWalk(t *testing.T) {
	for name, files := range map[string]map[string][]byte{
		"exec function": {
			rootKust: []byte("transformers: [fn.yaml]\n"),
			"fn.yaml": []byte(`apiVersion: example.com/v1
kind: Run
metadata:
  name: run
  annotations:
    config.kubernetes.io/function: |
      exec:
        path: /bin/sh
`),
		},
		"helm chart": {
			rootKust: []byte("helmCharts:\n  - name: x\n    repo: https://charts.example.invalid\n"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := render(context.Background(), files, ".")
			if !errors.Is(err, ErrRenderFailed) {
				t.Fatalf("got %v, want RenderFailed", err)
			}
		})
	}
}

// TestRenderWaitHonoursTheContext holds the render slot, as a stuck render would, and checks that another
// caller gives up when its context ends instead of waiting forever.
func TestRenderWaitHonoursTheContext(t *testing.T) {
	renderSlot <- struct{}{}
	defer func() { <-renderSlot }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	files := map[string][]byte{rootKust: []byte("resources: []\n")}
	if _, err := render(ctx, files, "."); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
}
