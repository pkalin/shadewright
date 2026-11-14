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
	"log/slog"
)

// Opaque is data an adapter returns with a reply and needs back, unchanged, on the assistant message that
// carries that reply's tool calls, for example the thinking blocks of the Anthropic API. Only the adapter that
// made it reads it; a caller copies it from Completion.ProviderBlocks to Message.ProviderBlocks and does
// nothing else with it. Its bytes are unexported and every printed or encoded form hides them, so it cannot
// reach a log line, an error, a trace or a stored record by accident. The zero value is empty.
type Opaque struct {
	b []byte
}

// NewOpaque returns an Opaque holding a copy of b.
func NewOpaque(b []byte) Opaque {
	if len(b) == 0 {
		return Opaque{}
	}
	return Opaque{b: bytes.Clone(b)}
}

// Bytes returns a copy of the data, for the adapter that made it.
func (o Opaque) Bytes() []byte { return bytes.Clone(o.b) }

// IsZero reports whether o holds nothing.
func (o Opaque) IsZero() bool { return len(o.b) == 0 }

// String hides the data.
func (o Opaque) String() string {
	if o.IsZero() {
		return ""
	}
	return "[opaque]"
}

// GoString hides the data from %#v.
func (o Opaque) GoString() string { return "providers.Opaque{" + o.String() + "}" }

// LogValue hides the data from slog.
func (o Opaque) LogValue() slog.Value { return slog.StringValue(o.String()) }

// MarshalJSON encodes nothing of the data: an Opaque is never stored.
func (Opaque) MarshalJSON() ([]byte, error) { return []byte("null"), nil }
