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

	"sigs.k8s.io/yaml"
)

// Four forms of #1075 that the decision covers and the first build left shown (count of 2026-11-22): the key
// order of a container ("command" after "args", as kubectl and sigs.k8s.io/yaml write it), a quoted value glued
// to the flag, the end of a list item (the command of one container never reaches the next), and an args item
// written as a YAML block scalar; and a fifth group of forms of the same kind (comments, one-string values, CRLF,
// anchors and tags). Each group is generated from the dimensions of the corpora before it (every
// tool of the table as the command word or the image, every flag form, every layout), with the answer known by
// construction, and compared with main (shortFormsMainTable).

// shortBox is a container written by hand, its keys in the order given.
type shortBox struct {
	name, image   string
	command, args []string
	order         []string
}

// shortRedisCLI is the redis client of the table.
const shortRedisCLI = "redis" + "-cli"

const (
	keyName    = "name"
	keyImage   = "image"
	keyCommand = "command"
	keyArgs    = "args"
)

// The key orders of a container: alphabetical (kubectl, sigs.k8s.io/yaml, the last-applied JSON), the order of
// the Go type, and the command as the last key of the item.
var (
	shortAlphabetical = []string{keyArgs, keyCommand, keyImage, keyName}
	shortGoOrder      = []string{keyName, keyImage, keyCommand, keyArgs}
	shortCommandLast  = []string{keyName, keyImage, keyArgs, keyCommand}
	shortNameFirst    = []string{keyName, keyArgs, keyImage}
)

