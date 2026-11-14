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
	"io"
	"math"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// helperArg starts the test binary as a render process that misbehaves in the way its second argument names
// ([runHelper]), so the tests can check how [runRenderProcess] reads each kind of end.
const helperArg = "-semdiff-test-helper"

// TestMain lets the test binary serve as the render process of Check ([RunRenderProcess]) and as the helper
// processes of the tests below. Check renders in this process ([RenderInProcessForTests]), except in the tests
// of the render process, which call [UseRenderProcess] or [runRenderProcess] itself (#717).
func TestMain(m *testing.M) {
	RunRenderProcess()
	if len(os.Args) > 2 && os.Args[1] == helperArg {
		os.Exit(runHelper(os.Args[2]))
	}
	if err := RenderInProcessForTests(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// UseRenderProcess makes Check render in a render process until the test ends, for the tests of the package
// semdiff_test that are about it. The tests of this package do not run in parallel.
func UseRenderProcess(t *testing.T) {
	t.Helper()
	inProcess.Store(false)
	t.Cleanup(func() { inProcess.Store(true) })
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
	case "crash": // a fatal error of the Go runtime other than a refused mapping
		ready()
		_, _ = os.Stderr.WriteString("fatal error: stack overflow\n\nruntime stack:\n")
		return exitRuntimeFatal
	case "oommeta": // the Go runtime's end when a mapping of its own metadata is refused
		ready()
		_, _ = os.Stderr.WriteString("fatal error: runtime: cannot allocate memory\n\nruntime stack:\n")
		return exitRuntimeFatal
	case "segv": // the Go runtime's crash when its collector cannot map its work queue at the data limit
		ready()
		_, _ = os.Stderr.WriteString("SIGSEGV: segmentation violation\nPC=0x41b48 m=2 sigcode=1 addr=0x0\n")
		return exitRuntimeFatal
	case "oom": // the Go runtime's end at a heap mapping the data limit refused, after some other output
		ready()
		_, _ = os.Stderr.WriteString("a line\nruntime: out of memory: cannot allocate 4194304-byte block (536870912 in use)\n" +
			"fatal error: out of memory\n\ngoroutine 1 gp=0x1 m=0 mp=0x2 [running]:\nfatal error: stack overflow\n")
		return exitRuntimeFatal
	case "killed": // a SIGKILL the process did not ask for, such as the container's OOM killer
		ready()
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Kill()
		}
		time.Sleep(time.Minute)
		return 0
	case "reason": // a refusal with a reason the check does not give
		ready()
		_, _ = os.Stdout.WriteString(`{"reason":"Unheard","detail":"x"}` + "\n")
		return 0
	case "detail": // a refusal with a detail far longer than any the check writes
		ready()
		_, _ = fmt.Fprintf(os.Stdout, `{"reason":"TooLarge","detail":"%s"}`+"\n", strings.Repeat("d", 16<<10))
		return 0
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
	case "resident": // the real limits without the process's own memory sampling, then memory touched until killed
		sampleMemory = false
		if limitRenderProcess() != nil {
			return exitFailed
		}
		debug.SetMemoryLimit(math.MaxInt64) // no collector working harder near the soft limit
		ready()
		for range 1024 { // 4 GiB, held, at most
			sink = append(sink, bytes.Repeat([]byte{1}, 4<<20))
		}
		return 0
	case "residentcrash": // the real limits without its own sampling, past the resident limit, then a crash of its own
		sampleMemory = false
		if limitRenderProcess() != nil {
			return exitFailed
		}
		debug.SetMemoryLimit(math.MaxInt64)
		ready()
		for range 1024 {
			if n, ok := residentBytes(os.Getpid()); ok && n > residentLimit()+16<<20 {
				break
			}
			sink = append(sink, bytes.Repeat([]byte{1}, 4<<20))
		}
		time.Sleep(200 * time.Millisecond) // the parent reads it over the limit meanwhile
		return exitRuntimeFatal
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
// limits is an error for a retry, never a refusal; one that set them and stopped at a limit it chose (the
// memory sampling, the CPU time) is a TooLarge refusal that says why; one killed by a signal, the Go runtime's
// out of memory exit (the data limit or a node short of memory alike), one that crashed otherwise, one that
// cannot read its input and one that answers nothing readable or a reason the check does not give are a
// RenderEnded for a retry, which the executor counts.
func TestRenderProcessEnds(t *testing.T) {
	for mode, c := range map[string]struct {
		refusal string // a TooLarge refusal naming this, or "" for an error for a retry
		retry   error  // the error it wraps, when it is not a RenderEnded
		says    string // what the error for a retry says
	}{
		"silent":  {retry: errNotStarted},
		"memory":  {refusal: "more than 128 MiB of memory"},
		"oom":     {says: "ran out of memory"},
		"cpu":     {refusal: "more than 20 s of CPU time"},
		"oommeta": {says: "ran out of memory"},
		"crash":   {says: "crashed"},
		"segv":    {says: "crashed"},
		"killed":  {says: "was killed"},
		"failed":  {says: "could not read its input"},
		"garbage": {says: "no readable answer"},
		"reason":  {says: "a reason the check does not give"},
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
				if _, ended := errors.AsType[*RenderEnded](err); ended != (c.retry == nil) {
					t.Fatalf("got %v (a RenderEnded: %t), want one: %t", err, ended, c.retry == nil)
				}
				if !strings.Contains(err.Error(), c.says) {
					t.Fatalf("got %v, want an error that says %q", err, c.says)
				}
				return
			}
			if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), c.refusal) {
				t.Fatalf("got %v, want TooLarge naming %q", err, c.refusal)
			}
		})
	}
}

