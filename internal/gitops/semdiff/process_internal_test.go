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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// helperArg starts the test binary as a render process that misbehaves in the way its second argument names
// ([runHelper]), so the tests can check how [runRenderProcess] reads each kind of end.
const helperArg = "-semdiff-test-helper"

// TestMain lets the test binary serve as the render process of Check ([RunRenderProcess]) and as the helper
// processes of the tests below.
func TestMain(m *testing.M) {
	RunRenderProcess()
	if len(os.Args) > 2 && os.Args[1] == helperArg {
		os.Exit(runHelper(os.Args[2]))
	}
	os.Exit(m.Run())
}

// sink keeps the helper's allocations alive.
var sink [][]byte

func runHelper(mode string) int {
	ready := func() { _, _ = os.Stdout.WriteString(renderReady) }
	switch mode {
	case "silent": // ends before it set its limits, as a binary without RunRenderProcess would
		return 0
	case "memory":
		ready()
		return exitMemory
	case "crash": // the Go runtime's exit code for a fatal error, such as running out of memory
		ready()
		return 2
	case "failed":
		ready()
		return exitFailed
	case "garbage":
		ready()
		_, _ = os.Stdout.WriteString("not an answer\n")
		return 0
	case "sleep":
		ready()
		time.Sleep(time.Minute)
		return 0
	case "cpu": // the real CPU limit, at one second
		catchCPU()
		if setLimits(noDataLimit, 1) != nil {
			return exitFailed
		}
		ready()
		for x := 0; ; x++ {
			_ = x
		}
	case "data": // the real data limit, 64 MiB above what the process holds at the start (Linux only)
		if err := setLimits(vmData()+64<<20, MaxRenderCPUSeconds); err != nil {
			return exitFailed
		}
		ready()
		for range 256 { // 1 GiB, held
			sink = append(sink, bytes.Repeat([]byte{1}, 4<<20))
		}
		return 0
	}
	return exitFailed
}

// vmData returns the VmData of this process from /proc, or 0 where there is none.
func vmData() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(b)) {
		var kb uint64
		if _, err := fmt.Sscanf(line, "VmData: %d kB", &kb); err == nil {
			return kb << 10
		}
	}
	return 0
}

// withHelper makes runRenderProcess start the helper in mode until the test ends.
func withHelper(t *testing.T, mode string) {
	t.Helper()
	orig := renderCommand
	t.Cleanup(func() { renderCommand = orig })
	renderCommand = func(ctx context.Context) (*exec.Cmd, error) {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, exe, helperArg, mode), nil
	}
}

// TestRenderProcessEnds checks how each end of the render process is read (#614): one that never set its
// limits is an error for a retry, never a refusal; one that set them and stopped at a limit or crashed is a
// TooLarge refusal that says why; one that cannot read its input or answers nothing readable is an error.
func TestRenderProcessEnds(t *testing.T) {
	for mode, c := range map[string]struct {
		refusal string // a TooLarge refusal naming this, or "" for a plain error
		retry   error  // the error it wraps, when it is not a refusal
	}{
		"silent":  {retry: errNotStarted},
		"memory":  {refusal: "more than 128 MiB of memory"},
		"crash":   {refusal: "stopped before it answered"},
		"cpu":     {refusal: "more than 60 s of CPU time"},
		"failed":  {},
		"garbage": {},
	} {
		t.Run(mode, func(t *testing.T) {
			withHelper(t, mode)
			_, err := runRenderProcess(context.Background(), Input{})
			if err == nil {
				t.Fatal("got no error")
			}
			_, isRefusal := errors.AsType[*Error](err)
			if c.refusal == "" {
				if isRefusal {
					t.Fatalf("got the refusal %v, want an error for a retry", err)
				}
				if c.retry != nil && !errors.Is(err, c.retry) {
					t.Fatalf("got %v, want %v", err, c.retry)
				}
				return
			}
			if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), c.refusal) {
				t.Fatalf("got %v, want TooLarge naming %q", err, c.refusal)
			}
		})
	}
}

// TestRenderProcessCancelled checks that a render process is killed when the context ends, and the caller
// gets the context's error for a retry.
func TestRenderProcessCancelled(t *testing.T) {
	withHelper(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runRenderProcess(ctx, Input{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the cancelled render process ran %s", took)
	}
}

// TestRenderProcessDataLimit checks that the operating system ends a render process that maps more than its
// data limit (RLIMIT_DATA), whatever the Go runtime would allow: the mapping is refused and the Go runtime exits
// with its fatal error code.
func TestRenderProcessDataLimit(t *testing.T) {
	if vmData() == 0 {
		t.Skip("RLIMIT_DATA is enforced on Linux only")
	}
	if raceBuild {
		t.Skip("the race detector's shadow memory is not bounded by RLIMIT_DATA")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(exe, helperArg, "data")
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	// The Go runtime ends the process when a heap mapping is refused; a test binary built with cgo may first
	// fail to map a thread's stack, which the runtime ends the same way.
	refused := strings.Contains(stderr.String(), "out of memory") || strings.Contains(stderr.String(), "pthread_create failed")
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !refused {
		t.Fatalf("got %v and %q, want exit 2 and a refused mapping", err, firstLine(stderr.String()))
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// CountRenderProcesses counts the render processes Check starts until the test ends, for the tests of the
// package semdiff_test.
func CountRenderProcesses(t *testing.T) func() int {
	t.Helper()
	orig := renderCommand
	t.Cleanup(func() { renderCommand = orig })
	var n int
	renderCommand = func(ctx context.Context) (*exec.Cmd, error) {
		n++
		return orig(ctx)
	}
	return func() int { return n }
}
