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
package semdiff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// inProcess makes [Check] render in this process instead of a render process (#717). Only a test binary turns
// it on ([RenderInProcessForTests]); every other binary renders in a render process, with its limits.
var inProcess atomic.Bool

// errNotATest is what [RenderInProcessForTests] returns in a binary that go test did not build.
var errNotATest = errors.New("semdiff: the render runs in this process only in a test binary")

// RenderInProcessForTests makes [Check] render in this process, without the render process and its limits, for
// the tests of code that calls Check but is not about the render process (#717): a race-built test binary
// started once per check costs more than the check. It changes nothing and returns an error in a binary that
// go test did not build (testing.Testing), so production binaries always render in a render process, and no
// file outside tests calls it (TestRenderInProcessOnlyFromTests). Call it from TestMain, before the tests run.
func RenderInProcessForTests() error {
	if !testing.Testing() {
		return errNotATest
	}
	inProcess.Store(true)
	return nil
}

// renderHere runs [check] on in in this process and reads its outcome as [runRenderProcess] reads the answer of
// a render process: the same answer, through the same JSON and [decodeAnswer], so a result or a refusal is the
// one a render process gives. Only the limits of the render process do not apply. ctx ends the wait for the
// render as it ends a render process: Check then returns ctx.Err().
func renderHere(ctx context.Context, in Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	data, err := json.Marshal(answerFor(ctx, in))
	if err != nil {
		return Result{}, fmt.Errorf("semdiff: encode the render answer: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// A render process writes the ready line, the answer and a newline into maxRenderAnswer bytes.
	return decodeAnswer(data, len(renderReady)+len(data)+1 > maxRenderAnswer)
}
