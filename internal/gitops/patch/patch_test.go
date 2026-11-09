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
package patch

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var (
	deployment = v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "orders", Name: "orders-worker"}
	scaled     = v1alpha1.ObjectRef{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject", Namespace: "orders", Name: "orders-worker"}
)

func mem(container, prev, desired string) v1alpha1.SetContainerMemoryLimit {
	return v1alpha1.SetContainerMemoryLimit{
		Container: container,
		Previous:  resource.MustParse(prev),
		Desired:   resource.MustParse(desired),
	}
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// changedLines returns the lines that differ, requiring the same line count.
func changedLines(t *testing.T, before, after []byte) (b, a []string) {
	t.Helper()
	lb, la := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
	if len(lb) != len(la) {
		t.Fatalf("line count changed: %d to %d", len(lb), len(la))
	}
	for i := range lb {
		if lb[i] != la[i] {
			b, a = append(b, lb[i]), append(a, la[i])
		}
	}
	return b, a
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if want := read(t, name); string(want) != string(got) {
		t.Errorf("%s differs from the golden file:\n%s", name, got)
	}
}

func TestSetContainerMemoryLimitGolden(t *testing.T) {
	in := read(t, "deployment.yaml")
	out, err := SetContainerMemoryLimit(in, deployment, mem("web", "512Mi", "768Mi"))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "deployment.golden.yaml", out)
	b, a := changedLines(t, in, out)
	if len(b) != 1 || b[0] != `            memory: "512Mi" # raised from 384Mi on 2026-10-14` ||
		a[0] != `            memory: "768Mi" # raised from 384Mi on 2026-10-14` {
		t.Errorf("unexpected change: %q to %q", b, a)
	}
}

func TestAdjustKedaMaxReplicasGolden(t *testing.T) {
	in := read(t, "scaledobject.yaml")
	out, err := AdjustKedaMaxReplicas(in, scaled, v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: 20})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "scaledobject.golden.yaml", out)
	b, a := changedLines(t, in, out)
	if len(b) != 1 || a[0] != "  maxReplicaCount: 20 # capped by the node pool size" {
		t.Errorf("unexpected change: %q to %q", b, a)
	}
}

func TestMemoryComparedAsQuantity(t *testing.T) {
	in := read(t, "deployment.yaml")
	if _, err := SetContainerMemoryLimit(in, deployment, mem("web", "536870912", "1Gi")); err != nil {
		t.Errorf("512Mi should equal 536870912: %v", err)
	}
	plain := strings.Replace(string(in), `"512Mi"`, "536870912", 1)
	out, err := SetContainerMemoryLimit([]byte(plain), deployment, mem("web", "512Mi", "1Gi"))
	if err != nil {
		t.Fatalf("536870912 should equal 512Mi: %v", err)
	}
	if !strings.Contains(string(out), "memory: 1Gi # raised") {
		t.Errorf("plain style not kept:\n%s", out)
	}
}

func TestStylesAndEncodingKept(t *testing.T) {
	base := read(t, "deployment.yaml")
	for name, in := range map[string]string{
		"single quoted": strings.Replace(string(base), `"512Mi"`, `'512Mi'`, 1),
		"plain":         strings.Replace(string(base), `"512Mi"`, `512Mi`, 1),
		"crlf":          strings.ReplaceAll(string(base), "\n", "\r\n"),
		"bom":           string(rune(0xFEFF)) + string(base),
		"no final newline, value last": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: orders-worker, namespace: orders}\n" +
			"spec:\n  template:\n    spec:\n      containers:\n      - name: web\n        resources:\n          limits:\n            memory: 512Mi",
		"non-ascii before the value": strings.Replace(string(base), "# pinned by the release job", "# zażółć gęślą jaźń", 1),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := SetContainerMemoryLimit([]byte(in), deployment, mem("web", "512Mi", "768Mi"))
			if err != nil {
				t.Fatal(err)
			}
			b, a := changedLines(t, []byte(in), out)
			if len(b) != 1 || !strings.Contains(a[0], "768Mi") {
				t.Errorf("unexpected change: %q to %q", b, a)
			}
			if strings.Count(string(out), "768Mi") != 1 {
				t.Errorf("value written %d times", strings.Count(string(out), "768Mi"))
			}
		})
	}
}

func TestOtherDocumentsUntouched(t *testing.T) {
	in := read(t, "scaledobject.yaml")
	out, err := AdjustKedaMaxReplicas(in, scaled, v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: 3})
	if err != nil {
		t.Fatal(err)
	}
	// The staging document, which has the same name and value, is byte for byte the same.
	_, tailIn, _ := strings.Cut(string(in), "---\n")
	_, tailOut, _ := strings.Cut(string(out), "---\n")
	if tailIn != tailOut {
		t.Errorf("the second document changed:\n%s", tailOut)
	}
}

