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
	"log"
	"os"
	"os/exec"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync/atomic"
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
	// budgets), so this leaves room for honest builds.
	MaxRenderMemory = 128 << 20
	// MaxRenderResident is the resident memory the render process may hold (Linux, /proc/<pid>/statm). The
	// parent reads it every millisecond while it waits and kills the process past it, which is a refusal:
	// a second sampler, in another process, for a render whose Go memory outruns the process's own sampling. The
	// executor's render process held at most 145 MiB on builds that pass and 179 MiB when its own sampling
	// stopped it at [MaxRenderMemory] (ADR-0035, Linux arm64; the binary's text is about 44 MiB of it). The
	// executor's memory limit holds its Go soft limit, this and [RenderOutsideData] (a chart test checks the
	// sum), so on cgroup v2, where a container OOM ends the executor too, a render the samplers stop does not
	// reach it. Both are samplers, not a kernel guarantee: a burst between two samples can pass it.
	MaxRenderResident = 192 << 20
	// RenderOutsideData is the room the executor's memory limit keeps besides the executor's Go soft limit and
	// [MaxRenderResident]: memory the Go soft limit does not count (the executor's binary text, stacks, pipe
	// buffers, page tables) and what a render can add between two samples. On Linux arm64 the parent's kill
	// came 1 to 48 MiB over [MaxRenderResident], and the render grew 16 to 64 MiB between two readings
	// (ADR-0035); this is twice the largest growth.
	RenderOutsideData = 128 << 20
	// MaxRenderData is the data limit of the render process (RLIMIT_DATA, Linux, what /proc shows as VmData).
	// It is a partial guard, not a bound: the kernel checks it on a fresh writable mapping, but the Go runtime
	// maps its heap over address space it reserved earlier (mmap with MAP_FIXED over a PROT_NONE reservation),
	// which the kernel does not check, so the heap can grow past it. The process then ends at its next fresh
	// mapping, where the Go runtime writes "out of memory" or "cannot allocate memory" or crashes with a SIGSEGV
	// in its collector. Neither end proves this limit (Linux refuses a mapping with the same error when the node
	// is short of memory to commit), so both are a [RenderEnded] for a retry (ADR-0035). The executor's render
	// process started with 83 MiB of it and reached 245 MiB when its own sampling stopped it; this is 1.5
	// times that.
	MaxRenderData = 384 << 20
	// MaxRenderCPUSeconds is the CPU time the render process may use (RLIMIT_CPU, every platform). The largest
	// check the input budgets allow used 3.9 s of CPU time (hack/load/render, Linux arm64, ADR-0035), so this
	// leaves five times that. At the executor's CPU limit in the chart (500m) it takes 40 s of wall time to
	// reach, well inside the two minutes of the pull request step (controller.DefaultWriteTimeout), so the
	// refusal arrives before the step's own deadline.
	MaxRenderCPUSeconds = 20
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

// maxRenderDetail bounds the detail of a refusal the render process answers: room for two repository paths of
// repopath.MaxLen bytes and the text around them. A longer detail is cut there.
const maxRenderDetail = 2 << 10

// maxFatalLine bounds the line of the render process's stderr that [fatalLine] keeps.
const maxFatalLine = 256

// exitRuntimeFatal is the Go runtime's exit code for a fatal error, such as a heap mapping the data limit
// refused ("fatal error: out of memory").
const exitRuntimeFatal = 2

// Exit codes of the render process, besides 0 (an answer was written).
const (
	exitMemory = 3 // the memory watchdog stopped the render
	exitFailed = 4 // the process could not set its limits or read its input
	exitCPU    = 5 // the process reached its CPU time limit (SIGXCPU)
)

// oomScoreAdj is the OOM score adjustment the render process sets on Linux ([preferOOMKill]): the most, so
// that the kernel's OOM killer picks the render process before the executor where it kills one process: on
// cgroup v1 and on cgroup v2 nodes whose kubelet sets singleProcessOOMKill. On other cgroup v2 nodes (the
// kubelet's default since Kubernetes 1.28) a container OOM kills every process in the container, and the
// render process's limits and the executor's memory limit ([MaxRenderResident]) are what keep the render process
// from causing one.
const oomScoreAdj = "1000"

