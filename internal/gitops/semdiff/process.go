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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"runtime/metrics"
	"time"
)

// Limits of the render process (#614, ADR-0035). The walk's budgets bound what a build can add to the render
// in most cases, but they are estimates: overlays that chain one transformer's output into another's input
// (a value moved into a name that kustomize then copies into every reference) grow the render past them. The
// render process makes the bound a property of the process instead of the input.
const (
	// MaxRenderMemory is the memory the Go runtime of the render process may hold (mapped, less what it has
	// returned to the system). The process samples it every millisecond and exits with a refusal past it, and
	// sets Go's soft memory limit to three quarters of it so that the collector works harder first. The render
	// check of the test suite peaks at 46 MiB (design 8.2.2 measured 45 to 78 MiB of Go heap at the input
	// budgets), so this leaves room for honest builds; with the executor's own memory it fits in the 256 Mi
	// limit of its container.
	MaxRenderMemory = 128 << 20
	// MaxRenderData is the hard memory limit the operating system enforces on the render process: its private
	// writable mappings (RLIMIT_DATA, Linux). It counts address space, not resident memory, and the process
	// starts with about 135 MiB of it (the binary's data and the Go runtime's own reservations); at the render
	// check's 46 MiB of Go memory it held 216 MiB. It is set above what [MaxRenderMemory] allows, so it stops
	// only a burst the sampling misses, and then the Go runtime ends the process ("out of memory").
	MaxRenderData = 512 << 20
	// MaxRenderCPUSeconds is the CPU time the render process may use (RLIMIT_CPU, every platform). A check at
	// the input budgets takes 1 to 15 s, depending on the host.
	MaxRenderCPUSeconds = 60
)

// cpuGrace is how long past MaxRenderCPUSeconds the kernel waits before it kills a render process that did not
// exit on SIGXCPU (the hard RLIMIT_CPU).
const cpuGrace = 5

// renderProcessArg is the first argument of the render process. It looks like a flag, so a binary that does
// not call [RunRenderProcess] first fails its flag parsing and exits instead of starting as itself.
const renderProcessArg = "-shadewright-render-process"

// renderReady is the first line the render process writes, once its limits are set and before it reads its
// input. Only a process that wrote it can end at a limit; any other early end is a failure to start.
const renderReady = "shadewright-render-process ready\n"

// maxRenderAnswer bounds what the parent reads from the render process: one [Result] or one refusal.
const maxRenderAnswer = 64 << 10

// Exit codes of the render process, besides 0 (an answer was written).
const (
	exitMemory = 3 // the memory watchdog stopped the render
	exitFailed = 4 // the process could not set its limits or read its input
	exitCPU    = 5 // the process reached its CPU time limit (SIGXCPU)
)

// sampleEvery is how often the render process samples the memory its Go runtime holds ([watchMemory]).
const sampleEvery = time.Millisecond

// renderAnswer is what the render process writes after renderReady: a result, a refusal or neither (Failed),
// for an error that is not a refusal.
type renderAnswer struct {
	Result *Result `json:"result,omitempty"`
	Reason Reason  `json:"reason,omitempty"`
	Detail string  `json:"detail,omitempty"`
	Failed bool    `json:"failed,omitempty"`
}

// errNotStarted is returned when the render process could not be started or ended before it set its limits.
var errNotStarted = errors.New("semdiff: the render process did not start")

// RunRenderProcess turns this process into the render process when it was started as one by [Check], and
// returns otherwise. Every binary that calls [Check], test binaries included (from TestMain), calls it first
// in main, before it parses flags or starts anything else. The render process sets its limits, reads one
// [Input] from stdin, writes its answer to stdout and exits.
func RunRenderProcess() {
	if len(os.Args) < 2 || os.Args[1] != renderProcessArg {
		return
	}
	os.Exit(serveRender(os.Stdin, os.Stdout))
}

// serveRender is the render process: limits first, then the untrusted input.
func serveRender(in io.Reader, out io.Writer) int {
	data, cpu := uint64(MaxRenderData), uint64(MaxRenderCPUSeconds)
	if raceBuild {
		data, cpu = noDataLimit, 8*cpu
	}
	catchCPU()
	if err := setLimits(data, cpu); err != nil {
		return exitFailed
	}
	debug.SetMemoryLimit(MaxRenderMemory * 3 / 4)
	go watchMemory(MaxRenderMemory)
	w := bufio.NewWriter(out)
	if _, err := w.WriteString(renderReady); err != nil || w.Flush() != nil {
		return exitFailed
	}
	var input Input
	if err := json.NewDecoder(in).Decode(&input); err != nil {
		return exitFailed
	}
	answer := answerFor(input)
	if err := json.NewEncoder(w).Encode(answer); err != nil || w.Flush() != nil {
		return exitFailed
	}
	return 0
}