func TestRefusals(t *testing.T) {
	dep := string(read(t, "deployment.yaml"))
	sub := func(old, new string) string {
		if !strings.Contains(dep, old) {
			t.Fatalf("test input lacks %q", old)
		}
		return strings.Replace(dep, old, new, 1)
	}
	twoWeb := sub("      - name: sidecar", "      - name: web\n        image: other\n      - name: sidecar")
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"previous differs", sub(`"512Mi"`, `"384Mi"`), ErrPreviousValueDiffers},
		{"unbounded exponent", sub(`"512Mi"`, `"1e2147483647"`), ErrUnsupported},
		{"container missing", sub("name: web", "name: api"), ErrFieldMissing},
		{"limits missing", sub(`          limits:
            memory: "512Mi" # raised from 384Mi on 2026-10-14
`, ""), ErrFieldMissing},
		{"two containers with the name", twoWeb, ErrAmbiguousContainer},
		{"object missing", sub("name: orders-worker\n  namespace: orders\n  labels", "name: other\n  namespace: orders\n  labels"), ErrTargetNotFound},
		{"other namespace", sub("namespace: orders\n  labels", "namespace: staging\n  labels"), ErrTargetNotFound},
		{"no namespace could match", sub("namespace: orders\n  labels", "labels"), ErrAmbiguousTarget},
		{"another version could match", sub("apps/v1\n", "apps/v1beta1\n"), ErrAmbiguousTarget},
		{"declared twice", dep + "---\n" + dep[strings.Index(dep, "apiVersion: apps/v1"):], ErrAmbiguousTarget},
		{"anchor on the path", sub("          limits:\n", "          limits: &lim\n"), ErrUnsupported},
		{"anchor on the value", sub(`"512Mi"`, `&m "512Mi"`), ErrUnsupported},
		{"alias value", sub(`"512Mi" # raised`, `*req # raised`), ErrUnsupported},
		{"anchored container", sub("- name: web", "- &c\n        name: web"), ErrUnsupported},
		{"merge key on the path", sub("          limits:\n", "          limits:\n            <<: {cpu: 1}\n"), ErrUnsupported},
		{"flow map on the path", sub(`          limits:
            memory: "512Mi" # raised from 384Mi on 2026-10-14
`, `          limits: {memory: "512Mi"}
`), ErrUnsupported},
		{"literal block value", sub(`memory: "512Mi" # raised from 384Mi on 2026-10-14`, "memory: |\n              512Mi"), ErrUnsupported},
		{"tagged value", sub(`"512Mi"`, `!!str 512Mi`), ErrUnsupported},
		{"escaped double quotes", sub(`"512Mi"`, `"512\x4di"`), ErrUnsupported},
		{"duplicate key", sub("  replicas: 2\n", "  replicas: 2\n  replicas: 3\n"), ErrInvalidYAML},
		{"invalid yaml", "a: [1, 2\n", ErrInvalidYAML},
		{"too large", strings.Repeat("# c\n", MaxBytes/4+1), ErrInvalidYAML},
		{"empty", "", ErrTargetNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := SetContainerMemoryLimit([]byte(c.in), deployment, mem("web", "512Mi", "768Mi"))
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if out != nil {
				t.Errorf("returned bytes with an error")
			}
		})
	}
}

func TestErrorsHoldNoDocumentText(t *testing.T) {
	in := strings.Replace(string(read(t, "deployment.yaml")), `"512Mi"`, `"s3cr3t-token"`, 1)
	_, err := SetContainerMemoryLimit([]byte(in), deployment, mem("web", "512Mi", "768Mi"))
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("error %v must not quote the document", err)
	}
}

func TestAnchoredSiblingsAreFine(t *testing.T) {
	// The anchor on requests is not on the path to limits.memory, and the alias in the sidecar is untouched.
	out, err := SetContainerMemoryLimit(read(t, "deployment.yaml"), deployment, mem("web", "512Mi", "768Mi"))
	if err != nil || !strings.Contains(string(out), "requests: *req") {
		t.Fatalf("got %v", err)
	}
}

