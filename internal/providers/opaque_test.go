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

package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// No printed, logged or encoded form of an Opaque, alone or inside a Message, shows its bytes; the adapter
// gets them back unchanged, and a change to the returned copy does not reach them.
func TestOpaqueHidesItsBytes(t *testing.T) {
	const mark = "THINKING-MARK-2c8e"
	src := []byte(`[{"type":"thinking","thinking":"` + mark + `"}]`)
	o := NewOpaque(src)
	src[0] = 'x'
	m := Message{Role: RoleAssistant, ProviderBlocks: o}
	enc, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("message", "blocks", o, "message", m)
	if s := fmt.Sprintf("%v %+v %#v %s %s %s", m, m, m, o, enc, logged.String()); strings.Contains(s, mark) {
		t.Errorf("the bytes show: %s", s)
	}
	// Every verb and flag, on the value, a pointer to it and a struct that holds it, prints no byte of the
	// data. The format strings are built at run time, so vet does not refuse the verbs that do not fit.
	verbs := []string{"v", "+v", "#v", "s", "q", "x", "X", "% x", "d", "+d", "5d", "c", "o", "O", "b", "U",
		"#U", "e", "f", "g", "t", "p", "08b"}
	for _, verb := range verbs {
		for _, arg := range []any{o, &o, m, &m, struct{ B Opaque }{o}, []Opaque{o}} {
			s := fmt.Sprintf("%"+verb, arg)
			for _, leak := range []string{mark, "84 72 73", "[91 123", "U+005B", "5b7b", "1011011"} {
				if strings.Contains(s, leak) {
					t.Errorf("%%%s of %T shows the bytes: %s", verb, arg, s)
				}
			}
		}
	}
	if o.Len() != len(src) || (Opaque{}).Len() != 0 {
		t.Errorf("Len = %d, want %d", o.Len(), len(src))
	}
	got := o.Bytes()
	if !strings.HasPrefix(string(got), `[{"type"`) || !strings.Contains(string(got), mark) {
		t.Fatalf("Bytes = %s", got)
	}
	got[0] = 'y'
	if o.Bytes()[0] != '[' {
		t.Error("a change to the copy reached the Opaque")
	}
	if !(Opaque{}).IsZero() || !NewOpaque(nil).IsZero() || o.IsZero() || (Opaque{}).String() != "" {
		t.Error("IsZero or String of the empty value")
	}
}
