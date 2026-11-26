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
	"fmt"
	"log/slog"
)

// Opaque is data an adapter returns with a reply and needs back, unchanged, on the assistant message that
// carries that reply's tool calls, for example the thinking blocks of the Anthropic API. Only the adapter that
// made it reads it; a caller copies it from Completion.ProviderBlocks to Message.ProviderBlocks and does
// nothing else with it. Its bytes are unexported and every printed or encoded form hides them, so it cannot
// reach a log line, an error, a trace or a stored record by accident. The zero value is empty. Compare two
// values with Bytes: reflect.DeepEqual never finds two non-empty ones equal, since the data sits behind a
// function value.
type Opaque struct {
	// d is a function value: fmt prints a function only as an address under every verb, also when it walks a
	// struct that holds the Opaque as an unexported field without calling its methods. A pointer would not do:
	// for a verb that does not fit a pointer (%s, %q), fmt's bad-verb form prints the target of the pointer.
	d func() []byte
}

// NewOpaque returns an Opaque holding a copy of b.
func NewOpaque(b []byte) Opaque {
	if len(b) == 0 {
		return Opaque{}
	}
	c := bytes.Clone(b)
	return Opaque{d: func() []byte { return c }}
}

func (o Opaque) data() []byte {
	if o.d == nil {
		return nil
	}
	return o.d()
}

// Bytes returns a copy of the data, for the adapter that made it.
func (o Opaque) Bytes() []byte { return bytes.Clone(o.data()) }

// Len returns the number of bytes held, for a caller that bounds the size of a request.
func (o Opaque) Len() int { return len(o.data()) }

// IsZero reports whether o holds nothing.
func (o Opaque) IsZero() bool { return o.Len() == 0 }

// String hides the data.
func (o Opaque) String() string {
	if o.IsZero() {
		return ""
	}
	return "[opaque]"
}

// Format prints the same placeholder as String for every verb and flag, %#v, %d, %x, %c, %U and the
// others included.
func (o Opaque) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(o.String())) }

// LogValue hides the data from slog.
func (o Opaque) LogValue() slog.Value { return slog.StringValue(o.String()) }

// MarshalJSON encodes nothing of the data: an Opaque is never stored.
func (Opaque) MarshalJSON() ([]byte, error) { return []byte("null"), nil }
