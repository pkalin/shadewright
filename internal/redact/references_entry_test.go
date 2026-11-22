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
	"bufio"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/redact"
)

// A reference is not a value (#845, seventh decision of 2026-11-18, point 3). get_resource on a container whose
// environment takes a variable from a Secret ({name: DB_PASSWORD, valueFrom: {secretKeyRef: {name:
// db-credentials, key: password}}}) hid the Secret's name and key, which main shows and a "secret not found"
// diagnosis needs, and hid the name of the next variable (AUTH_ISSUER) as if it were the value of the key
// "password". Here every place where a pod refers to a Secret or a ConfigMap (configMapKeyRef, secretKeyRef,
// envFrom, volumes of both kinds, imagePullSecrets) gets a name and a key from referenceKeys, between two other
// variables, and what get_resource shows of each identifier is compared with what main showed
// (testdata/references-main-entry.tsv, written by TestWriteReferencesMainTable in a checkout of main): an
// identifier main showed must show, one main hid must stay hidden (names main hides, #860 covers showing more).
// The table is the over-redaction table of these places: a later change that hides a reference shows up as a
// failing row. The rows of an identifier placed in two fields (fieldBefore, fieldKey) were read field by field at
// the commit the table's header names; the file no longer runs unchanged there (getResourceOf and the field env
// are helpers of later test files).

// referencesMainTable holds, for each place, case and identifier, whether main showed it.
const referencesMainTable = "testdata/references-main-entry.tsv"

// referenceKeys are the keys and the names a reference takes: secret words, a key named like an environment
// variable, a file name, and plain words.
var referenceKeys = []string{fieldsKey, "token", "api-key", "tls.key", refVarBefore, "username", "config.yaml"}

// The identifiers referenceResults places around a reference: the variables before and after it, the variable
// that takes the reference, and a volume's name and path.
const (
	refVarBefore = "DB_PASSWORD"
	refVarAfter  = "AUTH_ISSUER"
	refVar       = "REF_VAR"
	refVolume    = "ref-volume"
	refPath      = "ref-path"
)

// referenceNames are the names of the Secret or ConfigMap a reference names.
var referenceNames = []string{"db-credentials", "app-secrets", "regcred", "api-token", "settings"}

// referencePlace is one place where a pod refers to a Secret or a ConfigMap: fill puts the reference to name and
// key into the container c and the pod spec s; ids are the identifiers it places besides those (a variable's
// name, a volume's name and path), and keyless a place that takes no key. read returns, from the pod spec s and
// the container c as the answer shows them, each identifier fill placed with the value at the field it was put
// in.
type referencePlace struct {
	name    string
	ids     []string
	keyless bool
	fill    func(s *corev1.PodSpec, c *corev1.Container, name, key string)
	read    func(s *corev1.PodSpec, c *corev1.Container, name, key string) []placedID
}

// placedID is an identifier a case placed and the value the answer shows at the field it was placed in.
type placedID struct{ id, got string }

// The fields an identifier placed twice is told apart by in its rows: the variable before the reference, and the
// key of the reference (a key named like that variable).
const (
	fieldBefore = "before"
	fieldKey    = "key"
)

// envRef returns the variable that takes the reference in c, the one between the two others.
func envRef(c *corev1.Container) *corev1.EnvVar {
	if len(c.Env) != 3 || c.Env[1].ValueFrom == nil {
		return &corev1.EnvVar{ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{},
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{}}}
	}
	return &c.Env[1]
}

// firstVolume returns the first volume of s, or an empty one.
func firstVolume(s *corev1.PodSpec) corev1.Volume {
	if len(s.Volumes) == 0 {
		return corev1.Volume{}
	}
	return s.Volumes[0]
}

// firstItem returns the first item of items, or an empty one.
func firstItem(items []corev1.KeyToPath) corev1.KeyToPath {
	if len(items) == 0 {
		return corev1.KeyToPath{}
	}
	return items[0]
}

func objRef(name string) corev1.LocalObjectReference { return corev1.LocalObjectReference{Name: name} }