// TestRenderProcessDetail checks that the detail of a refusal from the render process is cut to
// maxRenderDetail bytes, whatever the process writes.
func TestRenderProcessDetail(t *testing.T) {
	withHelper(t, "detail")
	_, err := runRenderProcess(context.Background(), Input{})
	se, ok := errors.AsType[*Error](err)
	if !ok || se.Reason != ReasonTooLarge {
		t.Fatalf("got %v, want the TooLarge refusal", err)
	}
	if len(se.Detail) > maxRenderDetail {
		t.Fatalf("got a detail of %d bytes, want at most %d", len(se.Detail), maxRenderDetail)
	}
}

// TestReasonsKnown checks that every reason of the package is one the parent accepts from the render process.
func TestReasonsKnown(t *testing.T) {
	for _, e := range []*Error{ErrInvalidInput, ErrUnsupportedKustomization, ErrInvalidReference, ErrFileMissing,
		ErrTooLarge, ErrInvalidYAML, ErrRenderFailed, ErrFileNotInBuild, ErrTargetNotFound, ErrAmbiguousTarget,
		ErrObjectsAddedOrRemoved, ErrOtherObjectChanged, ErrOtherFieldChanged, ErrPreviousValueDiffers,
		ErrDesiredValueMissing, ErrFieldNotFound, ErrSourceIgnored, ErrSchemaInvalid} {
		if !e.Reason.known() {
			t.Errorf("reason %s is not known", e.Reason)
		}
	}
	if Reason("Unheard").known() {
		t.Error("an unknown reason is known")
	}
}

// TestRenderProcessOOMScore checks that the render process raises its own OOM score adjustment to the most
// before it reads its input, so that an OOM killer that ends one process picks it and not the executor (Linux
// only; on cgroup v2 the kubelet's default ends the whole container, see oomScoreAdj).
func TestRenderProcessOOMScore(t *testing.T) {
	if _, err := os.Stat("/proc/self/oom_score_adj"); err != nil {
		t.Skip("no OOM score here (Linux only)")
	}
	cmd, err := renderCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe() // held open, so the process waits for its input
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	line := make([]byte, len(renderReady))
	if _, err := io.ReadFull(stdout, line); err != nil || string(line) != renderReady {
		t.Fatalf("got %q, %v, want the ready line", line, err)
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != oomScoreAdj {
		t.Fatalf("the render process has an OOM score adjustment of %s, want %s", got, oomScoreAdj)
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
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitRuntimeFatal || !refused {
		t.Fatalf("got %v and %q, want exit 2 and a refused mapping", err, firstLine(stderr.String()))
	}
	if strings.Contains(stderr.String(), "pthread_create failed") {
		return // a cgo build; the executor is built without cgo
	}
	// The parent reads this end as the Go runtime's out of memory exit, for a retry: the same end comes from a
	// node short of memory to commit, so it does not prove the data limit.
	var fatal fatalLine
	_, _ = fatal.Write(stderr.Bytes())
	err = stopped(err, fatal.found)
	if _, ended := errors.AsType[*RenderEnded](err); !ended || !strings.Contains(err.Error(), "ran out of memory") {
		t.Fatalf("got %v from the first fatal line %q, want the out of memory end for a retry", err, fatal.found)
	}
}

// TestRenderProcessResident checks the parent's second memory bound (Linux only): a render process whose
// memory outruns its own sampling (here turned off, standing in for a burst between two samples) is killed by
// the parent past its resident limit, and the check is refused with TooLarge, not retried.
func TestRenderProcessResident(t *testing.T) {
	if _, ok := residentBytes(os.Getpid()); !ok {
		t.Skip("no resident memory to read here (Linux only)")
	}
	withHelper(t, "resident")
	var prev, at uint64
	orig := residentKill
	t.Cleanup(func() { residentKill = orig })
	residentKill = func(p *os.Process, before, n uint64) error {
		prev, at = before, n
		return orig(p, before, n)
	}
	start := time.Now()
	_, err := runRenderProcess(context.Background(), Input{})
	want := fmt.Sprintf("more than %d MiB of resident memory", residentLimit()>>20)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want TooLarge naming %q", err, want)
	}
	// The overshoot the margin of the executor's memory limit holds (RenderOutsideData, ADR-0035): how far over
	// the limit the reading that killed the process was, and how much it grew since the reading before.
	t.Logf("killed after %s at %d MiB resident: %d MiB over the limit, %d MiB more than the reading before",
		time.Since(start), at>>20, (at-residentLimit())>>20, (at-prev)>>20)
}

// TestRenderProcessCrashAfterResidentHit checks that the parent's kill at the resident limit is a refusal only
// when the process ended by that kill: a process the parent read over the limit that crashed by itself before
// the kill reached it (here the kill is not sent) is read by its own end, a crash for a retry.
func TestRenderProcessCrashAfterResidentHit(t *testing.T) {
	if _, ok := residentBytes(os.Getpid()); !ok {
		t.Skip("no resident memory to read here (Linux only)")
	}
	withHelper(t, "residentcrash")
	orig := residentKill
	t.Cleanup(func() { residentKill = orig })
	var hit atomic.Bool
	residentKill = func(*os.Process, uint64, uint64) error {
		hit.Store(true)
		return nil // as if the kill reached a process that had crashed already
	}
	_, err := runRenderProcess(context.Background(), Input{})
	if !hit.Load() {
		t.Fatal("the parent never read the process over its resident limit")
	}
	if _, ended := errors.AsType[*RenderEnded](err); !ended || !strings.Contains(err.Error(), "crashed") {
		t.Fatalf("got %v, want the crash for a retry, not the resident refusal", err)
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
