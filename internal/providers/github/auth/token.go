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

package auth

import (
	"fmt"
	"log/slog"
)

// Token is an installation token. It prints as a placeholder with every fmt verb and in slog, so it cannot
// reach a log line or an error by accident; the value is available only through [Token.Reveal].
//
// The value sits behind a pointer: when a Token is an unexported field of another struct, fmt and slog walk
// that struct without calling Token's methods, and they print a nested pointer as an address, never the
// text it points to.
type Token struct {
	value *secret
}

type secret struct{ s string }

func newToken(v string) Token { return Token{value: &secret{s: v}} }

// Reveal returns the token value, or "" for the zero Token. Use it only to set an Authorization header.
func (t Token) Reveal() string {
	if t.value == nil {
		return ""
	}
	return t.value.s
}

// String returns a placeholder.
func (Token) String() string { return "[redacted installation token]" }

// GoString returns a placeholder, for %#v.
func (Token) GoString() string { return "auth.Token{[redacted]}" }

// Format prints a placeholder for every verb.
func (t Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(t.String())) }

// LogValue returns a placeholder for slog.
func (Token) LogValue() slog.Value { return slog.StringValue("[redacted installation token]") }

// MarshalJSON returns a placeholder so the token is not serialized by accident.
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"[redacted installation token]"`), nil }

// MarshalText returns a placeholder.
func (Token) MarshalText() ([]byte, error) { return []byte(Token{}.String()), nil }
