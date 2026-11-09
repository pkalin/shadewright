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

// Package deadline runs a step of a test in a child process that is killed when it runs too long, so a test of
// a computation that can hang fails quickly and leaves no runaway goroutine behind. It is for tests only.
package deadline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// childEnv names the test a child process runs fn for.
const childEnv = "SHADEWRIGHT_DEADLINE_CHILD"

// Within runs fn in a child process of the test binary and fails t when the child does not end within limit,
// or when fn fails there. fn gets the child's own *testing.T and must not depend on state the parent set up
// before calling Within. In the child, Within runs fn and returns; the rest of the test runs there as well,
// so call Within once per test, at its end or in its own subtest.
func Within(t *testing.T, limit time.Duration, fn func(t *testing.T)) {
	t.Helper()
	if os.Getenv(childEnv) == t.Name() {
		fn(t)
		return
	}
	parts := strings.Split(t.Name(), "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	// #nosec G204 G702 -- the test binary itself, with a -test.run pattern built from the test's own name.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+strings.Join(parts, "/"), "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("did not return within %s", limit)
	}
	if err != nil {
		t.Fatalf("failed in the child process: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()+" ") {
		t.Fatalf("the child process did not run the test:\n%s", out)
	}
}