var referencePlaces = []referencePlace{
	{"env.valueFrom.secretKeyRef", []string{refVar}, false, func(_ *corev1.PodSpec, c *corev1.Container, name, key string) {
		c.Env = append(c.Env, corev1.EnvVar{Name: refVar, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: objRef(name), Key: key}}})
	}, func(_ *corev1.PodSpec, c *corev1.Container, name, key string) []placedID {
		e := envRef(c)
		ref := e.ValueFrom.SecretKeyRef
		if ref == nil {
			ref = &corev1.SecretKeySelector{}
		}
		return []placedID{{refVar, e.Name}, {name, ref.Name}, {key, ref.Key}}
	}},
	{"env.valueFrom.configMapKeyRef", []string{refVar}, false, func(_ *corev1.PodSpec, c *corev1.Container, name, key string) {
		c.Env = append(c.Env, corev1.EnvVar{Name: refVar, ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: objRef(name), Key: key}}})
	}, func(_ *corev1.PodSpec, c *corev1.Container, name, key string) []placedID {
		e := envRef(c)
		ref := e.ValueFrom.ConfigMapKeyRef
		if ref == nil {
			ref = &corev1.ConfigMapKeySelector{}
		}
		return []placedID{{refVar, e.Name}, {name, ref.Name}, {key, ref.Key}}
	}},
	{"envFrom.secretRef", nil, false, func(_ *corev1.PodSpec, c *corev1.Container, name, key string) {
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{Prefix: key, SecretRef: &corev1.SecretEnvSource{LocalObjectReference: objRef(name)}})
	}, func(_ *corev1.PodSpec, c *corev1.Container, name, key string) []placedID {
		var got corev1.EnvFromSource
		if len(c.EnvFrom) > 0 {
			got = c.EnvFrom[0]
		}
		if got.SecretRef == nil {
			got.SecretRef = &corev1.SecretEnvSource{}
		}
		return []placedID{{name, got.SecretRef.Name}, {key, got.Prefix}}
	}},
	{"envFrom.configMapRef", nil, false, func(_ *corev1.PodSpec, c *corev1.Container, name, key string) {
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{Prefix: key, ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: objRef(name)}})
	}, func(_ *corev1.PodSpec, c *corev1.Container, name, key string) []placedID {
		var got corev1.EnvFromSource
		if len(c.EnvFrom) > 0 {
			got = c.EnvFrom[0]
		}
		if got.ConfigMapRef == nil {
			got.ConfigMapRef = &corev1.ConfigMapEnvSource{}
		}
		return []placedID{{name, got.ConfigMapRef.Name}, {key, got.Prefix}}
	}},
	{"volumes.secret", []string{refVolume, refPath}, false, func(s *corev1.PodSpec, _ *corev1.Container, name, key string) {
		s.Volumes = append(s.Volumes, corev1.Volume{Name: refVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: name, Items: []corev1.KeyToPath{{Key: key, Path: refPath}}}}})
	}, func(s *corev1.PodSpec, _ *corev1.Container, name, key string) []placedID {
		v := firstVolume(s)
		if v.Secret == nil {
			v.Secret = &corev1.SecretVolumeSource{}
		}
		item := firstItem(v.Secret.Items)
		return []placedID{{refVolume, v.Name}, {name, v.Secret.SecretName}, {key, item.Key}, {refPath, item.Path}}
	}},
	{"volumes.configMap", []string{refVolume, refPath}, false, func(s *corev1.PodSpec, _ *corev1.Container, name, key string) {
		s.Volumes = append(s.Volumes, corev1.Volume{Name: refVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: objRef(name), Items: []corev1.KeyToPath{{Key: key, Path: refPath}}}}})
	}, func(s *corev1.PodSpec, _ *corev1.Container, name, key string) []placedID {
		v := firstVolume(s)
		if v.ConfigMap == nil {
			v.ConfigMap = &corev1.ConfigMapVolumeSource{}
		}
		item := firstItem(v.ConfigMap.Items)
		return []placedID{{refVolume, v.Name}, {name, v.ConfigMap.Name}, {key, item.Key}, {refPath, item.Path}}
	}},
	{"imagePullSecrets", nil, true, func(s *corev1.PodSpec, _ *corev1.Container, name, _ string) {
		s.ImagePullSecrets = append(s.ImagePullSecrets, objRef(name))
	}, func(s *corev1.PodSpec, _ *corev1.Container, name, _ string) []placedID {
		got := ""
		if len(s.ImagePullSecrets) > 0 {
			got = s.ImagePullSecrets[0].Name
		}
		return []placedID{{name, got}}
	}},
}

// referenceResult is what get_resource showed of one identifier of one case, at the field it was placed in.
type referenceResult struct {
	row   string // place, name, key and identifier, tab-separated
	shown bool
}

