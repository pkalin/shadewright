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
	"fmt"
	"os"
	"syscall"
)

// setLimits sets the render process's own limits before it reads any input: memory for data (RLIMIT_DATA,
// which Linux enforces on the private writable mappings the Go heap lives in, since Linux 4.7) and CPU time
// (RLIMIT_CPU: a SIGXCPU at the soft limit, which [catchCPU] turns into an exit, and a SIGKILL from the kernel
// at the hard limit, cpuGrace seconds later). The data limit's soft and hard values are the same, so the
// process cannot raise it again.
func setLimits(memory, cpuSeconds uint64) error {
	if err := syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: memory, Max: memory}); err != nil {
		return err
	}
	return syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: cpuSeconds, Max: cpuSeconds + cpuGrace})
}

// noDataLimit lifts the data limit, for a race build (raceBuild).
const noDataLimit = ^uint64(0) // RLIM_INFINITY

// preferOOMKill raises the render process's own OOM score adjustment to oomScoreAdj before it reads any input,
// which helps only where the OOM killer ends one process (see oomScoreAdj). Raising it needs no privilege. A
// failed write is not fatal: the process has no one to tell (its stderr is read only for the Go runtime's fatal
// error line), and the render runs with its other limits as before.
func preferOOMKill() {
	f, err := os.OpenFile("/proc/self/oom_score_adj", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	_, _ = f.WriteString(oomScoreAdj)
	_ = f.Close()
}

// residentBytes reads the resident memory of the process pid from /proc/<pid>/statm (its second field, in
// pages). It reports false when the file cannot be read or parsed, for example once the process has ended.
func residentBytes(pid int) (uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, false
	}
	var size, resident uint64
	if _, err := fmt.Sscan(string(b), &size, &resident); err != nil {
		return 0, false
	}
	return resident * uint64(os.Getpagesize()), true
}