// sampleEvery is how often the render process samples the memory its Go runtime holds ([watchMemory]).
const sampleEvery = time.Millisecond

// residentEvery is how often the parent reads the resident memory of the render process ([watchResident]).
const residentEvery = time.Millisecond

// sampleMemory turns the render process's own memory sampling ([watchMemory]) on; tests turn it off to stand
// in for a render that outruns it.
var sampleMemory = true

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

// RenderEnded is an end of a render process that had set its limits and was neither an answer nor one of
// its limit refusals: a kill the parent did not make (such as an OOM killer), a crash, the Go runtime's out of
// memory exit, a failure to read its input, or an answer that cannot be read. [Check] returns it for a retry.
// The same build may end this way on every retry, so the caller counts these ends in a row and refuses the
// change after a few (controller.maxRenderEnds, ADR-0035); a context that ended is never one.
type RenderEnded struct {
	// Cause says how the process ended, in fixed text, for example "crashed before it answered".
	Cause string
	// Err is the error behind it, if any: the wait status of the process.
	Err error
}

func (e *RenderEnded) Error() string {
	if e.Err == nil {
		return "semdiff: the render process " + e.Cause
	}
	return "semdiff: the render process " + e.Cause + ": " + e.Err.Error()
}

func (e *RenderEnded) Unwrap() error { return e.Err }

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
	if err := limitRenderProcess(); err != nil {
		return exitFailed
	}
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

// limitRenderProcess sets the limits of the render process and starts its memory sampling.
func limitRenderProcess() error {
	data, cpu := uint64(MaxRenderData), uint64(MaxRenderCPUSeconds)
	if raceBuild {
		data, cpu = noDataLimit, 8*cpu
	}
	catchCPU()
	if err := setLimits(data, cpu); err != nil {
		return err
	}
	preferOOMKill()
	quietStderr()
	debug.SetMemoryLimit(MaxRenderMemory * 3 / 4)
	if sampleMemory {
		go watchMemory(MaxRenderMemory)
	}
	return nil
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

// quietStderr points the standard error of Go code in the render process (os.Stderr and the log package) at
// nothing, so that what the parent reads from the process's stderr ([fatalLine]) is written by the Go runtime
// alone, which writes to the file descriptor itself. A failure leaves stderr as it was.
func quietStderr() {
	log.SetOutput(io.Discard)
	if f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stderr = f
	}
}

// watchMemory ends the process with exitMemory when the memory its Go runtime holds (mapped, less what it has
// returned to the system) passes limit. It samples every millisecond, so it can overshoot; the parent's reading
// of the process's resident memory ([MaxRenderResident]) is the second bound.
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

// fatalLine keeps the first line of the render process's stderr that starts with "fatal error: ", which is
// the line the Go runtime writes first when it ends the process, up to maxFatalLine bytes, and drops the rest.
type fatalLine struct {
	line  []byte
	found string
}

func (f *fatalLine) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && f.found == "" {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		f.line = append(f.line, part[:min(len(part), maxFatalLine-len(f.line))]...)
		if i < 0 {
			break
		}
		if bytes.HasPrefix(f.line, []byte("fatal error: ")) {
			f.found = string(f.line)
		}
		f.line = f.line[:0]
		p = p[i+1:]
	}
	return n, nil
}

// runtimeOutOfMemory reports whether line, the first fatal error line of a render process, is the Go
// runtime's own end at a refused mapping: "fatal error: out of memory", "fatal error: runtime: out of memory"
// (a heap mapping) or "fatal error: runtime: cannot allocate memory" (its own metadata), the texts Go 1.27
// writes at the data limit, and also when the node is short of memory to commit.
func runtimeOutOfMemory(line string) bool {
	msg, ok := strings.CutPrefix(line, "fatal error: ")
	return ok && (strings.Contains(msg, "out of memory") || strings.Contains(msg, "cannot allocate memory"))
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
	fatal := &fatalLine{}
	cmd.Stderr = fatal
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("%w: %w", errNotStarted, err)
	}
	stop, hit := watchResident(cmd.Process, residentLimit())
	runErr := cmd.Wait()
	stop()
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
	// The parent's kill is the end only when the process ended by a SIGKILL after it: a process that crashed
	// just before the kill reached it is read by its own end.
	if hit() && killedBySIGKILL(runErr) {
		return Result{}, refuse(ReasonTooLarge,
			fmt.Sprintf("the render needs more than %d MiB of resident memory", residentLimit()>>20))
	}
	if runErr != nil {
		return Result{}, stopped(runErr, fatal.found)
	}
	return decodeAnswer(answer, out.over)
}

