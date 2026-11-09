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
package semdiff_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/gityaml"
	"github.com/pkalin/shadewright/internal/gitops/patch"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

const (
	soFile  = "apps/orders/scaledobject.yaml"
	soKust  = "apps/orders/kustomization.yaml"
	depFile = "apps/web/deployment.yaml"
	depKust = "apps/web/kustomization.yaml"
	webName = "web"
)

const scaledObjectYAML = `apiVersion: v1
kind: Service
metadata:
  name: orders-worker
  namespace: orders
spec:
  ports:
    - port: 80
---
# The worker scales on queue depth.
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: orders-worker
  namespace: orders
spec:
  scaleTargetRef:
    name: orders-worker
  minReplicaCount: 1
  maxReplicaCount: 10
`

const deploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: web
spec:
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: sidecar
          image: example/sidecar:1.0
        - name: web
          image: example/web:1.0
          resources:
            requests:
              memory: 256Mi
            limits:
              memory: "512Mi" # raised in October
`

func soTarget(name string) v1alpha1.ObjectRef {
	return v1alpha1.ObjectRef{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject", Namespace: "orders", Name: name}
}

func depTarget() v1alpha1.ObjectRef {
	return v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: webName, Name: webName}
}

// replicas changes maxReplicaCount from the 10 the fixtures hold.
func replicas(desired int32) v1alpha1.Action {
	return v1alpha1.Action{
		Type:                  v1alpha1.ActionAdjustKedaMaxReplicas,
		AdjustKedaMaxReplicas: &v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: desired},
	}
}

// memory changes the web container's memory limit from the 512Mi the fixtures hold.
func memory(desired string) v1alpha1.Action {
	return v1alpha1.Action{
		Type: v1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{
			Container: webName, Previous: resource.MustParse("512Mi"), Desired: resource.MustParse(desired),
		},
	}
}

func kust(resources ...string) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n")
	for _, r := range resources {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	return []byte(b.String())
}

// ordersRepo is the plain case the source mapping supports: one kustomization listing one file.
func ordersRepo() map[string][]byte {
	return map[string][]byte{
		soKust:                kust("scaledobject.yaml"),
		soFile:                []byte(scaledObjectYAML),
		"unrelated/README.md": []byte("not part of the build"),
	}
}

func soInput(t *testing.T, files map[string][]byte, path string, desired int32) semdiff.Input {
	t.Helper()
	patched, err := patch.AdjustKedaMaxReplicas(files[soFile], soTarget("orders-worker"),
		*replicas(desired).AdjustKedaMaxReplicas)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	return semdiff.Input{
		Files: files, Path: path, File: soFile, Patched: patched,
		Target: soTarget("orders-worker"), Action: replicas(desired),
	}
}

func wantReason(t *testing.T, err error, want *semdiff.Error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %s", err, want.Reason)
	}
}

func TestCheckAcceptsTheOneChange(t *testing.T) {
	res, err := semdiff.Check(context.Background(), soInput(t, ordersRepo(), "./apps/orders", 15))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := semdiff.Result{Objects: 2, Field: "spec.maxReplicaCount", Previous: "10", Desired: "15"}
	if res != want {
		t.Fatalf("got %+v, want %+v", res, want)
	}
}

func TestCheckAcceptsMemoryThroughABase(t *testing.T) {
	files := map[string][]byte{
		"clusters/lab/kustomization.yaml": kust("../../apps/web"),
		depKust:                           kust("deployment.yaml"),
		depFile:                           []byte(deploymentYAML),
	}
	act := memory("768Mi")
	patched, err := patch.SetContainerMemoryLimit(files[depFile], depTarget(), *act.SetContainerMemoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	res, err := semdiff.Check(context.Background(), semdiff.Input{
		Files: files, Path: "clusters/lab", File: depFile, Patched: patched, Target: depTarget(), Action: act,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Previous != `"512Mi"` || res.Desired != `"768Mi"` || res.Objects != 1 {
		t.Fatalf("unexpected result %+v", res)
	}
}

// sharedBase builds two ScaledObjects, a-orders-worker and b-orders-worker, from one base file.
func sharedBase() map[string][]byte {
	prefixed := func(p string) []byte {
		return fmt.Appendf(nil, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namePrefix: %s-\nresources:\n  - ../base\n", p)
	}
	return map[string][]byte{
		"apps/kustomization.yaml":      kust("a", "b"),
		"apps/a/kustomization.yaml":    prefixed("a"),
		"apps/b/kustomization.yaml":    prefixed("b"),
		"apps/base/kustomization.yaml": kust("scaledobject.yaml"),
		"apps/base/scaledobject.yaml": []byte(strings.ReplaceAll(scaledObjectYAML,
			"apiVersion: v1\nkind: Service", "apiVersion: v1\nkind: ConfigMap")),
	}
}

func TestCheckRefusesAChangeToAnotherObjectThroughASharedBase(t *testing.T) {
	files := sharedBase()
	base := "apps/base/scaledobject.yaml"
	patched := []byte(strings.Replace(string(files[base]), "maxReplicaCount: 10", "maxReplicaCount: 15", 1))
	_, err := semdiff.Check(context.Background(), semdiff.Input{
		Files: files, Path: "apps", File: base, Patched: patched,
		Target: soTarget("a-orders-worker"), Action: replicas(15),
	})
	wantReason(t, err, semdiff.ErrOtherObjectChanged)
}

func TestCheckRefusesAChangeToAnotherDocumentOfTheFile(t *testing.T) {
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	in.Patched = []byte(strings.Replace(string(in.Patched), "port: 80", "port: 81", 1))
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrOtherObjectChanged)
}

func TestCheckRefusesAnotherFieldOfTheTarget(t *testing.T) {
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	in.Patched = []byte(strings.Replace(string(in.Patched), "minReplicaCount: 1", "minReplicaCount: 2", 1))
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrOtherFieldChanged)
}

func TestCheckRefusesAddedOrRemovedObjects(t *testing.T) {
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	in.Patched = append(in.Patched, []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: orders\n")...)
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrObjectsAddedOrRemoved)

	in = soInput(t, ordersRepo(), "apps/orders", 15)
	in.Patched = in.Patched[strings.Index(string(in.Patched), "---"):]
	_, err = semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrObjectsAddedOrRemoved)
}

func TestCheckRefusesAnUnchangedFile(t *testing.T) {
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	in.Patched = in.Files[soFile]
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrDesiredValueMissing)
}

// overlayPatch sets maxReplicaCount through a patch in an overlay, so the file value is not what renders.
func overlayPatch() map[string][]byte {
	files := ordersRepo()
	files["clusters/lab/kustomization.yaml"] = []byte(`resources:
  - ../../apps/orders