// yamlScalar writes s as sigs.k8s.io/yaml writes a string: plain where it can, quoted where it must.
func yamlScalar(s string) string {
	b, err := yaml.Marshal(s)
	if err != nil {
		panic(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// present reports whether the box writes the key k.
func (b shortBox) present(k string) bool {
	switch k {
	case keyImage:
		return b.image != ""
	case keyCommand:
		return b.command != nil
	case keyArgs:
		return b.args != nil
	}
	return true
}

// json writes the box as a compact JSON object.
func (b shortBox) json() string {
	var parts []string
	for _, k := range b.order {
		if !b.present(k) {
			continue
		}
		var v string
		switch k {
		case keyName:
			v = mustJSON(b.name)
		case keyImage:
			v = mustJSON(b.image)
		case keyCommand:
			v = mustJSON(b.command)
		case keyArgs:
			v = mustJSON(b.args)
		}
		parts = append(parts, mustJSON(k)+":"+v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// flow writes the box as a YAML flow mapping with plain scalars.
func (b shortBox) flow() string {
	var parts []string
	for _, k := range b.order {
		if !b.present(k) {
			continue
		}
		var v string
		switch k {
		case keyName:
			v = b.name
		case keyImage:
			v = b.image
		case keyCommand:
			v = "[" + strings.Join(b.command, ", ") + "]"
		case keyArgs:
			v = "[" + strings.Join(b.args, ", ") + "]"
		}
		parts = append(parts, k+": "+v)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// block writes the box as an item of a YAML block sequence whose dash stands at column 2, as under
// "containers:"; with scalar set the items of its args are block scalars with that header ("- >-" and the item on
// the next line).
func (b shortBox) block(scalar string) string {
	const ind = "  "
	var sb strings.Builder
	first := true
	for _, k := range b.order {
		if !b.present(k) {
			continue
		}
		if first {
			sb.WriteString(ind + "- ")
			first = false
		} else {
			sb.WriteString(ind + "  ")
		}
		switch k {
		case keyName:
			sb.WriteString("name: " + yamlScalar(b.name) + "\n")
		case keyImage:
			sb.WriteString("image: " + yamlScalar(b.image) + "\n")
		case keyCommand, keyArgs:
			items := b.command
			if k == keyArgs {
				items = b.args
			}
			sb.WriteString(k + ":\n")
			for _, a := range items {
				if k == keyArgs && scalar != "" {
					sb.WriteString(ind + "  - " + scalar + "\n" + ind + "    " + a + "\n")
				} else {
					sb.WriteString(ind + "  - " + yamlScalar(a) + "\n")
				}
			}
		}
	}
	return sb.String()
}

// shortPodLayout writes the containers of a pod.
type shortPodLayout struct {
	name  string
	write func(bs []shortBox) string
}

func shortBlockPod(scalar string) func(bs []shortBox) string {
	return func(bs []shortBox) string {
		var sb strings.Builder
		sb.WriteString("kind: Pod\nspec:\n  containers:\n")
		for _, b := range bs {
			sb.WriteString(b.block(scalar))
		}
		sb.WriteString("  restartPolicy: Always\n")
		return sb.String()
	}
}

var shortPodLayouts = []shortPodLayout{
	{name: "JSON", write: func(bs []shortBox) string {
		items := make([]string, len(bs))
		for i, b := range bs {
			items[i] = b.json()
		}
		return `{"kind":"Pod","spec":{"containers":[` + strings.Join(items, ",") + `]}}`
	}},
	{name: "block YAML", write: shortBlockPod("")},
	{name: "flow YAML", write: func(bs []shortBox) string {
		items := make([]string, len(bs))
		for i, b := range bs {
			items[i] = b.flow()
		}
		return "kind: Pod\nspec: {containers: [" + strings.Join(items, ", ") + "]}\n"
	}},
}

// shortAppBox is a container of an image not in the table whose "-p" is a port.
func shortAppBox(order []string) shortBox {
	return shortBox{name: shortAppName, image: shortAppImage, args: shortAppArgs, order: order}
}

// shortImageOf returns the image base name of the tool t ("redis" for redis-cli, as kubectl users name it).
func shortImageOf(t shortTool) string {
	if t.letter == 'a' { // redis-cli, the one tool of the table whose flag is -a
		return shortRedis
	}
	return t.name
}

// shortFormsHidden reports whether out hides every secret: none is left, and for a secret of ten bytes or more
// neither its first nor its last six bytes.
func shortFormsHidden(secrets []string, out string) bool {
	for _, s := range secrets {
		if strings.Contains(out, s) {
			return false
		}
		if len(s) >= 10 && (strings.Contains(out, s[:6]) || strings.Contains(out, s[len(s)-6:])) {
			return false
		}
	}
	return true
}

// shortKeyOrderCorpus is group 1: a container whose "command" (a tool of the table, by its name or a path) comes
// after its "args", with no image, the tool's own image or an image not in the table, alone or next to a
// container of another image, in JSON, block YAML and flow YAML: the arguments take the command's row. The secret
// is one the arguments with no command word show (shortImageSecret), so only the command decides. And with no
// command, an image of the table after the arguments in flow YAML, which the image corpus has no layout for.
func shortKeyOrderCorpus() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 7)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []shortFlagCase
	i := 0
	for _, t := range shortTools {
		for _, f := range shortArgForms {
			if f.separated && t.glued {
				continue
			}
			for _, image := range []string{"", shortImageOf(t) + ":8", shortAppImage, "-"} {
				for _, l := range shortPodLayouts {
					secret := shortImageSecret(rng, t, f)
					i++
					args := append(append([]string{shortLogLevel, shortDebug}, f.words(t, shortValue(t, secret))...), shortTail)
					db := shortBox{name: "db", image: image, command: []string{t.name}, args: args, order: shortAlphabetical}
					if i%2 == 0 {
						db.command = []string{"/usr/local/bin/" + t.name}
					}
					form := fmt.Sprintf("key order: %s %s, command after args, image %q, %s", t.name, f.name, image, l.name)
					if image == "-" {
						// No command: the image of the table after the arguments decides.
						db.command, db.image = nil, shortImageOf(t)+":8"
						form = fmt.Sprintf("key order: %s %s, image after args, no command, %s", t.name, f.name, l.name)
					}
					boxes := make([]shortBox, 0, 2)
					kept := []string{shortLogLevel, shortTail}
					switch i % 3 {
					case 1:
						boxes = append(boxes, shortAppBox(shortAlphabetical), db)
					case 2:
						boxes = append(boxes, db, shortAppBox(shortAlphabetical))
					default:
						boxes = append(boxes, db)
					}
					if len(boxes) > 1 {
						kept = append(kept, shortAppPort)
					}
					out = append(out, shortFlagCase{form: form, in: l.write(boxes), secrets: []string{secret}, kept: kept})
				}
			}
		}
	}
	return out
}

// shortQuotedForms are group 2's flag forms: a quoted value glued to the flag, after "=", the quote closed in
// the middle of the value or opened there, and a quoted value as a word of its own. a and b are the two halves
// of the value.
var shortQuotedForms = []shortArgForm{
	{name: "glued single quotes", words: func(t shortTool, v string) []string { return []string{shortFlag(t) + "'" + v + "'"} }},
	{name: "glued double quotes", words: func(t shortTool, v string) []string { return []string{shortFlag(t) + `"` + v + `"`} }},
	{name: "equals single quotes", words: func(t shortTool, v string) []string { return []string{shortFlag(t) + "='" + v + "'"} }},
	{name: "equals double quotes", words: func(t shortTool, v string) []string { return []string{shortFlag(t) + `="` + v + `"`} }},
	{name: "single quote closed inside", words: func(t shortTool, v string) []string {
		return []string{shortFlag(t) + "'" + v[:len(v)-6] + "'" + v[len(v)-6:]}
	}},
	{name: "double quote opened inside", words: func(t shortTool, v string) []string {
		return []string{shortFlag(t) + v[:len(v)-6] + `"` + v[len(v)-6:] + `"`}
	}},
	{name: "combined glued double quotes", words: func(t shortTool, v string) []string {
		return []string{"-v" + string(t.letter) + `"` + v + `"`}
	}},
	{name: "separated single quotes", separated: true,
		words: func(t shortTool, v string) []string { return []string{shortFlag(t), "'" + v + "'"} }},
	{name: "separated double quotes", separated: true,
		words: func(t shortTool, v string) []string { return []string{shortFlag(t), `"` + v + `"`} }},
}

// shortQuoteClash reports whether the form f puts into the value a quote that the context ctx quotes the whole
// command with, which no shell line could hold.
func shortQuoteClash(f shortArgForm, ctx shortContext) bool {
	return strings.Contains(ctx.name, "single quotes") && strings.Contains(f.name, "single") ||
		strings.Contains(ctx.name, "double quotes") && strings.Contains(f.name, "double")
}

// shortQuotedSecrets are the pieces of secret a quoted form must hide: the whole, and for a form that splits it
// at a quote both halves.
func shortQuotedSecrets(f shortArgForm, t shortTool, secret string) []string {
	if !strings.Contains(f.name, "inside") {
		return []string{secret}
	}
	v := shortValue(t, secret)
	a, b := v[:len(v)-6], v[len(v)-6:]
	if t.colon {
		a = strings.TrimPrefix(a, "admin:")
	}
	return []string{a, b}
}

// shortQuotedCorpus is group 2: every tool of the table with every quoted form it reads the password in, in
// every context of a command; and every flag of the arguments with no command word with the same forms but the
// combined one, in every context of those.
func shortQuotedCorpus() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 8)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []shortFlagCase
	i := 0
	for _, t := range shortTools {
		for _, f := range shortQuotedForms {
			if f.separated && t.glued {
				continue
			}
			for _, ctx := range shortContexts {
				if shortQuoteClash(f, ctx) {
					continue
				}
				secret := shortSecret(rng, i)
				i++
				args := append(append([]string{shortHead, "9"}, f.words(t, shortValue(t, secret))...), shortTail)
				in, items := ctx.text(t.name, args)
				out = append(out, shortFlagCase{form: "quoted: tool " + t.name + " " + f.name + ", " + ctx.name, in: in,
					items: items, secrets: shortQuotedSecrets(f, t, secret), kept: []string{t.name, shortHead, shortTail}})
			}
		}
	}
	for _, t := range shortArgsFlags {
		for _, f := range shortQuotedForms {
			if strings.HasPrefix(f.name, "combined") {
				continue // the arguments with no command word read no combined flags
			}
			for _, ctx := range shortArgsContexts {
				secret := shortSecret(rng, i)
				i++
				words := f.words(t, shortValue(t, secret))
				args := append(append([]string{shortLogLevel, shortDebug}, words...), shortTail)
				if strings.HasPrefix(ctx.name, "sequence") || strings.HasPrefix(ctx.name, "strings") {
					args = append(append(words, shortLogLevel, shortDebug), shortTail)
				}
				in, items := ctx.text("", args)
				out = append(out, shortFlagCase{form: "quoted: args " + f.name + ", " + ctx.name, in: in, items: items,
					secrets: shortQuotedSecrets(f, t, secret), kept: []string{shortLogLevel, shortTail}})
			}
		}
	}
	return out
}

// shortItemCorpus is group 3: a container whose command is a tool of the table, in each key order (the command
// the last key of the item among them), then a second container with no command whose arguments hold another
// flag of the arguments with no command word ("-a" after mysql) with a value that is no port, with no image, an
// image not in the table or the image of the tool of that flag, in JSON, block YAML and flow YAML. The second
// container's value is hidden: the first one's command does not reach it.
func shortItemCorpus() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 9)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	imageOf := map[byte]string{'p': "mongo:7", 'P': "sqlcmd:1", 'a': "redis:7", 'u': "curlimages/curl:8"}
	var out []shortFlagCase
	i := 0
	for _, t := range shortTools {
		for _, next := range shortArgsFlags {
			if next.letter == t.letter {
				continue
			}
			for _, order := range [][]string{shortCommandLast, shortGoOrder, shortAlphabetical} {
				for _, image := range []string{"", shortAppImage, imageOf[next.letter]} {
					for _, l := range shortPodLayouts {
						first, second := shortSecret(rng, i), shortSecret(rng, i+1)
						i += 2
						db := shortBox{name: "db", image: shortAppImage, command: []string{t.name},
							args:  []string{shortFlag(t) + shortValue(t, first)},
							order: order}
						order2 := shortNameFirst
						if i%4 == 0 {
							order2 = shortAlphabetical
						}
						worker := shortBox{name: "worker", image: image,
							args:  []string{shortLogLevel, shortDebug, shortFlag(next), shortValue(next, second), shortTail},
							order: order2}
						out = append(out, shortFlagCase{
							form: fmt.Sprintf("item end: %s then %s, order %v, image %q, %s", t.name,
								shortFlag(next), order, image, l.name),
							in: l.write([]shortBox{db, worker}), secrets: []string{first, second},
							kept: []string{shortLogLevel, shortTail}})
					}
				}
			}
		}
	}
	return out
}

// shortScalarIndicators are the headers of a block scalar an args item can be written with.
var shortScalarIndicators = []string{">-", "|-", ">", "|"}

// shortScalarCorpus is group 4: the arguments of a container written as block scalars ("- >-" and the item on
// the next line), under an image of the table with no command, under a command of the table before the
// arguments, and under one after them, alone or next to a container of another image: they are read as the
// plain items are.
func shortScalarCorpus() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 10)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []shortFlagCase
	i := 0
	for _, row := range shortImageRows() {
		for _, f := range shortArgForms[:3] {
			if f.separated && row.tool.glued {
				continue
			}
			for _, ind := range shortScalarIndicators {
				for _, decider := range []string{"image", "command before args", "command after args"} {
					secret := shortImageSecret(rng, row.tool, f)
					i++
					args := append(append([]string{shortLogLevel, shortDebug}, f.words(row.tool, shortValue(row.tool, secret))...),
						shortTail)
					db := shortBox{name: "db", image: row.image + ":8", args: args, order: shortNameFirst}
					switch decider {
					case "command before args":
						db.image, db.command, db.order = shortAppImage, []string{row.tool.name}, shortGoOrder
					case "command after args":
						db.image, db.command, db.order = shortAppImage, []string{row.tool.name}, shortAlphabetical
					}
					boxes := make([]shortBox, 0, 2)
					kept := []string{shortLogLevel, shortTail}
					switch i % 3 {
					case 1:
						boxes = append(boxes, shortAppBox(shortNameFirst), db)
					case 2:
						boxes = append(boxes, db, shortAppBox(shortAlphabetical))
					default:
						boxes = append(boxes, db)
					}
					if len(boxes) > 1 {
						kept = append(kept, shortAppPort)
					}
					out = append(out, shortFlagCase{
						form: fmt.Sprintf("block scalar: %s %s, %q, under the %s", row.image, f.name, ind, decider),
						in:   shortBlockPod(ind)(boxes), secrets: []string{secret}, kept: kept})
				}
			}
		}
	}
	return out
}

