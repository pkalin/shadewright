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

// Option configures a redactor created by [New]. An extra [Pattern] is also an option.
type Option interface {
	apply(*options)
}

type options struct {
	patterns []Pattern
	hooks    Hooks
}

func (p Pattern) apply(opts *options) { opts.patterns = append(opts.patterns, p) }

// Hooks report redaction decisions without access to the entry. Callbacks run synchronously and must
// return promptly and be safe for concurrent calls. Nil callbacks do nothing.
type Hooks struct {
	// Withheld receives one of ReasonWork, ReasonSize, ReasonHeap or ReasonWait.
	Withheld func(reason string)
	// HeapWait runs when an entry cannot immediately acquire a share of the heap limit.
	HeapWait func()
}

func (h Hooks) apply(opts *options) { opts.hooks = h }

func (h Hooks) withheld(reason string) {
	if h.Withheld != nil {
		h.Withheld(reason)
	}
}

// SetDefaultHooks installs hooks for package-level decisions and a callback for the process-wide heap
// limit when it first falls back to a fixed size. Call it during program initialization, before any redaction.
func SetDefaultHooks(h Hooks, heapFallback func(limitBytes int64)) {
	defaultRedactor.hooks = h
	redactionHeap.onFallback = heapFallback
}
