# ADR-0035: The render check runs in a child process with operating system limits

- Status: Proposed
- Date: 2026-11-14

## Context

The render check (design 8.2.2, `internal/gitops/semdiff`) builds the Kustomization path of a change twice with the kustomize API and compares the outputs. Until now it ran in the executor's own process. Git contents are untrusted (invariant 4), and kustomize grows its output from small input: a value that a transformer copies into every place it reaches, a patch merged into a wide mapping, a name copied into every reference.

Each growth found so far got a static bound on the input, checked before the render: the build bytes and documents (#482, #488), the refusal of anchors and JSON patch `copy` (#488), what patches and transformers can add (`MaxTransformBytes`, #495, #626) and the keys patches can merge into one mapping (`MaxMergedKeys`, #626). A review on 2026-11-09 found a fifth case ([#614](https://github.com/pkalin/shadewright/issues/614)): a base adds a 64 KiB annotation, an overlay moves it into a ConfigMap name with a JSON patch, and kustomize copies the new name into 1,000 references. About 100 KB of input is within every budget; the render reached 66 MB of output and 374 MiB of Go heap in 5.6 s (measured 2026-11-14 in the test process, Go 1.27.4, arm64), and the check passed. In the executor's container (256 Mi from #615 until this decision) such a tree ends the process on every recheck. #626 listed more cases past its estimate (keys merged under sibling paths, labels added across overlays, patches wrapped in nested `List` documents, JSON 6902 moves, a list created by a JSON `add` and then merged).

The estimates only over-count, and each round made them more complex without closing the class. A render cannot be cancelled once it has started, and a Go process cannot bound the memory of one call. On 2026-11-10 the static estimates were stopped there, and bounding the render itself was chosen for #614 ("child-process render with an OS memory and CPU limit, needs an ADR").

## Decision

`semdiff.Check` validates its input in the executor, then runs the walk, both renders and the comparison in a child process: the executor's own binary, started again with one fixed argument (`semdiff.RunRenderProcess`, called first in `main`). Nothing else changes in what the check proves.

1. The child sets its limits before it reads any input: a CPU time limit (`RLIMIT_CPU`, `semdiff.MaxRenderCPUSeconds`, 20 s; a `SIGXCPU` at the soft limit makes it exit, a `SIGKILL` from the kernel 5 s later if it did not) and, on Linux, a data limit (`RLIMIT_DATA`, `semdiff.MaxRenderData`, 512 MiB of private writable mappings, the soft and hard values equal so the child cannot raise them). It then samples the memory its Go runtime holds every millisecond and exits past `semdiff.MaxRenderMemory` (128 MiB), with Go's soft memory limit at three quarters of that so the collector works harder first. The data limit counts address space (the child starts with about 135 MiB of it), so it sits above what the sampling allows and stops only a burst the sampling misses.
2. On Linux the child also raises its own OOM score adjustment to 1000 before it reads any input (raising it needs no privilege), so that when the container runs out of memory the kernel's OOM killer picks the child and not the executor. A failed write is not fatal and is not reported: the child has no one to tell, since the parent reads its stderr only for the Go runtime's fatal error line, and the render then runs with its other limits.
3. The parent writes the input as JSON to the child's stdin and reads one answer from its stdout: a result, a refusal with its reason and detail, or a failure. The answer is read up to 64 KiB. A refusal is taken only with one of the package's own reasons (any other is an error for a retry), and its detail is cut at 2 KiB, room for two repository paths of 512 bytes; the reason and detail then go where they went before (the request's condition shows only the reason). The child gets an empty environment. Go code in the child writes nothing to stderr (`os.Stderr` and the `log` package point at nothing), and the parent keeps only the first line of it that starts with `fatal error: `, which the Go runtime writes when it ends the process.
4. The child writes a ready line once its limits are set. Only the ends the child chose are refusals, with `TooLarge` and a message that names the limit: its own exit at the memory sampling ("the render needs more than 128 MiB of memory"), its own exit at the CPU time limit ("more than 20 s of CPU time"), and the Go runtime's exit when the data limit refused a heap mapping (exit code 2 with a fatal error line about running out of memory: "more than 512 MiB of data mappings"). The same build ends the same way on every retry, so a refusal is the right end and the person reads it on the request. Every other end is an error for a retry: a child that could not be started, ended before its ready line, could not read its input or answered nothing readable; a child killed by a signal, such as a `SIGKILL` from the container's OOM killer while the executor held its memory, which says nothing about the build; and any other crash. A request whose check keeps failing that way ends at its deadline, as other retried errors do. When the caller's context ends the child is killed and `Check` returns the context's error, also for a retry: a node starved of CPU must not turn into a refusal.
5. The CPU time limit is reached inside the step that runs the check. The pull request step bounds the load of the build, the wait for the render slot and the render by one context of 2 minutes (`controller.DefaultWriteTimeout`). At the executor's CPU limit in the chart (500m, shared by the executor and the child) 20 s of CPU time take 40 s of wall time, so even with one render at the limit ahead of it in the slot the refusal arrives before the step's deadline (`TestRenderCPURefusalWithinTheStep`, computed from the chart's value). The largest builds the input budgets allow used at most 3.9 s of CPU time (`hack/load/render` at 500m CPU and 384 MiB, Linux arm64, 2026-11-14: `heavy 4095` 3.9 s, `docs 4095` 3.5 s, `dense 100` 3.1 s, `patches 1708` 2.8 s, `flat 4` 1.3 s), so the limit leaves five times that. With 60 s, as first proposed, the limit took 120 s of wall time, the step's deadline came first, and the request was retried until its own deadline without the refusal.
6. The executor runs one render process at a time, as it ran one render at a time before, so its memory is its own plus one child's. The container's memory limit covers both: the chart raises the executor's limit from 256 Mi to 384 Mi (request unchanged) and sets its `GOMEMLIMIT` to 60 percent of the limit instead of 90, which keeps the executor's Go soft limit at 241591910 bytes (about 230 MiB), as before. 230 MiB for the executor plus 128 MiB of Go memory for the child at its limit is 358 MiB, which leaves 26 MiB for the memory of the two processes outside their Go runtimes (mostly the binary's pages). The child's peak resident memory, measured when the sampling stopped the #614 case, was 125 MiB (test binary, Linux arm64), and 97 MiB at the largest build the budgets allow (`dense 100`).
7. The input budgets are not removed or widened: they refuse most large builds early, with a precise message, at the policy check before any plan is made. The process limits bound what they miss.

[![The executor's container, 384Mi and 500m CPU, holds the executor and one render process. The executor validates the input, waits for the render slot and starts its own binary again with one fixed argument and an empty environment. The render process sets 20 s of CPU time, 512 MiB of data mappings, 128 MiB of Go memory and an OOM score adjustment of 1000 before it reads its input on stdin, then answers on stdout. Three ends: an answer (a result, or a refusal with a known reason and a detail cut at 2 KiB); a TooLarge refusal for the ends the process chose (its memory and CPU exits, the Go runtime's out of memory at the data limit); an error for a retry for everything else (no ready line, a signal such as the container's OOM killer, another crash, an unreadable answer, the step's context ending). The two processes share the user, file system and network.](../assets/diagrams/render-process.svg)](../assets/diagrams/render-process.svg "Open full size")

```mermaid
sequenceDiagram
    participant E as Executor (semdiff.Check)
    participant C as Render process (same binary)
    E->>E: Validate the input, wait for the one render slot
    E->>C: Start with the fixed argument, empty environment
    C->>C: Set RLIMIT_CPU and RLIMIT_DATA, start the memory sampling
    C-->>E: Ready line
    E->>C: Input as JSON on stdin
    C->>C: Walk, render before and after, compare
    alt Answer
        C-->>E: Result or refusal as JSON, exit 0
        E->>E: Take a refusal only with a known reason, cut its detail
    else The child stops at its own limit
        C-->>E: Exit at the memory sampling or the CPU limit, or the runtime's out of memory exit
        E->>E: Refuse with TooLarge
    else Killed by a signal, another crash, or the context ends
        C-->>E: Exit without an answer
        E->>E: Error for a retry
    end
```

## Consequences

- A build that grows past the estimates is refused with `TooLarge` instead of ending the executor. The #614 case is refused in about 1 s (`TestCheckBoundsAChainedNameReference`), with the executor's own heap under 64 MiB, and an ordinary chain of the same shape renders (`TestCheckRendersAnOrdinaryChain`).
- A render can now be cancelled: the process is killed when the context ends.
- Each check starts one process and copies the build's files through a pipe, at most a few MiB.
- Every binary that calls `Check` must call `semdiff.RunRenderProcess` first in `main`, test binaries from `TestMain`. A binary that does not fails its flag parsing on the fixed argument and exits before the ready line, so the check fails with an error for a retry; it never starts the binary as itself and never passes.
- The executor's container needs 384 Mi instead of 256 Mi (a chart value users see), with the executor's `GOMEMLIMIT` at 60 percent of the limit. A user who sets `executor.goMemLimit` or lowers the limit has to leave room for one render process.
- The memory bound is Linux's `RLIMIT_DATA` plus the sampling. macOS, used for development only, refuses a small data limit, so there the sampling is the only memory bound. Other platforms refuse to render. A binary built with the race detector (tests only) sets no data limit, since its shadow memory takes terabytes of address space, allows eight times the CPU time and gives the child `GORACE=atexit_sleep_ms=0`, so that it does not sleep a second before it exits.
- The executor's identity, RBAC and image are unchanged. The child is the same binary with no environment, no token and no network use; it reads only its stdin.
- Residual risk: the child runs in the same container as the executor, as the same user, with the same file system and network. The limits bound its memory and CPU time, not what a render exploited through a flaw in kustomize or YAML parsing could read or reach: the tokens mounted in the Pod (the executor's service account token, and its workload identity token when it has one) and the network the executor may use. The empty environment keeps the executor's environment from it, and nothing in the child's normal path opens a file or a connection; a sandbox (another user, a seccomp profile of its own, no network) would need privileges the executor does not have and is not part of this decision.
- Invariant 4 holds more strongly: the cost of untrusted Git content is bounded by the process, not by an estimate of it. Invariant 3 is unchanged: a refusal commits nothing.

## Alternatives considered

- **Another estimate round** (a compositional estimate: any value an earlier stage could create counts at its largest size for later stages). It would refuse the #614 case, but every round so far left a gap a review found, and the estimate already over-counts honest trees. Rejected on 2026-11-10.
- **A memory limit inside the executor's process** (Go's soft memory limit, or a watchdog that stops the render). The soft limit only makes the collector work harder, and kustomize cannot be stopped mid-render, so a watchdog could only end the whole executor.
- **A separate render binary in the image.** The same isolation, with a second binary to build, sign, ship and find at run time. Starting the executor's own binary needs none of that.
- **A cgroup per render.** A tighter memory bound (resident memory rather than address space), but it needs write access to the cgroup tree, which the non-root executor with all capabilities dropped does not have.
- **`RLIMIT_AS`.** The Go runtime reserves large ranges of address space it never uses, so a useful address space limit would refuse honest renders.

## Amendments

None.