patches:
  - path: max.yaml
    target:
      kind: ScaledObject
`)
	files["clusters/lab/max.yaml"] = []byte(`apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: orders-worker
  namespace: orders
spec:
  maxReplicaCount: 10
`)
	return files
}

func TestCheckRefusesWhenAnOverlayOverridesTheValue(t *testing.T) {
	// The overlay pins 10, so the patched file renders 10 again.
	_, err := semdiff.Check(context.Background(), soInput(t, overlayPatch(), "clusters/lab", 15))
	wantReason(t, err, semdiff.ErrDesiredValueMissing)

	// The overlay pins another value than the plan's previous one.
	files := overlayPatch()
	files["clusters/lab/max.yaml"] = []byte(strings.Replace(string(files["clusters/lab/max.yaml"]), "10", "12", 1))
	_, err = semdiff.Check(context.Background(), soInput(t, files, "clusters/lab", 15))
	wantReason(t, err, semdiff.ErrPreviousValueDiffers)
}

func TestCheckRefusesAFileOutsideTheBuild(t *testing.T) {
	files := ordersRepo()
	files[soKust] = kust()
	_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
	wantReason(t, err, semdiff.ErrFileNotInBuild)

	// A file used only as a patch is not a resource either.
	files = overlayPatch()
	in := soInput(t, files, "clusters/lab", 15)
	in.File = "clusters/lab/max.yaml"
	_, err = semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrFileNotInBuild)
}

func TestCheckRefusesSchemaViolations(t *testing.T) {
	files := map[string][]byte{depKust: kust("deployment.yaml"), depFile: []byte(deploymentYAML)}
	act := memory("128Mi") // below the 256Mi request
	patched, err := patch.SetContainerMemoryLimit(files[depFile], depTarget(), *act.SetContainerMemoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	in := semdiff.Input{Files: files, Path: "apps/web", File: depFile, Patched: patched, Target: depTarget(), Action: act}
	_, err = semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrSchemaInvalid)

	// An unknown field in the Deployment fails the strict conversion.
	unknown := strings.Replace(deploymentYAML, "  selector:", "  replicaz: 3\n  selector:", 1)
	files[depFile] = []byte(unknown)
	act = memory("768Mi")
	in.Patched, err = patch.SetContainerMemoryLimit(files[depFile], depTarget(), *act.SetContainerMemoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	in.Action = act
	_, err = semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrSchemaInvalid)

	// maxReplicaCount below minReplicaCount.
	sof := ordersRepo()
	sof[soFile] = []byte(strings.Replace(scaledObjectYAML, "minReplicaCount: 1", "minReplicaCount: 5", 1))
	_, err = semdiff.Check(context.Background(), soInput(t, sof, "apps/orders", 3))
	wantReason(t, err, semdiff.ErrSchemaInvalid)
}

func TestCheckRefusesAnAmbiguousTarget(t *testing.T) {
	files := ordersRepo()
	files[soKust] = kust("scaledobject.yaml", "old.yaml")
	files["apps/orders/old.yaml"] = []byte("apiVersion: keda.sh/v1beta1\nkind: ScaledObject\nmetadata:\n" +
		"  name: orders-worker\n  namespace: orders\nspec:\n  maxReplicaCount: 10\n")
	_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
	wantReason(t, err, semdiff.ErrAmbiguousTarget)
}

func TestCheckRefusesWhenTheTargetIsNotRendered(t *testing.T) {
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	in.Target = soTarget("other")
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrTargetNotFound)
}

func TestCheckRefusesABuildKustomizeRejects(t *testing.T) {
	files := ordersRepo()
	files[soKust] = kust("scaledobject.yaml", "copy.yaml")
	files["apps/orders/copy.yaml"] = files[soFile] // the same objects twice
	_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
	wantReason(t, err, semdiff.ErrRenderFailed)
}

func TestCheckRefusesInvalidInput(t *testing.T) {
	for name, mutate := range map[string]func(*semdiff.Input){
		"absolute path":         func(in *semdiff.Input) { in.Path = "/apps/orders" },
		"path outside":          func(in *semdiff.Input) { in.Path = "../apps" },
		"file is kustomization": func(in *semdiff.Input) { in.File = soKust },
		"file not yaml":         func(in *semdiff.Input) { in.File = "apps/orders/x.json" },
		"no change planned":     func(in *semdiff.Input) { in.Action = replicas(10) },
		"wrong kind":            func(in *semdiff.Input) { in.Target.Kind = "Deployment" },
		"no namespace":          func(in *semdiff.Input) { in.Target.Namespace = "" },
		"patched too large":     func(in *semdiff.Input) { in.Patched = make([]byte, semdiff.MaxFileBytes+1) },
	} {
		t.Run(name, func(t *testing.T) {
			in := soInput(t, ordersRepo(), "apps/orders", 15)
			mutate(&in)
			_, err := semdiff.Check(context.Background(), in)
			var e *semdiff.Error
			if !errors.As(err, &e) || (e.Reason != semdiff.ReasonInvalidInput && e.Reason != semdiff.ReasonTooLarge) {
				t.Fatalf("got %v, want InvalidInput or TooLarge", err)
			}
		})
	}
}

// TestCheckNeverReachesRemoteOrPlugins covers the lock-down: every reference kustomize could read as remote
// and every field that loads plugins, generators or Helm is refused before kustomize runs.
func TestCheckNeverReachesRemoteOrPlugins(t *testing.T) {
	remote := []string{
		"https://github.com/example/fleet//apps?ref=main",
		"http://example.com/so.yaml",
		"github.com/example/fleet/apps",
		"GitHub.com/example/fleet",
		"git@github.com:example/fleet.git",
		"git::https://example.com/fleet",
		"ssh://git@example.com/fleet",
		"file:///etc/fleet",
		"/etc/passwd",
		"../../../outside",
		"base..x",
		"user@host:repo",
		"so.yaml?ref=x",
	}
	for _, ref := range remote {
		for _, field := range []string{"resources", "components"} {
			t.Run(field+" "+ref, func(t *testing.T) {
				files := ordersRepo()
				files[soKust] = kust("scaledobject.yaml", fmt.Sprintf("%q", ref))
				if field == "components" {
					files[soKust] = fmt.Appendf(nil,
						"resources:\n  - scaledobject.yaml\ncomponents:\n  - %q\n", ref)
				}
				_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
				wantReason(t, err, semdiff.ErrInvalidReference)
			})
		}
		t.Run("patch "+ref, func(t *testing.T) {
			files := ordersRepo()
			files[soKust] = fmt.Appendf(nil,
				"resources:\n  - scaledobject.yaml\npatches:\n  - path: %q\n", ref)
			_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
			wantReason(t, err, semdiff.ErrInvalidReference)
		})
	}
	for _, field := range []string{
		"generators: [gen.yaml]", "transformers: [t.yaml]", "validators: [v.yaml]",
		"helmCharts: [{name: x, repo: 'https://example.com'}]", "helmGlobals: {chartHome: x}",
		"configMapGenerator: [{name: x, files: ['https://example.com/x']}]", "secretGenerator: [{name: x}]",
		"openapi: {path: 'https://example.com/schema.json'}", "crds: [crd.yaml]", "configurations: [c.yaml]",
		"replacements: [{path: r.yaml}]", "bases: [../base]", "patchesStrategicMerge: [p.yaml]",
		"patchesJson6902: [{path: p.yaml}]", "buildMetadata: [originAnnotations]",
		"patches: [{path: p.yaml, exec: x}]",
	} {
		t.Run(field, func(t *testing.T) {
			files := ordersRepo()
			files[soKust] = []byte("resources:\n  - scaledobject.yaml\n" + field + "\n")
			_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
			wantReason(t, err, semdiff.ErrUnsupportedKustomization)
		})
	}
}

func TestCheckRefusesBadKustomizationFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string][]byte
		want  *semdiff.Error
	}{
		"no kustomization": {map[string][]byte{soFile: []byte(scaledObjectYAML)}, semdiff.ErrFileMissing},
		"two kustomizations": {map[string][]byte{
			soKust:                      kust("scaledobject.yaml"),
			"apps/orders/Kustomization": kust("scaledobject.yaml"),
			soFile:                      []byte(scaledObjectYAML),
		}, semdiff.ErrFileMissing},
		"missing resource": {map[string][]byte{
			soKust: kust("scaledobject.yaml", "gone.yaml"),
			soFile: []byte(scaledObjectYAML),
		}, semdiff.ErrFileMissing},
		"invalid yaml": {map[string][]byte{
			soKust: []byte("resources: [\n"), soFile: []byte(scaledObjectYAML),
		}, semdiff.ErrInvalidYAML},
		"duplicate key": {map[string][]byte{
			soKust: []byte("resources: []\nresources: [scaledobject.yaml]\n"),
			soFile: []byte(scaledObjectYAML),
		}, semdiff.ErrInvalidYAML},
		"wrong kind": {map[string][]byte{
			soKust: []byte("kind: Component\nresources: [scaledobject.yaml]\n"),
			soFile: []byte(scaledObjectYAML),
		}, semdiff.ErrUnsupportedKustomization},
		"resources not a list": {map[string][]byte{
			soKust: []byte("resources: scaledobject.yaml\n"), soFile: []byte(scaledObjectYAML),
		}, semdiff.ErrUnsupportedKustomization},
		"component without kind": {map[string][]byte{
			soKust:                         []byte("resources: [scaledobject.yaml]\ncomponents: [../comp]\n"),
			"apps/comp/kustomization.yaml": []byte("resources: []\n"),
			soFile:                         []byte(scaledObjectYAML),
		}, semdiff.ErrUnsupportedKustomization},
		"file too large": {map[string][]byte{
			soKust:                 kust("scaledobject.yaml", "big.yaml"),
			"apps/orders/big.yaml": make([]byte, semdiff.MaxFileBytes+1),
			soFile:                 []byte(scaledObjectYAML),
		}, semdiff.ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			in := soInput(t, map[string][]byte{soFile: []byte(scaledObjectYAML)}, "apps/orders", 15)
			in.Files = tc.files
			_, err := semdiff.Check(context.Background(), in)
			wantReason(t, err, tc.want)
		})
	}
}

func TestCheckAcceptsAComponent(t *testing.T) {
	files := ordersRepo()
	files[soKust] = []byte("resources: [scaledobject.yaml]\ncomponents: [../comp]\n")
	files["apps/comp/kustomization.yaml"] = []byte("apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n" +
		"labels:\n  - pairs:\n      team: orders\n")
	if _, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15)); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// fanOut builds a chain of directories where each lists the next one twice under two prefixes, so the
// number of visits doubles with every level while the files stay few.
func fanOut(levels int) map[string][]byte {
	files := map[string][]byte{}
	for i := range levels {
		dir := fmt.Sprintf("l%d", i)
		files[dir+"/kustomization.yaml"] = fmt.Appendf(nil, "resources: [../l%dx, ../l%dy]\n", i+1, i+1)
		for _, s := range []string{"x", "y"} {
			files[fmt.Sprintf("l%d%s/kustomization.yaml", i+1, s)] = fmt.Appendf(nil,
				"namePrefix: %s-\nresources: [../l%d]\n", s, i+1)
		}
	}
	files[fmt.Sprintf("l%d/kustomization.yaml", levels)] = kust("so.yaml")
	files[fmt.Sprintf("l%d/so.yaml", levels)] = []byte(scaledObjectYAML)
	return files
}

func TestCheckBoundsTheBuild(t *testing.T) {
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	in.Files, in.Path, in.File = fanOut(5), "l0", "l5/so.yaml"
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrTooLarge) // nested deeper than MaxDepth

	in.Files, in.File = fanOut(3), "l3/so.yaml"
	files := in.Files
	// Same chain but wider: 3 levels of 8-way fan-out visit 8^3 directories.
	for i := range 3 {
		refs := make([]string, 0, 8)
		for j := range 8 {
			refs = append(refs, fmt.Sprintf("../l%dv%d", i+1, j))
			files[fmt.Sprintf("l%dv%d/kustomization.yaml", i+1, j)] = fmt.Appendf(nil, "namePrefix: v%d-\nresources: [../l%d]\n", j, i+1)
		}
		files[fmt.Sprintf("l%d/kustomization.yaml", i)] = fmt.Appendf(nil, "resources: [%s]\n", strings.Join(refs, ", "))
	}
	_, err = semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), "visits") {
		t.Fatalf("got %v, want the visit limit", err)
	}
}

// The budgets are literals here, not the constants, so the tests pin the bounds a 128 MiB executor can
// render (design 8.2.2, #482): 4 MiB of build input and 1,024 objects.
const (
	buildBudget  = 4 << 20
	objectBudget = 1024
)

// budgetRepo is ordersRepo with filler ConfigMaps (comment padded, at most 1 MiB each) so that one visit of
// apps/orders counts exactly total bytes: the kustomization file and every file it lists.
func budgetRepo(t *testing.T, total int) map[string][]byte {
	t.Helper()
	files := ordersRepo()
	const fillers = 5
	names := make([]string, 0, 1+fillers)
	names = append(names, "scaledobject.yaml")
	for i := range fillers {
		names = append(names, fmt.Sprintf("fill%d.yaml", i))
	}
	files[soKust] = kust(names...)
	left := total - len(files[soKust]) - len(files[soFile])
	for i := range fillers {
		cm := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fill%d\n  namespace: orders\n", i)
		size := left / (fillers - i)
		if size > semdiff.MaxFileBytes || size < len(cm)+3 {
			t.Fatalf("filler of %d bytes does not fit", size)
		}
		files[fmt.Sprintf("apps/orders/fill%d.yaml", i)] = []byte("# " + strings.Repeat("x", size-len(cm)-3) + "\n" + cm)
		left -= size
	}
	return files
}

func TestCheckBuildByteBudget(t *testing.T) {
	// desired 15 keeps the patched file as long as the original; 100 makes it one byte longer, so a tree at
	// exactly the budget is over it once patched.
	cases := map[string]struct {
		total   int
		desired int32
		want    *semdiff.Error
	}{
		"exactly the budget":              {buildBudget, 15, nil},
		"one byte over":                   {buildBudget + 1, 15, semdiff.ErrTooLarge},
		"one byte over once patched":      {buildBudget, 100, semdiff.ErrTooLarge},
		"exactly the budget once patched": {buildBudget - 1, 100, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files := budgetRepo(t, c.total)
			_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", c.desired))
			if c.want == nil {
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				return
			}
			wantReason(t, err, c.want)
			if !strings.Contains(err.Error(), fmt.Sprintf("more than %d bytes", buildBudget)) {
				t.Fatalf("got %v, want the build byte budget named", err)
			}
		})
	}
}

// configMapsYAML returns n small ConfigMaps in one file.
func configMapsYAML(n int) []byte {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm%d\n  namespace: orders\n", i)
	}
	return []byte(b.String())
}

func TestCheckObjectBudget(t *testing.T) {
	cases := map[string]struct {
		objects int
		want    *semdiff.Error
	}{
		"exactly the budget": {objectBudget, nil},
		"one object over":    {objectBudget + 1, semdiff.ErrTooLarge},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files := ordersRepo()
			files[soKust] = kust("scaledobject.yaml", "cms.yaml")
			files["apps/orders/cms.yaml"] = configMapsYAML(c.objects - 2) // the file holds a Service and the ScaledObject
			res, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
			if c.want == nil {
				if err != nil || res.Objects != c.objects {
					t.Fatalf("got %+v, %v, want %d objects", res, err, c.objects)
				}
				return
			}
			wantReason(t, err, c.want)
			if !strings.Contains(err.Error(), fmt.Sprintf("more than %d objects", objectBudget)) {
				t.Fatalf("got %v, want the object budget named", err)
			}
		})
	}
}

// atBudget names the case of a build exactly at a budget.
const atBudget = "exactly the budget"

// documentBudget is semdiff.MaxBuildDocuments as a literal, so the test pins the bound (design 8.2.2, #488).
const documentBudget = 4096

// emptyDocs returns a file of n empty YAML documents: each counts against the document budget, and kustomize
// skips them, so a build at the budget renders at once. The blank lines keep the separators apart, as kustomize
// reads "---\n---\n" as a single one.
func emptyDocs(n int) []byte { return []byte(strings.Repeat("---\n\n", n)) }

// docsRepo is ordersRepo with an extra resource file of empty documents, so that one visit of apps/orders
// holds total documents: the two of scaledobject.yaml and the extra ones.
func docsRepo(total int) map[string][]byte {
	files := ordersRepo()
	files[soKust] = kust("scaledobject.yaml", "empty.yaml")
	files["apps/orders/empty.yaml"] = emptyDocs(total - 2)
	return files
}

func wantDocumentBudget(t *testing.T, err error) {
	t.Helper()
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), fmt.Sprintf("more than %d YAML documents", documentBudget)) {
		t.Fatalf("got %v, want the document budget named", err)
	}
}

func TestCheckDocumentBudget(t *testing.T) {
	cases := map[string]struct {
		files   map[string][]byte
		patched func([]byte) []byte
		over    bool
	}{
		atBudget:            {files: docsRepo(documentBudget)},
		"one document over": {files: docsRepo(documentBudget + 1), over: true},
		"one document over once patched": {files: docsRepo(documentBudget),
			patched: func(b []byte) []byte { return append(b, "---\n"...) }, over: true},
		// kustomize builds a base once per overlay that reaches it, so its documents count once per visit.
		"through two overlays": {files: func() map[string][]byte {
			f := docsRepo(documentBudget/2 + 1)
			f["apps/a/kustomization.yaml"] = []byte("namePrefix: a-\nresources: [../orders]\n")
			f["apps/b/kustomization.yaml"] = []byte("namePrefix: b-\nresources: [../orders]\n")
			f["apps/kustomization.yaml"] = kust("a", "b")
			return f
		}(), over: true},
		// A List counts as its items, which kustomize reads as objects.
		"a List of tiny objects": {files: func() map[string][]byte {
			f := docsRepo(4)
			var b strings.Builder
			b.WriteString("apiVersion: v1\nkind: List\nitems:\n")
			for i := range documentBudget - 1 {
				fmt.Fprintf(&b, "  - {apiVersion: v1, kind: ConfigMap, metadata: {name: c%d, namespace: orders}}\n", i)
			}
			f["apps/orders/empty.yaml"] = []byte(b.String())
			return f
		}(), over: true},
		// An inline patch is read like a patch file.
		"inline patches": {files: func() map[string][]byte {
			f := docsRepo(documentBudget)
			f[soKust] = fmt.Appendf(nil, "resources: [scaledobject.yaml, empty.yaml]\npatches:\n  - patch: %q\n",
				"---\napiVersion: v1\nkind: Service\nmetadata: {name: orders-worker, namespace: orders}\n")
			return f
		}(), over: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := "apps/orders"
			if _, ok := c.files["apps/kustomization.yaml"]; ok {
				path = "apps"
			}
			in := soInput(t, c.files, path, 15)
			if c.patched != nil {
				in.Patched = c.patched(in.Patched)
			}
			res, err := semdiff.Check(context.Background(), in)
			if !c.over {
				if err != nil || res.Objects != 2 {
					t.Fatalf("got %+v, %v, want the build accepted", res, err)
				}
				return
			}
			wantDocumentBudget(t, err)
		})
	}
}

// copyOps returns a JSON patch that adds a value to the orders-worker Service and copies it n times, each copy
// doubling it: 17 copies of a few bytes rendered 10 MB, 25 would reach a gigabyte (#488).
func copyOps(n int) string {
	var b strings.Builder
	b.WriteString("- op: add\n  path: /spec/payload\n  value: {a: x, b: y}\n")
	for i := range n {
		fmt.Fprintf(&b, "- op: copy\n  from: /spec/payload\n  path: /spec/payload/n%d\n", i)
	}
	return b.String()
}

const serviceTarget = "target: {kind: Service, name: orders-worker}"

// A JSON patch copy operation is refused before the render in every form kustomize reads a patch in, since
// copies amplify the output past every bound checked before it.
func TestCheckRefusesJSONPatchCopy(t *testing.T) {
	jsonCopy := `[{"op": "add", "path": "/spec/payload", "value": {"a": "x"}},` +
		`{"\u006fp": "c\u006fpy", "from": "/spec/payload", "path": "/spec/payload/n"}]`
	withPatch := func(entry string, extra map[string]string) func(map[string][]byte) {
		return func(f map[string][]byte) {
			f[soKust] = []byte("resources: [scaledobject.yaml]\npatches:\n  - " + entry + "\n")
			for p, d := range extra {
				f[p] = []byte(d)
			}
		}
	}
	cases := map[string]func(map[string][]byte){
		"an inline YAML patch":          withPatch(fmt.Sprintf("%s\n    patch: %q", serviceTarget, copyOps(17)), nil),
		"an inline JSON patch, escaped": withPatch(fmt.Sprintf("%s\n    patch: %q", serviceTarget, jsonCopy), nil),
		"a quoted op": withPatch(fmt.Sprintf("%s\n    patch: %q", serviceTarget,
			"- {op: add, path: /spec/p, value: 1}\n- {'op': \"copy\", from: /spec/p, path: /spec/q}\n"), nil),
		"a YAML patch file": withPatch(serviceTarget+"\n    path: copy.yaml",
			map[string]string{"apps/orders/copy.yaml": copyOps(3)}),
		"a JSON patch file": withPatch(serviceTarget+"\n    path: copy.json",
			map[string]string{"apps/orders/copy.json": jsonCopy}),
		"a patch file after other documents": withPatch(serviceTarget+"\n    path: copy.yaml",
			map[string]string{"apps/orders/copy.yaml": "---\n# none\n---\n" + copyOps(1)}),
		"a patch in a component": func(f map[string][]byte) {
			f[soKust] = []byte("resources: [scaledobject.yaml]\ncomponents: [../c]\n")
			f["apps/c/kustomization.yaml"] = fmt.Appendf(nil, "apiVersion: kustomize.config.k8s.io/v1alpha1\n"+
				"kind: Component\npatches:\n  - %s\n    patch: %q\n", serviceTarget, copyOps(1))
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			files := ordersRepo()
			edit(files)
			_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
			wantReason(t, err, semdiff.ErrUnsupportedKustomization)
			if !strings.Contains(err.Error(), "copy operations") {
				t.Fatalf("got %v, want copy operations named", err)
			}
		})
	}
	// Other JSON patch operations, and "copy" under a key other than op, still build.
	t.Run("add and move", func(t *testing.T) {
		files := ordersRepo()
		withPatch(fmt.Sprintf("%s\n    patch: %q", serviceTarget,
			"- {op: add, path: /spec/p, value: {mode: copy}}\n- {op: move, from: /spec/p, path: /spec/q}\n"), nil)(files)
		res, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
		if err != nil || res.Objects != 2 {
			t.Fatalf("got %+v, %v, want the build accepted", res, err)
		}
	})
}

// Anchors, aliases and merge keys are refused in every file the build reads, before kustomize expands them:
// an anchor that contains itself made it recurse until the Go stack overflowed, which ends the process, and
// aliases can turn a few bytes into many objects (#488, #492).
func TestCheckRefusesAnchors(t *testing.T) {
	cases := map[string]func(map[string][]byte){
		"an anchor that contains itself": func(f map[string][]byte) {
			f[soKust] = kust("scaledobject.yaml", "loop.yaml")
			f["apps/orders/loop.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: loop, namespace: orders}\n" +
				"data: &d {x: *d}\n")
		},
		"alias keys": func(f map[string][]byte) {
			f[soKust] = kust("scaledobject.yaml", "list.yaml")
			f["apps/orders/list.yaml"] = []byte("x: &kind a\ny: &items b\n*kind : List\n*items :\n" +
				"- {apiVersion: v1, kind: ConfigMap, metadata: {name: c, namespace: orders}}\n")
		},
		"a kustomization file": func(f map[string][]byte) {
			f[soKust] = []byte("r: &r scaledobject.yaml\nresources: [*r]\n")
		},
		// kustomize's split drops the separator line, but the parser of the kustomization file reads it as the
		// first document (#594).
		"a kustomization file with aliases on a separator": func(f map[string][]byte) {
			f[soKust] = []byte("# The orders app.\n--- {resources: [scaledobject.yaml], metadata: {annotations: &a {x: y}, labels: *a}}\n")
		},
		// "---#" starts no comment: the parser reads the line as a key holding what follows.
		"a kustomization file with aliases glued on a separator": func(f map[string][]byte) {
			f[soKust] = []byte("# The orders app.\n---#x: {annotations: &a {x: y}, labels: *a}\nresources: [scaledobject.yaml]\n")
		},
		"a kustomization file past the document budget": func(f map[string][]byte) {
			f[soKust] = append(append(kust("scaledobject.yaml"), emptyDocs(documentBudget+1)...),
				"x: &x 1\ny: *x\n"...)
		},
		"an inline patch": func(f map[string][]byte) {
			f[soKust] = fmt.Appendf(nil, "resources: [scaledobject.yaml]\npatches:\n  - patch: %q\n",
				"apiVersion: v1\nkind: Service\nmetadata: &m {name: orders-worker, namespace: orders}\n")
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			files := ordersRepo()
			edit(files)
			_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
			if strings.HasSuffix(name, "past the document budget") {
				wantReason(t, err, semdiff.ErrTooLarge)
				return
			}
			wantReason(t, err, semdiff.ErrInvalidYAML)
			want := gityaml.ErrAnchor
			if strings.HasSuffix(name, "on a separator") {
				want = gityaml.ErrSeparator
			}
			if !strings.Contains(err.Error(), want.Error()) {
				t.Fatalf("got %v, want %q named", err, want)
			}
		})
	}
}

func TestLoadDocumentBudget(t *testing.T) {
	for name, c := range map[string]struct {
		total int
		over  bool
	}{atBudget: {documentBudget, false}, "one document over": {documentBudget + 1, true}} {
		t.Run(name, func(t *testing.T) {
			git := fake.NewGit("main", docsRepo(c.total))
			_, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
			if !c.over {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			wantDocumentBudget(t, err)
		})
	}
}

func TestLoadReadsTheBuild(t *testing.T) {
	files := overlayPatch()
	git := fake.NewGit("main", files)
	got, err := semdiff.Load(context.Background(), git, head(t, git), "./clusters/lab")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := maps.Clone(files)
	delete(want, "unrelated/README.md")
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d", len(got), len(want))
	}
	for p, d := range want {
		if string(got[p]) != string(d) {
			t.Fatalf("file %s differs", p)
		}
	}
	in := soInput(t, got, "./clusters/lab", 15)
	in.Files[soFile] = files[soFile]
	if _, err := semdiff.Check(context.Background(), in); !errors.Is(err, semdiff.ErrDesiredValueMissing) {
		t.Fatalf("Check of the loaded build: %v", err)
	}
}

func TestLoadRefuses(t *testing.T) {
	ctx := context.Background()
	git := fake.NewGit("main", ordersRepo())
	_, err := semdiff.Load(ctx, git, head(t, git), "../x")
	wantReason(t, err, semdiff.ErrInvalidInput)
	_, err = semdiff.Load(ctx, git, head(t, git), "missing")
	wantReason(t, err, semdiff.ErrFileMissing)

	big := ordersRepo()
	big[soKust] = kust("scaledobject.yaml", "big.yaml")
	big["apps/orders/big.yaml"] = make([]byte, providers.MaxFileBytes+1)
	bigGit := fake.NewGit("main", big)
	_, err = semdiff.Load(ctx, bigGit, head(t, bigGit), "apps/orders")
	wantReason(t, err, semdiff.ErrTooLarge)
}

// limitRecorder is a GitProvider that records the limit of each ReadFile call and the bytes it returns.
type limitRecorder struct {
	providers.GitProvider
	limits []int
	served int
}

func (r *limitRecorder) ReadFile(ctx context.Context, commit, p string, limit int) (providers.File, error) {
	r.limits = append(r.limits, limit)
	f, err := r.GitProvider.ReadFile(ctx, commit, p, limit)
	r.served += len(f.Data)
	return f, err
}

// Load asks for at most what is left of MaxBuildBytes, so a directory of many files near MaxFileBytes is
// refused without downloading past the budget (#298).
func TestLoadDownloadsNothingPastTheBudget(t *testing.T) {
	files := ordersRepo()
	const size = semdiff.MaxFileBytes - 1024
	const bigFiles = 2*semdiff.MaxBuildBytes/size + 1
	names := make([]string, 0, 1+bigFiles)
	names = append(names, "scaledobject.yaml")
	for i := range bigFiles {
		n := fmt.Sprintf("big%d.yaml", i)
		names = append(names, n)
		files["apps/orders/"+n] = []byte("# " + strings.Repeat("x", size-3) + "\n")
	}
	files[soKust] = kust(names...)
	git := fake.NewGit("main", files)
	rec := &limitRecorder{GitProvider: git}
	_, err := semdiff.Load(context.Background(), rec, head(t, git), "apps/orders")
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("got %v, want the byte budget", err)
	}
	if rec.served > semdiff.MaxBuildBytes {
		t.Errorf("downloaded %d bytes, over the budget of %d", rec.served, semdiff.MaxBuildBytes)
	}
	if last := rec.limits[len(rec.limits)-1]; last >= semdiff.MaxFileBytes || last >= size {
		t.Errorf("last read asked with limit %d, want what is left of the budget, under %d", last, size)
	}
}

func TestLoadHidesProviderErrors(t *testing.T) {
	git := fake.NewGit("main", ordersRepo())
	commit := head(t, git)
	git.Fail = func(string) error {
		return &providers.RetryableError{RetryAfter: 3 * time.Second, Err: errors.New("https://token@example.com failed")}
	}
	_, err := semdiff.Load(context.Background(), git, commit, "apps/orders")
	var te *sourcemap.TransientError
	if !errors.Is(err, sourcemap.ErrTransient) || !errors.As(err, &te) || te.RetryAfter != 3*time.Second {
		t.Fatalf("got %v, want a transient error with its delay", err)
	}
	if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "example.com") {
		t.Fatalf("error leaks provider text: %v", err)
	}
}

func head(t *testing.T, git *fake.Git) string {
	t.Helper()
	sha, err := git.BranchHead(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestCheckAcceptsAStatefulSet(t *testing.T) {
	sts := strings.Replace(deploymentYAML, "kind: Deployment", "kind: StatefulSet", 1)
	files := map[string][]byte{depKust: kust("deployment.yaml"), depFile: []byte(sts)}
	target := depTarget()
	target.Kind = "StatefulSet"
	act := memory("1Gi")
	patched, err := patch.SetContainerMemoryLimit(files[depFile], target, *act.SetContainerMemoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	res, err := semdiff.Check(context.Background(), semdiff.Input{
		Files: files, Path: "apps/web", File: depFile, Patched: patched, Target: target, Action: act,
	})
	if err != nil || res.Desired != `"1Gi"` {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestCheckRefusesAScaledObjectWithoutScaleTarget(t *testing.T) {
	files := ordersRepo()
	files[soFile] = []byte(strings.Replace(scaledObjectYAML, "  scaleTargetRef:\n    name: orders-worker\n", "", 1))
	_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
	wantReason(t, err, semdiff.ErrSchemaInvalid)
}

func TestCheckRefusesARemovedField(t *testing.T) {
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	in.Patched = []byte(strings.Replace(string(in.Files[soFile]), "  maxReplicaCount: 10\n", "", 1))
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrDesiredValueMissing)
}

func TestErrorText(t *testing.T) {
	if got := semdiff.ErrTooLarge.Error(); got != "semdiff: TooLarge" {
		t.Fatalf("got %q", got)
	}
}

// diamond builds levels of overlays where each lists the next level three times under three name
// prefixes, over one base file of size bytes. Directories are few, but kustomize builds the base once per
// path.
func diamond(levels, size int) map[string][]byte {
	files := map[string][]byte{}
	for i := range levels {
		files[fmt.Sprintf("d%d/kustomization.yaml", i)] = fmt.Appendf(nil, "resources: [../d%dx, ../d%dy, ../d%dz]\n", i, i, i)
		for _, s := range []string{"x", "y", "z"} {
			files[fmt.Sprintf("d%d%s/kustomization.yaml", i, s)] = fmt.Appendf(nil,
				"namePrefix: %s%d-\nresources: [../d%d]\n", s, i, i+1)
		}
	}
	files[fmt.Sprintf("d%d/kustomization.yaml", levels)] = kust("so.yaml")
	pad := "# " + strings.Repeat("x", size) + "\n"
	files[fmt.Sprintf("d%d/so.yaml", levels)] = []byte(pad + scaledObjectYAML)
	return files
}

func TestCheckBoundsADiamondOfOverlays(t *testing.T) {
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	// 3 levels: 3^3 = 27 paths to a base of almost 1 MiB, within MaxVisits but over MaxBuildBytes.
	in.Files, in.Path, in.File = diamond(3, semdiff.MaxFileBytes-1024), "d0", "d3/so.yaml"
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("got %v, want the byte limit", err)
	}
	// The same diamond over a small base is within every limit and renders 27 copies; the target is
	// one of them, the others change too.
	in.Files = diamond(3, 10)
	in.Target = soTarget("x0-x1-x2-orders-worker")
	in.Patched = []byte(strings.Replace(string(in.Files["d3/so.yaml"]), "maxReplicaCount: 10", "maxReplicaCount: 15", 1))
	_, err = semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrOtherObjectChanged)
}

func TestCheckChecksADirectoryForEveryKindItIsReachedAs(t *testing.T) {
	files := ordersRepo()
	// apps/shared is a valid Kustomization, listed first as a resource and then as a component.
	files[soKust] = []byte("resources: [scaledobject.yaml, ../shared]\ncomponents: [../shared]\n")
	files["apps/shared/kustomization.yaml"] = []byte("resources: []\n")
	_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
	wantReason(t, err, semdiff.ErrUnsupportedKustomization)
}

func TestCheckRefusesAFieldMissingBefore(t *testing.T) {
	files := ordersRepo()
	files[soFile] = []byte(strings.Replace(scaledObjectYAML, "  maxReplicaCount: 10\n", "", 1))
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	in.Files = files
	_, err := semdiff.Check(context.Background(), in)
	wantReason(t, err, semdiff.ErrFieldNotFound)
}

func TestCheckRefusesASourceIgnoreFile(t *testing.T) {
	for _, p := range []string{".sourceignore", "apps/.sourceignore", "apps/orders/.sourceignore", "apps/comp/.sourceignore"} {
		t.Run(p, func(t *testing.T) {
			files := ordersRepo()
			files[soKust] = []byte("resources: [scaledobject.yaml]\ncomponents: [../comp]\n")
			files["apps/comp/kustomization.yaml"] = []byte("apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n")
			files[p] = []byte("*.yaml\n")
			_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
			wantReason(t, err, semdiff.ErrSourceIgnored)
			git := fake.NewGit("main", files)
			_, err = semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
			wantReason(t, err, semdiff.ErrSourceIgnored)
		})
	}
	// A patch file in a directory with its own .sourceignore.
	files := overlayPatch()
	files["clusters/lab/kustomization.yaml"] = []byte(strings.Replace(string(files["clusters/lab/kustomization.yaml"]),
		"path: max.yaml", "path: ../../patches/max.yaml", 1))
	files["patches/max.yaml"] = files["clusters/lab/max.yaml"]
	files["patches/.sourceignore"] = []byte("x\n")
	_, err := semdiff.Check(context.Background(), soInput(t, files, "clusters/lab", 15))
	wantReason(t, err, semdiff.ErrSourceIgnored)
}

func TestCheckRefusesANonYAMLResourceFile(t *testing.T) {
	files := ordersRepo()
	files[soKust] = kust("scaledobject.yaml", "deploy.json")
	files["apps/orders/deploy.json"] = []byte("{}")
	_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
	wantReason(t, err, semdiff.ErrUnsupportedKustomization)
	if !strings.Contains(err.Error(), ".yaml") {
		t.Fatalf("the reason does not say why: %v", err)
	}
	git := fake.NewGit("main", files)
	_, err = semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
	wantReason(t, err, semdiff.ErrUnsupportedKustomization)
}

func TestCheckHonoursTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := soInput(t, ordersRepo(), "apps/orders", 15)
	if _, err := semdiff.Check(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
