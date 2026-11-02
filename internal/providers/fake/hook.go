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

package fake

import "sync"

// Hook injects errors. Fail is called with the operation name (for example "OpenChangeRequest") before the
// operation runs; a non-nil result is returned instead of running it. After, when set, is called with the
// operation name once the operation has succeeded, and its error is returned although the change was made,
// which simulates a crash or timeout between the remote call and the caller seeing the result.
type Hook struct {
	mu    sync.Mutex
	Fail  func(op string) error
	After func(op string) error
}

func (h *Hook) before(op string) error {
	h.mu.Lock()
	f := h.Fail
	h.mu.Unlock()
	if f == nil {
		return nil
	}
	return f(op)
}

func (h *Hook) after(op string) error {
	h.mu.Lock()
	f := h.After
	h.mu.Unlock()
	if f == nil {
		return nil
	}
	return f(op)
}

// SetFail replaces the Fail function safely while the fake is in use.
func (h *Hook) SetFail(f func(op string) error) {
	h.mu.Lock()
	h.Fail = f
	h.mu.Unlock()
}

// SetAfter replaces the After function safely while the fake is in use.
func (h *Hook) SetAfter(f func(op string) error) {
	h.mu.Lock()
	h.After = f
	h.mu.Unlock()
}
