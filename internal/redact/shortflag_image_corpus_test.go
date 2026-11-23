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

package redact

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// The image row of #1075 (decision of 2026-11-22, second): a container read as an object with "args" and no
// "command" takes the base name of its "image" in the same object (no registry host and port, path, tag or
// digest) as its command word when that name is in the table. The corpus is built with its answer known: each
// tool of the table as an image, its password flag in the arguments in a form the arguments with no command word
// leave shown (glued and shaped like a long flag name, or a number from 1 to 65535), in a pod, its init or
// ephemeral containers, next to a container of another image, or in the template of a workload, written as JSON
// (image before "args"), indented JSON, YAML (image after "args") and by hand both ways.

// shortImageRow is an image base name and the row of the table it names: every tool by its own name, and the
// image "redis" for redis-cli, as the decision gives it.
type shortImageRow struct {
	image string
	tool  shortTool
}

func shortImageRows() []shortImageRow {
	out := make([]shortImageRow, 0, len(shortTools)+1)
	for _, t := range shortTools {
		out = append(out, shortImageRow{image: t.name, tool: t})
		if t.name == "redis-cli" {
			out = append(out, shortImageRow{image: shortRedis, tool: t})
		}
	}
	return out
}

// shortDigest is a digest of letters and digits in which no number of the corpus can appear.
var shortDigest = "sha256:" + strings.Repeat("ab", 32)

// shortImageRefs write an image base name as a reference: bare, with a path, a tag, a registry with a port, and a
// digest.
var shortImageRefs = []func(name string) string{
	func(n string) string { return n },
	func(n string) string { return "bitnami/" + n },
	func(n string) string { return n + ":8.4" },
	func(n string) string { return "registry.example:5000/library/" + n + ":8.4@" + shortDigest },
	func(n string) string { return "ghcr.io/acme/tools/" + n + "@" + shortDigest },
}

// shortAppImage and shortAppArgs are a container of an image not in the table whose "-p" is a port: its words stay
// whatever container stands next to it.
const (
	shortAppName  = "app"
	shortOneDB    = "one container"
	shortRedis    = "redis"
	shortAppImage = "example.com/team/web:2"
	shortAppPort  = "8080"
)

var shortAppArgs = []string{"-p", shortAppPort, shortLogLevel, shortDebug}

// shortImageSecret returns the value the arguments with no command word show for the flag form f of t: a word of
// lower-case letters and digits glued to the flag (it reads as a flag of a longer name, "-phunter2"), else a
// number from 10000 to 65535 (it reads as a port); for curl the number after "admin:", and for a flag of an
// upper-case letter, which never reads as a longer name, the number glued too.
func shortImageSecret(rng *rand.Rand, t shortTool, f shortArgForm) string {
	if f.name == "glued" && !t.colon && t.letter >= 'a' {
		const lower = "abcdefghjkmnpqrstuvwxyz23456789"
		var b strings.Builder
		b.WriteString("sw")
		for range 8 + rng.IntN(3) {
			b.WriteByte(lower[rng.IntN(len(lower))])
		}
		return b.String()
	}
	return fmt.Sprint(10000 + rng.IntN(55536))
}

// shortImageCase is one input of the image corpus: the text, the secret that must be hidden and the words that
// must stay.
type shortImageCase struct {
	form   string
	in     string
	secret string
	kept   []string
}

// shortPlacement puts the container of the tool (db) and, for some, a container of another image (app) into an
// object: a pod, or the pod template of a workload.
type shortPlacement struct {
	name string
	// hand: the placement has the containers of a pod only, which the hand-written layouts can write.
	hand   bool
	object func(db, app corev1.Container) any
}

func metav1Type(apiVersion, kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: apiVersion, Kind: kind}
}

func shortPod(spec corev1.PodSpec) any {
	return &corev1.Pod{TypeMeta: metav1Type("v1", "Pod"), Spec: spec}
}

func shortTemplate(spec corev1.PodSpec) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: spec}
}