// shortOtherVariants are the layouts of group 5, forms of the same kind found after the four: comment lines in
// a YAML sequence and between a flag and its value, "args" written as one string, "command" written as one string
// after the arguments, CRLF line ends, and an anchor or a tag before a block scalar item. Each writes a pod of
// the container db (its image of the table, or with command set an image not in the table and that command)
// next to a container of another image.
var shortOtherVariants = []struct {
	name    string
	command bool
	write   func(db, app shortBox) string
}{
	{name: "comment after the args key", write: func(db, app shortBox) string {
		return strings.ReplaceAll(shortBlockPod("")([]shortBox{app, db}), "args:\n", "args:\n    # the arguments\n")
	}},
	{name: "comment before every item", write: func(db, app shortBox) string {
		return strings.ReplaceAll(shortBlockPod("")([]shortBox{db, app}), "\n    - ", "\n    # next\n    - ")
	}},
	{name: "args as one plain string", write: func(db, app shortBox) string {
		return "kind: Pod\nspec:\n  containers:\n  - name: db\n    image: " + db.image + "\n    args: " +
			strings.Join(db.args, " ") + "\n" + app.block("")
	}},
	{name: "args as one JSON string", write: func(db, app shortBox) string {
		return `{"kind":"Pod","spec":{"containers":[` + app.json() + `,{"name":"db","image":` + mustJSON(db.image) +
			`,"args":` + mustJSON(strings.Join(db.args, " ")) + `}]}}`
	}},
	{name: "command as one string after args, YAML", command: true, write: func(db, app shortBox) string {
		db.order = []string{keyArgs, keyImage, keyName}
		return "kind: Pod\nspec:\n  containers:\n" + db.block("") + "    command: " + db.command[0] + "\n" +
			app.block("")
	}},
	{name: "command as one string after args, JSON", command: true, write: func(db, app shortBox) string {
		return `{"kind":"Pod","spec":{"containers":[{"name":"db","args":` + mustJSON(db.args) + `,"command":` +
			mustJSON(db.command[0]) + `,"image":` + mustJSON(db.image) + `},` + app.json() + `]}}`
	}},
	{name: "CRLF", write: func(db, app shortBox) string {
		return strings.ReplaceAll(shortBlockPod("")([]shortBox{db, app}), "\n", "\r\n")
	}},
	{name: "CRLF, command after args", command: true, write: func(db, app shortBox) string {
		db.order = shortAlphabetical
		return strings.ReplaceAll(shortBlockPod("")([]shortBox{app, db}), "\n", "\r\n")
	}},
	{name: "CRLF block scalar", write: func(db, app shortBox) string {
		return strings.ReplaceAll(shortBlockPod("|-")([]shortBox{db, app}), "\n", "\r\n")
	}},
	{name: "anchor before a block scalar", write: func(db, app shortBox) string {
		return shortBlockPod("&arg >-")([]shortBox{db, app})
	}},
	{name: "tag before a block scalar", write: func(db, app shortBox) string {
		return shortBlockPod("!!str |")([]shortBox{app, db})
	}},
}