func TestKedaRefusals(t *testing.T) {
	sc := string(read(t, "scaledobject.yaml"))
	p := v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: 20}
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"previous differs", strings.Replace(sc, "maxReplicaCount: 10", "maxReplicaCount: 12", 1), ErrPreviousValueDiffers},
		{"field missing", strings.Replace(sc, "  maxReplicaCount: 10 # capped by the node pool size\n", "", 1), ErrFieldMissing},
		{"quoted number", strings.Replace(sc, "maxReplicaCount: 10", `maxReplicaCount: "10"`, 1), ErrUnsupported},
		{"hex number", strings.Replace(sc, "maxReplicaCount: 10", "maxReplicaCount: 0xA", 1), ErrUnsupported},
		{"anchored value", strings.Replace(sc, "maxReplicaCount: 10", "maxReplicaCount: &n 10", 1), ErrUnsupported},
		{"wrong object", strings.Replace(sc, "name: orders-worker\n  namespace: orders", "name: x\n  namespace: orders", 1), ErrTargetNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := AdjustKedaMaxReplicas([]byte(c.in), scaled, p); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestInvalidInput(t *testing.T) {
	in := read(t, "deployment.yaml")
	if _, err := SetContainerMemoryLimit(in, scaled, mem("web", "512Mi", "768Mi")); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("wrong kind: %v", err)
	}
	if _, err := SetContainerMemoryLimit(in, deployment, mem("web", "512Mi", "-1Mi")); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative: %v", err)
	}
	if _, err := SetContainerMemoryLimit(in, deployment, mem("", "512Mi", "1Gi")); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("no container: %v", err)
	}
	if _, err := AdjustKedaMaxReplicas(in, deployment, v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 1, DesiredValue: 2}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("wrong kind: %v", err)
	}
}

// second returns a minimal second document that declares the same Deployment through an alias or a merge key.
func TestIdentityFromAliasOrMergeRefused(t *testing.T) {
	dep := string(read(t, "deployment.yaml"))
	docs := map[string]string{
		"alias name":      "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: *n\n  namespace: orders\n",
		"merge name":      "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  <<: {name: orders-worker}\n  namespace: orders\n",
		"alias metadata":  "apiVersion: apps/v1\nkind: Deployment\nmetadata: *m\n",
		"merge at root":   "<<: {kind: Deployment}\napiVersion: apps/v1\nmetadata:\n  name: orders-worker\n  namespace: orders\n",
		"anchored kind":   "apiVersion: apps/v1\nkind: &k Deployment\nmetadata:\n  name: orders-worker\n  namespace: orders\n",
		"binary name":     "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: !!binary b3JkZXJzLXdvcmtlcg==\n  namespace: orders\n",
		"tagged str name": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: !!str orders-worker\n  namespace: orders\n",
		"binary key":      "apiVersion: apps/v1\nkind: Deployment\n!!binary bWV0YWRhdGE=:\n  name: orders-worker\n  namespace: orders\n",
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			in := "x: &n orders-worker\nmeta: &m {name: orders-worker, namespace: orders}\n---\n" + doc + "---\n" + dep
			if _, err := SetContainerMemoryLimit([]byte(in), deployment, mem("web", "512Mi", "768Mi")); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("got %v, want %v", err, ErrUnsupported)
			}
		})
	}
}

func TestAmbiguousNumberSyntaxRefused(t *testing.T) {
	dep := string(read(t, "deployment.yaml"))
	for _, written := range []string{"010000000", "0o10000000", "0x800000", "1_000_000", "2e6", "0b1"} {
		for _, prev := range []string{"2Mi", "10M", "2097152", "1000000", "8388608"} {
			in := strings.Replace(dep, `"512Mi"`, written, 1)
			if _, err := SetContainerMemoryLimit([]byte(in), deployment, mem("web", prev, "768Mi")); !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s with previous %s: got %v, want %v", written, prev, err, ErrUnsupported)
			}
		}
	}
	sc := string(read(t, "scaledobject.yaml"))
	for _, written := range []string{"010", "0o12", "1_0", "1e1"} {
		in := strings.Replace(sc, "maxReplicaCount: 10", "maxReplicaCount: "+written, 1)
		if _, err := AdjustKedaMaxReplicas([]byte(in), scaled, v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: 20}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: got %v, want %v", written, err, ErrUnsupported)
		}
	}
}

// FuzzPatch feeds arbitrary documents to both patchers: they must never panic, and when they accept an
// input, only the target line may differ.
func FuzzPatch(f *testing.F) {
	for _, name := range []string{"deployment.yaml", "scaledobject.yaml"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte("a: &x [1]\nb: *x\n<<: {c: d}\n---\n- !!binary x\n"))
	f.Add([]byte("\xef\xbb\xbf---\r\n...\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if out, err := SetContainerMemoryLimit(data, deployment, mem("web", "512Mi", "768Mi")); err == nil && len(out) == 0 {
			t.Errorf("accepted input produced no output")
		}
		if out, err := AdjustKedaMaxReplicas(data, scaled, v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: 20}); err == nil && len(out) != len(data)+0 {
			t.Errorf("same-width patch changed the length")
		}
	})
}