// answerFor runs the check and turns its outcome into an answer; a panic is a render failure, as in [render].
func answerFor(in Input) (a renderAnswer) {
	defer func() {
		if recover() != nil {
			a = renderAnswer{Reason: ReasonRenderFailed, Detail: "kustomize could not build the path"}
		}
	}()
	res, err := check(context.Background(), in)
	if se, ok := errors.AsType[*Error](err); ok {
		return renderAnswer{Reason: se.Reason, Detail: se.Detail}
	}
	if err != nil {
		return renderAnswer{Failed: true}
	}
	return renderAnswer{Result: &res}
}

// watchMemory ends the process with exitMemory when the memory its Go runtime holds (mapped, less what it has
// returned to the system) passes limit. It samples every millisecond, so it can overshoot; the operating
// system's limit ([MaxRenderData]) is the hard bound.
func watchMemory(limit uint64) {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	for {
		metrics.Read(s)
		if s[0].Value.Uint64()-s[1].Value.Uint64() > limit {
			os.Exit(exitMemory)
		}
		time.Sleep(sampleEvery)
	}
}

// capped keeps the first max bytes written to it and drops the rest, so the render process never blocks on a
// full pipe; over records that something was dropped.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.over = true
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.buf.Write(p)
}

// renderCommand returns the command of the render process: this binary with [renderProcessArg]. Tests replace
// it to start a process that misbehaves.
var renderCommand = func(ctx context.Context) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// #nosec G204 -- the executable is this binary and the one argument a constant; the input goes to stdin.
	return exec.CommandContext(ctx, exe, renderProcessArg), nil
}

// runRenderProcess runs [check] on in in a render process, one at a time, and turns how it ended into a
// result, a refusal or an error for a retry.
func runRenderProcess(ctx context.Context, in Input) (Result, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return Result{}, fmt.Errorf("semdiff: encode the render input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	select {
	case renderSlot <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-renderSlot }()

	cmd, err := renderCommand(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", errNotStarted, err)
	}
	cmd.Env = []string{} // nothing of the executor's environment reaches the render process
	if raceBuild {
		// The race detector sleeps a second before a process exits, by default; a test suite starts hundreds.
		cmd.Env = []string{"GORACE=atexit_sleep_ms=0"}
	}
	cmd.Stdin = bytes.NewReader(payload)
	out := &capped{max: maxRenderAnswer}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	runErr := cmd.Run()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	answer, ready := bytes.CutPrefix(out.buf.Bytes(), []byte(renderReady))
	if !ready {
		if runErr == nil {
			runErr = errors.New("no ready line")
		}
		return Result{}, fmt.Errorf("%w: %w", errNotStarted, runErr)
	}
	if runErr != nil {
		return Result{}, stopped(runErr)
	}
	return decodeAnswer(answer, out.over)
}

// stopped turns the end of a render process that had set its limits and wrote no answer into a refusal: it
// ran out of memory or CPU time, or kustomize crashed it (a Go stack overflow is not recoverable), and the
// same build would end the same way on every retry.
func stopped(err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("semdiff: wait for the render process: %w", err)
	}
	switch {
	case exitErr.ExitCode() == exitFailed:
		return fmt.Errorf("semdiff: the render process could not read its input or write its answer: %w", err)
	case exitErr.ExitCode() == exitMemory:
		return refuse(ReasonTooLarge, fmt.Sprintf("the render needs more than %d MiB of memory", MaxRenderMemory>>20))
	case exitErr.ExitCode() == exitCPU, outOfCPU(exitErr):
		return refuse(ReasonTooLarge, fmt.Sprintf("the render needs more than %d s of CPU time", MaxRenderCPUSeconds))
	}
	return refuse(ReasonTooLarge, fmt.Sprintf(
		"the render process stopped before it answered: at its limits of %d MiB of memory, %d MiB of data or %d s of CPU time, or by a crash",
		MaxRenderMemory>>20, MaxRenderData>>20, MaxRenderCPUSeconds))
}

// decodeAnswer reads the answer of a render process that exited normally.
func decodeAnswer(data []byte, over bool) (Result, error) {
	var a renderAnswer
	if over || json.Unmarshal(data, &a) != nil {
		return Result{}, errors.New("semdiff: the render process wrote no readable answer")
	}
	switch {
	case a.Result != nil:
		return *a.Result, nil
	case a.Reason != "":
		return Result{}, refuse(a.Reason, a.Detail)
	default:
		return Result{}, errors.New("semdiff: the render process failed")
	}
}
