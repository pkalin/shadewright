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
