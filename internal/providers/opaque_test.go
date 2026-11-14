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
