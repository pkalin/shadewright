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

package main

import "github.com/pkalin/shadewright/internal/termtext"

// Bounds of the text printed for one field, in runes. A longer value is cut and ends with "...".
const (
	maxName    = 253
	maxMessage = 512
)

// clean makes a string from the cluster or the model safe to print on a terminal (invariant 4), cut to
// maxRunes runes: see [termtext.Clean]. The OpenAI-compatible provider's credential scan skips escape
// sequences by the same grammar ([termtext.EscapeEnd]).
func clean(s string, maxRunes int) string { return termtext.Clean(s, maxRunes) }