// shortOtherCorpus is group 5: every image of the table (or its tool as the command) with every flag form its tool
// reads the password in, in every layout of shortOtherVariants.
func shortOtherCorpus() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 11)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []shortFlagCase
	for _, row := range shortImageRows() {
		for _, f := range shortArgForms[:3] {
			if f.separated && row.tool.glued {
				continue
			}
			for _, v := range shortOtherVariants {
				secret := shortImageSecret(rng, row.tool, f)
				args := append(append([]string{shortLogLevel, shortDebug}, f.words(row.tool, shortValue(row.tool, secret))...),
					shortTail)
				db := shortBox{name: "db", image: row.image + ":8", args: args, order: shortNameFirst}
				if v.command {
					db.image, db.command = shortAppImage, []string{row.tool.name}
				}
				out = append(out, shortFlagCase{form: fmt.Sprintf("other: %s %s, %s", row.image, f.name, v.name),
					in: v.write(db, shortAppBox(shortAlphabetical)), secrets: []string{secret},
					kept: []string{shortLogLevel, shortTail, shortAppPort}})
			}
		}
	}
	return out
}

// shortFormsKeptCases are inputs of the same layouts that come out as main redacts them: a command not in the
// table after arguments under an image of the table (the command decides, and it has no row), the port of a
// table tool after its arguments, and the next item's port after a command of the table.
func shortFormsKeptCases() []shortFlagCase {
	out := make([]shortFlagCase, 0, 4*len(shortPodLayouts))
	for _, l := range shortPodLayouts {
		for _, b := range []shortBox{
			{name: "db", image: "mysql:8.4", command: []string{"/docker-entrypoint.sh"}, args: []string{"-pswcommand9"},
				order: shortAlphabetical},
			{name: "db", image: "mysql:8.4", command: []string{shortMySQL}, args: []string{"-P", "3306", "-h", "db"},
				order: shortAlphabetical},
			{name: "cache", image: "redis:7", command: []string{shortRedisCLI}, args: []string{"-p", "6380"},
				order: shortAlphabetical},
		} {
			in := l.write([]shortBox{b, shortAppBox(shortNameFirst)})
			out = append(out, shortFlagCase{form: "kept: " + b.command[0] + ", " + l.name, in: in})
		}
		in := l.write([]shortBox{{name: "db", image: shortAppImage, command: []string{"mongosh"}, args: []string{"--quiet"},
			order: shortCommandLast}, {name: "web", args: []string{"-p", "8443"}, order: shortNameFirst}})
		out = append(out, shortFlagCase{form: "kept: next item's port, " + l.name, in: in})
	}
	return out
}