// referenceResults calls get_resource on a pod for every place, name and key, with a variable before the
// reference and one after it, and reports for each identifier it placed (the name, the key, the variables
// around it, the volume's name and path) whether the answer, decoded as a pod, shows it at the fields it was
// placed in: a field read by its path, never a search of the text, so that the same identifier in another field
// cannot stand in for it. An identifier placed in two fields (a key named like the variable before the reference)
// has a row for each, its identifier followed by the field in parentheses (fieldBefore, fieldKey).
func referenceResults(t *testing.T) []referenceResult {
	t.Helper()
	e := newFieldEnv(t)
	var out []referenceResult
	for _, p := range referencePlaces {
		keys := referenceKeys
		if p.keyless {
			keys = []string{""}
		}
		for _, name := range referenceNames {
			for _, key := range keys {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fieldsPod, Namespace: fieldsNS}}
				c := corev1.Container{Name: fieldsContainer, Env: []corev1.EnvVar{{Name: refVarBefore, Value: "x"}}}
				p.fill(&pod.Spec, &c, name, key)
				c.Env = append(c.Env, corev1.EnvVar{Name: refVarAfter, Value: "x"})
				pod.Spec.Containers = []corev1.Container{c}
				text := getResourceOf(t, e, pod)
				var got corev1.Pod
				if err := json.Unmarshal([]byte(text), &got); err != nil || len(got.Spec.Containers) != 1 {
					t.Fatalf("%s %s %s: the answer is not one pod with one container (%v): %.300s", p.name, name, key, err, text)
				}
				gc := &got.Spec.Containers[0]
				var before, after string
				if len(gc.Env) > 0 {
					before, after = gc.Env[0].Name, gc.Env[len(gc.Env)-1].Name
				}
				placed := append([]placedID{{refVarBefore, before}, {refVarAfter, after}}, p.read(&got.Spec, gc, name, key)...)
				ids := append([]string{name, key, refVarBefore, refVarAfter}, p.ids...)
				slices.Sort(ids)
				for _, id := range slices.Compact(ids) {
					if id == "" {
						continue
					}
					row := p.name + "\t" + name + "\t" + key + "\t" + id
					if id != refVarBefore || key != refVarBefore {
						i := slices.IndexFunc(placed, func(pl placedID) bool { return pl.id == id })
						out = append(out, referenceResult{row: row, shown: placed[i].got == id})
						continue
					}
					// The variable before the reference is placed first, the key by the place's read.
					var at []string
					for _, pl := range placed {
						if pl.id == id {
							at = append(at, pl.got)
						}
					}
					if len(at) != 2 {
						t.Fatalf("%s: %q placed %d times, want 2", row, id, len(at))
					}
					out = append(out,
						referenceResult{row: row + " (" + fieldBefore + ")", shown: at[0] == id},
						referenceResult{row: row + " (" + fieldKey + ")", shown: at[1] == id})
				}
			}
		}
	}
	return out
}

func TestReferencesShowWhatMainShowed(t *testing.T) {
	redact.ParallelUnderRace(t)
	main := readReferencesMainTable(t)
	results := referenceResults(t)
	over, lost, shown := 0, 0, 0
	for _, r := range results {
		was, ok := main[r.row]
		if !ok {
			t.Errorf("%q: no row in %s (write it again from main)", r.row, referencesMainTable)
			continue
		}
		if r.shown {
			shown++
		}
		switch {
		case was && !r.shown:
			if over++; over <= 20 {
				t.Errorf("%q: hidden, main showed it", r.row)
			}
		case !was && r.shown:
			if lost++; lost <= 20 {
				t.Errorf("%q: shown, main hid it", r.row)
			}
		}
	}
	if len(results) != len(main) {
		t.Errorf("%d results, %d rows in %s", len(results), len(main), referencesMainTable)
	}
	t.Logf("%d identifiers, %d shown; hidden where main showed them %d, shown where main hid them %d", len(results), shown, over, lost)
}

// readReferencesMainTable reads referencesMainTable: whether main showed each row's identifier.
func readReferencesMainTable(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(referencesMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, '\t')
		if i < 0 {
			t.Fatalf("%s: malformed line %q", referencesMainTable, line)
		}
		rows[line[:i]] = line[i+1:] == rowShown
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestWriteReferencesMainTable writes referencesMainTable from the code it runs with, when REFERENCES_MAIN_TABLE
// names the output path (and REFERENCES_MAIN_COMMIT the commit); it is run once in a checkout of main.
func TestWriteReferencesMainTable(t *testing.T) {
	path := os.Getenv("REFERENCES_MAIN_TABLE")
	if path == "" {
		t.Skip("REFERENCES_MAIN_TABLE is not set")
	}
	var b strings.Builder
	b.WriteString("# Whether get_resource showed each identifier of a reference to a Secret or a ConfigMap, at main " +
		os.Getenv("REFERENCES_MAIN_COMMIT") + " (#845), each at the field it was placed in.\n# place\tname\tkey\tidentifier\tshown or hidden\n")
	for _, r := range referenceResults(t) {
		b.WriteString(r.row + "\t" + map[bool]string{true: rowShown, false: rowHidden}[r.shown] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}