var shortPlacements = []shortPlacement{
	{name: shortOneDB, hand: true, object: func(db, _ corev1.Container) any {
		return shortPod(corev1.PodSpec{Containers: []corev1.Container{db}})
	}},
	{name: "before another image", hand: true, object: func(db, app corev1.Container) any {
		return shortPod(corev1.PodSpec{Containers: []corev1.Container{db, app}})
	}},
	{name: "after another image", hand: true, object: func(db, app corev1.Container) any {
		return shortPod(corev1.PodSpec{Containers: []corev1.Container{app, db}})
	}},
	{name: "init container", object: func(db, app corev1.Container) any {
		return shortPod(corev1.PodSpec{InitContainers: []corev1.Container{db}, Containers: []corev1.Container{app}})
	}},
	{name: "ephemeral container", object: func(db, app corev1.Container) any {
		return shortPod(corev1.PodSpec{Containers: []corev1.Container{app},
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon(db)}}})
	}},
	{name: "Deployment", object: func(db, app corev1.Container) any {
		return &appsv1.Deployment{TypeMeta: metav1Type("apps/v1", "Deployment"), Spec: appsv1.DeploymentSpec{
			Template: shortTemplate(corev1.PodSpec{Containers: []corev1.Container{app, db}})}}
	}},
	{name: "StatefulSet", object: func(db, app corev1.Container) any {
		return &appsv1.StatefulSet{TypeMeta: metav1Type("apps/v1", "StatefulSet"), Spec: appsv1.StatefulSetSpec{
			Template: shortTemplate(corev1.PodSpec{Containers: []corev1.Container{db, app}})}}
	}},
	{name: "Job", object: func(db, app corev1.Container) any {
		return &batchv1.Job{TypeMeta: metav1Type("batch/v1", "Job"), Spec: batchv1.JobSpec{
			Template: shortTemplate(corev1.PodSpec{Containers: []corev1.Container{app, db}})}}
	}},
	{name: "CronJob", object: func(db, app corev1.Container) any {
		return &batchv1.CronJob{TypeMeta: metav1Type("batch/v1", "CronJob"), Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *", JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: shortTemplate(corev1.PodSpec{Containers: []corev1.Container{db, app}})}}}}
	}},
}

// shortLayout writes an object as text.
type shortLayout struct {
	name string
	// hand: written by hand from the containers of a pod (shortPlacement.hand only).
	hand  bool
	write func(obj any, containers []corev1.Container) string
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// shortHandJSON writes the containers of a pod as compact JSON with "args" before "image".
func shortHandJSON(cs []corev1.Container) string {
	items := make([]string, len(cs))
	for i, c := range cs {
		items[i] = `{"name":` + mustJSON(c.Name) + `,"args":` + mustJSON(c.Args) + `,"image":` + mustJSON(c.Image) + `}`
	}
	return `{"kind":"Pod","spec":{"containers":[` + strings.Join(items, ",") + `]}}`
}

// shortHandYAML writes the containers of a pod as YAML with "image" before "args", each item of the arguments on
// a line of its own, as kubectl writes them.
func shortHandYAML(cs []corev1.Container) string {
	var b strings.Builder
	b.WriteString("kind: Pod\nspec:\n  containers:\n")
	for _, c := range cs {
		b.WriteString("  - name: " + c.Name + "\n    image: " + c.Image + "\n    args:\n")
		for _, a := range c.Args {
			b.WriteString("    - " + mustJSON(a) + "\n")
		}
		b.WriteString("    imagePullPolicy: IfNotPresent\n")
	}
	return b.String()
}

var shortLayouts = []shortLayout{
	{name: "JSON", write: func(obj any, _ []corev1.Container) string { return mustJSON(obj) }},
	{name: "indented JSON", write: func(obj any, _ []corev1.Container) string {
		b, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			panic(err)
		}
		return string(b)
	}},
	{name: "YAML", write: func(obj any, _ []corev1.Container) string {
		b, err := yaml.Marshal(obj)
		if err != nil {
			panic(err)
		}
		return string(b)
	}},
	{name: "JSON args before image", hand: true, write: func(_ any, cs []corev1.Container) string { return shortHandJSON(cs) }},
	{name: "YAML image before args", hand: true, write: func(_ any, cs []corev1.Container) string { return shortHandYAML(cs) }},
}

// shortPodContainers returns the containers a hand-written layout writes for a placement that has hand set.
func shortPodContainers(p shortPlacement, db, app corev1.Container) []corev1.Container {
	switch p.name {
	case "before another image":
		return []corev1.Container{db, app}
	case "after another image":
		return []corev1.Container{app, db}
	}
	return []corev1.Container{db}
}