// shortFormsGroups are the five groups by name.
func shortFormsGroups() []struct {
	name  string
	cases []shortFlagCase
} {
	return []struct {
		name  string
		cases []shortFlagCase
	}{
		{"key order", shortKeyOrderCorpus()}, {"quoting", shortQuotedCorpus()},
		{"item boundaries", shortItemCorpus()}, {"block scalars", shortScalarCorpus()},
		{"other forms", shortOtherCorpus()},
	}
}

// shortFormsEntries are the entry points every text of the five groups goes through: String and each entry with
// a flag that redacts one text whole.
func shortFormsEntries() []flagged {
	out := []flagged{{name: "String (plain)", run: func(in string) ([]string, bool) { return []string{String(in)}, true }}}
	for _, e := range flaggedEntries {
		// Strings and the pieces entries split the text; the others redact it whole.
		if strings.HasPrefix(e.name, "Strings") || strings.HasPrefix(e.name, "Pieces") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Every input of the five groups hides its secrets and keeps the words around them, at every entry point. On the
// branch before this round each group left secrets shown (counts in the pull request of #1075).
func TestShortFlagFormsHideTheKnownSecret(t *testing.T) {
	parallelUnderRace(t)
	entries := shortFormsEntries()
	for _, g := range shortFormsGroups() {
		leaked, lost := 0, 0
		for i, c := range g.cases {
			if !raceKeeps(t.Name()+g.name, i) {
				continue
			}
			outs := []string{c.out()}
			// Every entry on a fifth of the inputs: they share String's reading, and the whole corpus through
			// all of them takes seconds.
			if c.items == nil && i%5 == 0 {
				for _, e := range entries[1:] {
					o, ok := e.run(c.in)
					if !ok {
						t.Errorf("%s: %s withheld it", c.form, e.name)
					}
					outs = append(outs, strings.Join(o, ""))
				}
			}
			for _, out := range outs {
				if !shortFormsHidden(c.secrets, out) {
					leaked++
					if leaked <= 5 {
						t.Errorf("%s: a secret %q is left:\n in: %q %q\nout: %q", c.form, c.secrets, c.in, c.items, out)
					}
					break
				}
				if k := shortKept(c, out); k != "" {
					lost++
					if lost <= 5 {
						t.Errorf("%s: %q is gone:\n in: %q %q\nout: %q", c.form, k, c.in, c.items, out)
					}
					break
				}
			}
		}
		if leaked > 0 || lost > 0 {
			t.Errorf("%s: %d of %d inputs leak a secret, %d lose the text around it", g.name, leaked, len(g.cases), lost)
		}
	}
}

// shortFormsMainTable holds main's output and redacted byte ranges for every text of shortFormsTableInputs
// (origin/main at 7ba21160), written once by running TestWriteShortFlagFormsMainTable in a checkout of that
// commit with this file and the corpus files it uses copied in, and SHORTFLAG_FORMS_MAIN_TABLE set to the output
// path; regenerate it only from that commit.
const shortFormsMainTable = "testdata/shortflag-forms-main.jsonl"

// shortFormsTableInputs are the texts of the five groups, then of the kept cases, in order.
func shortFormsTableInputs() []string {
	var out []string
	for _, g := range shortFormsGroups() {
		for _, c := range g.cases {
			if c.items == nil {
				out = append(out, c.in)
			}
		}
	}
	for _, c := range shortFormsKeptCases() {
		out = append(out, c.in)
	}
	return out
}

// TestWriteShortFlagFormsMainTable writes shortFormsMainTable from the redactor it runs with, when
// SHORTFLAG_FORMS_MAIN_TABLE names the output path; otherwise it is skipped.
func TestWriteShortFlagFormsMainTable(t *testing.T) {
	path := os.Getenv("SHORTFLAG_FORMS_MAIN_TABLE")
	if path == "" {
		t.Skip("SHORTFLAG_FORMS_MAIN_TABLE is not set")
	}
	inputs := shortFormsTableInputs()
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

// Nothing main redacted in the five groups is kept now, and every kept case comes out as main redacts it.
func TestShortFlagFormsAgainstMain(t *testing.T) {
	parallelUnderRace(t)
	gen := shortFormsTableInputs()
	kept := map[string]bool{}
	for _, c := range shortFormsKeptCases() {
		kept[c.in] = true
	}
	n := readJSONLines(t, shortFormsMainTable, func(i int, line []byte) {
		var row mainRedaction
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		checkTableInput(t, shortFormsMainTable, i+1, row.In, gen)
		if !kept[row.In] && !raceKeeps(t.Name(), i) {
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
		if got := String(row.In); kept[row.In] && got != row.Out {
			t.Errorf("a kept case changed:\n in: %q\nmain: %q\n now: %q", row.In, row.Out, got)
		}
	})
	if n != len(gen) {
		t.Fatalf("%s has %d lines, the corpus %d inputs", shortFormsMainTable, n, len(gen))
	}
}
