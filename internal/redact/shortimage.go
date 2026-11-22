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
	"strings"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The image of a container as its command word (#1075, second decision of 2026-11-22). A container read as an
// object with "args" and no "command" runs the entrypoint of its image, so the base name of the object's "image"
// (no registry host and port, path, tag or digest: "registry.example:5000/library/mysql:8.4@sha256:..." gives
// "mysql") is the command word of those arguments when it names a row of the table. The image is a field of the
// object already given to the redactor; nothing else is read.
//
// shortObjects finds, before the reading of shortFlagRead, the objects of the text and in each its "image",
// "args" and "command" keys, wherever they stand in the object (kubectl and sigs.k8s.io/yaml write "args" before
// "command" and "image", get_resource after them): a JSON or flow object from "{" to its "}", or a
// YAML block mapping, the keys at one column from the line that opens it (a "- " item or a key line) to the
// first line with less indentation (outside braces and brackets). An object is read once, in one pass, and its
// "args" keys get its row when it closes; an "image" or "args" key of a nested object (a container inside a pod
// template) belongs to the nested one only.

// imageRows names the rows of the table by an image's base name where the image is not called as its tool: the
// redis image holds redis-cli.
var imageRows = map[string]string{"redis": "redis-cli"}

// imageTool returns the row of the table that the image reference ref names by its base name.
func imageTool(wm *meter.Meter, ref string) (shortFlagTool, bool) {
	name := ref
	if k := strings.IndexByte(name, '@'); k >= 0 {
		name = name[:k]
	}
	if k := strings.LastIndexByte(name, '/'); k >= 0 {
		name = name[k+1:]
	}
	if k := strings.IndexByte(name, ':'); k >= 0 {
		name = name[:k]
	}
	tick(wm, len(ref))
	if alias, ok := imageRows[name]; ok {
		name = alias
	}
	t, ok := shortFlagTools[name]
	return t, ok
}

// imageObject is an object being read by shortObjects.
type imageObject struct {
	// brace: a JSON or flow object; else a YAML block mapping whose keys stand at column col.
	brace bool
	col   int
	tool  shortFlagTool
	// image: the object's "image" names a row of the table (tool); command: the object has a "command" key, the
	// first of which starts at commandAt.
	image, command bool
	commandAt      int
	// from: the index in imageReader.args of the object's first "args" key.
	from int
}

// argsRow is what decides the arguments of an "args" key: the "command" key of its object (command, its start;
// -1 when the object has none), else the object's "image" when it names a row of the table (image, tool).
type argsRow struct {
	command int
	image   bool
	tool    shortFlagTool
}

// imageReader is the state of shortObjects.
type imageReader struct {
	keyWords
	objects []imageObject
	// args: the starts of the "args" keys of the open objects, the innermost last.
	args             []int
	out              map[int]argsRow
	braces, brackets int
}

// shortObjects returns, by the start of each "args" key whose object has a "command" key or an "image" that
// names a row of the table, what decides its arguments; nil when there is none. Each byte is read once as a word
// or a separator, and once more as the indentation of a line.
func shortObjects(wm *meter.Meter, s string) map[int]argsRow {
	r := &imageReader{keyWords: keyWords{s: s}}
	for i := 0; i < len(s); {
		tick(wm, 1)
		if (i == 0 || s[i-1] == '\n') && r.braces == 0 && r.brackets == 0 {
			r.line(wm, i)
		}
		switch c := s[i]; {
		case c == '{':
			r.braces++
			r.objects = append(r.objects, imageObject{brace: true, from: len(r.args)})
			i++
		case c == '}':
			if r.braces > 0 {
				r.braces--
				r.closeBrace(wm)
			}
			i++
		case c == '[':
			r.brackets++
			i++
		case c == ']':
			r.brackets = max(r.brackets-1, 0)
			i++
		case shortDelimiter(wm, c) || strings.IndexByte(";|&()`", c) >= 0:
			i++
		default:
			j := i
			for j < len(s) && !shortDelimiter(wm, s[j]) && strings.IndexByte(";|&()`{}[]", s[j]) < 0 {
				tick(wm, 1)
				j++
			}
			r.word(wm, i, j)
			i = j
		}
	}
	for len(r.objects) > 0 {
		tick(wm, 1)
		r.close(wm)
	}
	return r.out
}

// close closes the innermost object: its "args" keys take its command, or with none its image of the table.
func (r *imageReader) close(wm *meter.Meter) {
	o := r.objects[len(r.objects)-1]
	r.objects = r.objects[:len(r.objects)-1]
	if o.image || o.command {
		row := argsRow{command: -1, image: o.image, tool: o.tool}
		if o.command {
			row = argsRow{command: o.commandAt}
		}
		for _, at := range r.args[o.from:] {
			tick(wm, 1)
			if r.out == nil {
				r.out = map[int]argsRow{}
			}
			r.out[at] = row
		}
	}
	r.args = r.args[:o.from]
}

// closeBrace closes the objects up to the innermost brace object, that one included.
func (r *imageReader) closeBrace(wm *meter.Meter) {
	for len(r.objects) > 0 {
		tick(wm, 1)
		brace := r.objects[len(r.objects)-1].brace
		r.close(wm)
		if brace {
			return
		}
	}
}

// line reads the indentation of the line that starts at i, outside braces and brackets: it closes the YAML
// mappings whose keys stand further right, and opens one for a "- " item (at the column of the item's content)
// or for a key line at a column with no open mapping. A blank or comment line changes nothing.
func (r *imageReader) line(wm *meter.Meter, i int) {
	s := r.s
	k := i
	for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
		tick(wm, 1)
		k++
	}
	if k == len(s) || s[k] == '\n' || s[k] == '\r' || s[k] == '#' {
		return
	}
	col := k - i
	if s[k] == '-' && (k+1 == len(s) || s[k+1] == ' ' || s[k+1] == '\t') {
		r.closeRight(wm, col)
		m := k + 1
		for m < len(s) && (s[m] == ' ' || s[m] == '\t') {
			tick(wm, 1)
			m++
		}
		r.objects = append(r.objects, imageObject{col: m - i, from: len(r.args)})
		return
	}
	r.closeRight(wm, col)
	if n := len(r.objects); n == 0 || r.objects[n-1].col != col {
		r.objects = append(r.objects, imageObject{col: col, from: len(r.args)})
	}
}

// closeRight closes the YAML mappings whose keys stand right of col.
func (r *imageReader) closeRight(wm *meter.Meter, col int) {
	for n := len(r.objects); n > 0 && !r.objects[n-1].brace && r.objects[n-1].col > col; n = len(r.objects) {
		tick(wm, 1)
		r.close(wm)
	}
}

// word reads the word s[i:j]: the value of an "image" key, an "args" key or a "command" key of the innermost
// object.
func (r *imageReader) word(wm *meter.Meter, i, j int) {
	if len(r.objects) == 0 {
		r.push(i, j)
		return
	}
	o := &r.objects[len(r.objects)-1]
	if _, ok := r.keyAt("image"); ok {
		o.tool, o.image = imageTool(wm, r.s[i:j])
	}
	r.push(i, j)
	if at, ok := r.keyAt("args"); ok {
		r.args = append(r.args, at)
	}
	if at, ok := r.keyAt("command"); ok && !o.command {
		o.command, o.commandAt = true, at
	}
}