// residentLimit is [MaxRenderResident], or four times it in a race build, whose shadow memory is resident too.
func residentLimit() uint64 {
	if raceBuild {
		return 4 * MaxRenderResident
	}
	return MaxRenderResident
}

// residentKill kills the render process p, which the parent read at n bytes resident, prev at the reading
// before. Tests replace it to see the readings or to stand in for a process that ended by itself first.
var residentKill = func(p *os.Process, _, _ uint64) error { return p.Kill() }

// watchResident reads the resident memory of the render process p every residentEvery and kills it past limit
// (Linux only; elsewhere [residentBytes] reads nothing). stop ends the watch and waits for it; hit reports
// whether the watch sent the kill to a process not reaped yet. That alone does not say the kill ended it: the
// caller also needs its wait status. A failed read is neither a refusal nor an error: the process may have
// ended, and its end is read as usual.
func watchResident(p *os.Process, limit uint64) (stop func(), hit func() bool) {
	var sent atomic.Bool
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTicker(residentEvery)
		defer t.Stop()
		var prev uint64
		for {
			n, ok := residentBytes(p.Pid)
			if ok && n > limit {
				if residentKill(p, prev, n) == nil {
					sent.Store(true)
				}
				return
			}
			if ok {
				prev = n
			}
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return func() { close(done); <-ended }, sent.Load
}

// stopped reads the end of a render process that had set its limits, wrote no answer and was not killed by
// the parent at its resident limit (a refusal, read in [runRenderProcess]). Only the ends the process chose
// itself at a limit are refusals: its own exits at the memory sampling and at the CPU time limit. The same build
// ends the same way on every retry. Every other end is a [RenderEnded] for a retry: a signal, such as a SIGKILL
// from an OOM killer, the Go runtime's out of memory exit (fatal, the first fatal error line of its stderr),
// which the data limit or a node short of memory can cause alike, or another crash, such as the Go runtime's
// SIGSEGV when its collector cannot map its work queue at the data limit.
func stopped(err error, fatal string) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("semdiff: wait for the render process: %w", err)
	}
	switch code := exitErr.ExitCode(); {
	case code == exitMemory:
		return refuse(ReasonTooLarge, fmt.Sprintf("the render needs more than %d MiB of memory", MaxRenderMemory>>20))
	case code == exitCPU:
		return refuse(ReasonTooLarge, fmt.Sprintf("the render needs more than %d s of CPU time", MaxRenderCPUSeconds))
	case code == exitRuntimeFatal && runtimeOutOfMemory(fatal):
		return &RenderEnded{Cause: "ran out of memory (the Go runtime's out of memory exit)", Err: err}
	case code == exitFailed:
		return &RenderEnded{Cause: "could not read its input or write its answer", Err: err}
	case code < 0:
		return &RenderEnded{Cause: "was killed before it answered", Err: err}
	}
	return &RenderEnded{Cause: "crashed before it answered", Err: err}
}

// decodeAnswer reads the answer of a render process that exited normally.
func decodeAnswer(data []byte, over bool) (Result, error) {
	var a renderAnswer
	if over || json.Unmarshal(data, &a) != nil {
		return Result{}, &RenderEnded{Cause: "wrote no readable answer"}
	}
	switch {
	case a.Result != nil:
		return *a.Result, nil
	case a.Reason != "":
		if !a.Reason.known() {
			return Result{}, &RenderEnded{Cause: "answered a reason the check does not give"}
		}
		return Result{}, refuse(a.Reason, capDetail(a.Detail))
	default:
		return Result{}, &RenderEnded{Cause: "failed without a refusal"}
	}
}

// capDetail cuts the detail of a refusal from the render process to maxRenderDetail bytes of valid UTF-8.
func capDetail(d string) string {
	if len(d) <= maxRenderDetail {
		return d
	}
	return strings.ToValidUTF8(d[:maxRenderDetail], "")
}