// shortImageCorpus returns, for every image of the table, every flag form its tool reads the password in (not
// the separated form for a tool that reads it glued only), in every placement and layout: the secret is hidden,
// and the words of the container's arguments, and of a container of another image next to it, stay.
func shortImageCorpus() []shortImageCase {
	rng := rand.New(rand.NewPCG(1075, 5)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []shortImageCase
	i := 0
	for _, row := range shortImageRows() {
		for _, f := range shortArgForms[:3] {
			if f.separated && row.tool.glued {
				continue
			}
			for _, p := range shortPlacements {
				for _, l := range shortLayouts {
					if l.hand && !p.hand {
						continue
					}
					secret := shortImageSecret(rng, row.tool, f)
					image := shortImageRefs[i%len(shortImageRefs)](row.image)
					i++
					args := append(append([]string{shortLogLevel, shortDebug}, f.words(row.tool, shortValue(row.tool, secret))...),
						shortTail)
					db := corev1.Container{Name: "db", Image: image, Args: args}
					app := corev1.Container{Name: shortAppName, Image: shortAppImage, Args: shortAppArgs}
					kept := []string{shortLogLevel, shortTail}
					if p.name != shortOneDB {
						kept = append(kept, `"`+shortAppPort+`"`, shortAppImage)
					}
					in := l.write(p.object(db, app), shortPodContainers(p, db, app))
					out = append(out, shortImageCase{form: fmt.Sprintf("%s %s, %s, %s", row.image, f.name, p.name, l.name),
						in: in, secret: secret, kept: kept})
				}
			}
		}
	}
	return out
}

// shortImageKeptCases are containers that come out as main redacts them: an image not in the table (today's
// reading of arguments with no command word), a flag of a table image that is a port or another meaning, and a
// table image whose container has a command, which decides instead.
func shortImageKeptCases() []shortImageCase {
	var out []shortImageCase
	add := func(form, image string, command, args []string, kept ...string) {
		c := corev1.Container{Name: "db", Image: image, Command: command, Args: args}
		app := corev1.Container{Name: shortAppName, Image: shortAppImage, Args: shortAppArgs}
		pod := shortPod(corev1.PodSpec{Containers: []corev1.Container{c, app}})
		for _, l := range shortLayouts {
			if l.hand && command != nil {
				continue
			}
			in := l.write(pod, []corev1.Container{c, app})
			out = append(out, shortImageCase{form: form + ", " + l.name, in: in, kept: append(kept, `"`+shortAppPort+`"`)})
		}
	}
	add("postgres port", "postgres:16", nil, []string{"-p", "5432", "-c", "max_connections=200"}, `"5432"`)
	add("image not in the table, glued", "docker.io/library/influxdb:2", nil, []string{"-phunter2", "--verbose"}, "-phunter2")
	add("image not in the table, port", "nginx:1.27", nil, []string{"-p", "31337"}, `"31337"`)
	add("image not in the table that wraps a tool", "acme/mysql-client:1", nil, []string{"-pswrapped9"}, "-pswrapped9")
	add("mysql port flag", "mysql:8.4", nil, []string{"-P", "3306", "-h", "db"}, `"3306"`)
	add("redis port", "bitnami/redis:7", nil, []string{"-p", "6379", "-h", "cache"}, `"6379"`)
	add("curl user with no password", "curlimages/curl:8", nil, []string{"-u", "deploy", "https://example.com/x"}, "deploy")
	add("mysql with a command", "mysql:8.4", []string{"/docker-entrypoint.sh"}, []string{"-pswcommand9"}, "-pswcommand9")
	add("redis image with a command", shortRedis+":7", []string{"redis-server"}, []string{"-p", "6380"}, `"6380"`)
	return out
}

// shortImageTableInputs are the texts of the image corpus and its kept cases, in order.
func shortImageTableInputs() []string {
	var out []string
	for _, cs := range [][]shortImageCase{shortImageCorpus(), shortImageKeptCases()} {
		for _, c := range cs {
			out = append(out, c.in)
		}
	}
	return out
}

// shortImageHidden reports whether out hides secret: the secret is gone, and for a word secret its first and last
// six bytes too.
func shortImageHidden(secret, out string) bool {
	if strings.Contains(out, secret) {
		return false
	}
	if len(secret) >= 10 && (strings.Contains(out, secret[:6]) || strings.Contains(out, secret[len(secret)-6:])) {
		return false
	}
	return true
}

// Every input of the image corpus hides its secret and keeps the words around it, the other container's port
// included. On the branch before the image row every one of them showed the secret (counted in the pull request
// of #1075).
func TestShortFlagImageCorpusHidesTheKnownSecret(t *testing.T) {
	parallelUnderRace(t)
	cases := shortImageCorpus()
	leaked, lost := 0, 0
	for i, c := range cases {
		if !raceKeeps(t.Name(), i) {
			continue
		}
		out := String(c.in)
		if !shortImageHidden(c.secret, out) {
			leaked++
			if leaked <= 5 {
				t.Errorf("%s: the secret %q is left:\n in: %q\nout: %q", c.form, c.secret, c.in, out)
			}
		}
		for _, k := range c.kept {
			if !strings.Contains(out, k) {
				lost++
				if lost <= 5 {
					t.Errorf("%s: %q is gone:\n in: %q\nout: %q", c.form, k, c.in, out)
				}
				break
			}
		}
	}
	if leaked > 0 || lost > 0 {
		t.Errorf("image corpus: %d of %d inputs leak the secret, %d lose a word around it", leaked, len(cases), lost)
	}
}

// Every kept case keeps the words it names: an image not in the table, a port or another meaning of a table
// image's flag, and a container whose command decides.
func TestShortFlagImageKeptCases(t *testing.T) {
	for _, c := range shortImageKeptCases() {
		out := String(c.in)
		for _, k := range c.kept {
			if !strings.Contains(out, k) {
				t.Errorf("%s: %q is gone:\n in: %q\nout: %q", c.form, k, c.in, out)
			}
		}
	}
}

// shortImageMainTable holds main's output and redacted byte ranges for every input of shortImageTableInputs
// (commit a9b4120f), written once by running TestWriteShortFlagImageMainTable in a checkout of that commit with
// SHORTFLAG_IMAGE_MAIN_TABLE set to the output path; regenerate it only from that commit.
const shortImageMainTable = "testdata/shortflag-image-main.jsonl"

// TestWriteShortFlagImageMainTable writes shortImageMainTable from the redactor it runs with, when
// SHORTFLAG_IMAGE_MAIN_TABLE names the output path; otherwise it is skipped.
func TestWriteShortFlagImageMainTable(t *testing.T) {
	path := os.Getenv("SHORTFLAG_IMAGE_MAIN_TABLE")
	if path == "" {
		t.Skip("SHORTFLAG_IMAGE_MAIN_TABLE is not set")
	}
	inputs := shortImageTableInputs()
	rows := make([]any, 0, len(inputs))
	for _, in := range inputs {
		c := mainRedaction{In: in, Out: String(in)}
		for _, sp := range defaultRedactor.spans(nil, in) {
			c.Spans = append(c.Spans, [2]int{sp.start, sp.end})
		}
		rows = append(rows, c)
	}
	writeJSONLines(t, path, rows)
}

// Nothing main redacted in the image corpus is kept now (the property of #1075), every input of the corpus
// redacts more than main, and every kept case comes out as main redacts it.
func TestShortFlagImageCorpusAgainstMain(t *testing.T) {
	parallelUnderRace(t)
	gen := shortImageTableInputs()
	corpus := len(shortImageCorpus())
	same := 0
	n := readJSONLines(t, shortImageMainTable, func(i int, line []byte) {
		var row mainRedaction
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		checkTableInput(t, shortImageMainTable, i+1, row.In, gen)
		if !raceKeeps(t.Name(), i) {
			return
		}
		now := defaultRedactor.spans(nil, row.In)
		for _, want := range row.Spans {
			for b := want[0]; b < want[1]; b++ {
				if !coveredBy([2]int{b, b + 1}, now) {
					t.Errorf("main redacted byte %d, now kept:\n in: %q\nmain: %q\n now: %q", b, row.In, row.Out,
						String(row.In))
					break
				}
			}
		}
		got := String(row.In)
		if i < corpus && got == row.Out {
			same++
		}
		if i >= corpus && got != row.Out {
			t.Errorf("a kept case changed:\n in: %q\nmain: %q\n now: %q", row.In, row.Out, got)
		}
	})
	if n != len(gen) {
		t.Fatalf("%s has %d lines, the corpus %d inputs", shortImageMainTable, n, len(gen))
	}
	if same > 0 {
		t.Errorf("%d inputs of the image corpus come out as on main", same)
	}
}
