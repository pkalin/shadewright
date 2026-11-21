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

package incidentcontext

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// FuzzLogExcerpt checks the order of redaction and cuts on untrusted log text: whatever the text around it
// and wherever the bounds fall, a credential on its own line never reaches a block, whole or in part of its
// signature; a block stays within its bound and valid UTF-8; and the total cut keeps the header and whole
// lines.
func FuzzLogExcerpt(f *testing.F) {
	f.Add("line one\nline two\n", uint16(0), uint16(2048), uint16(4096))
	f.Add(strings.Repeat("x", 3000), uint16(1500), uint16(1024), uint16(4096))
	f.Add("a\nb\nc\n\xff\xfe\n", uint16(2), uint16(1100), uint16(1024))
	f.Add("password: |\n  ", uint16(14), uint16(1200), uint16(0))
	red, err := redact.New()
	if err != nil {
		f.Fatal(err)
	}
	b := &Builder{redactor: red, limits: DefaultLimits()}
	signature := leakJWT[strings.LastIndexByte(leakJWT, '.')+1:]
	f.Fuzz(func(t *testing.T, text string, at, budget, total uint16) {
		if len(text) > 16<<10 {
			return
		}
		i := min(int(at), len(text))
		text = text[:i] + "\n" + leakJWT + "\n" + text[i:]
		n := max(int(budget), 256)
		pick := logPick{namespace: testNS, pod: crashPod, container: appName, previous: true}
		blk, failed := b.logBlock(pick, providers.LogExcerpt{Text: text}, nil, n)
		if failed && len(text) <= 2*n {
			t.Fatalf("failed on an answer within bounds")
		}
		check := func(stage string, blk Block) {
			if strings.Contains(blk.Text, signature) {
				t.Fatalf("%s: the credential reached the block:\n%q", stage, blk.Text)
			}
			if !utf8.ValidString(blk.Text) {
				t.Fatalf("%s: invalid UTF-8", stage)
			}
			if blk.headLen > len(blk.Text) || (blk.headLen > 0 && blk.Text[blk.headLen-1] != '\n') {
				t.Fatalf("%s: header length %d of %q", stage, blk.headLen, blk.Text)
			}
		}
		check("section", blk)
		if len(blk.Text) > n {
			t.Fatalf("block of %d bytes over its bound of %d", len(blk.Text), n)
		}
		c := &Context{Blocks: []Block{{Section: SectionSummary, Text: "subject\n"}, blk}}
		cb := &Builder{limits: Limits{Total: int(total)}}
		cb.fitTotal(c)
		if c.Size() > max(int(total), 0) {
			t.Fatalf("context of %d bytes over its bound of %d", c.Size(), total)
		}
		if c.Truncated != (len(blk.Text)+len("subject\n") > int(total)) {
			t.Fatalf("truncated %t for %d bytes and a bound of %d", c.Truncated, len(blk.Text)+8, total)
		}
		check("total", c.Blocks[1])
		if c.Blocks[1].headLen > 0 && !strings.HasPrefix(c.Blocks[1].Text, "pod web-abc-1 container app, previous instance\n") {
			t.Fatalf("the total cut lost the header: %q", c.Blocks[1].Text)
		}
	})
}
