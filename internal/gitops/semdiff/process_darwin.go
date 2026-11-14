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

// setLimits sets the render process's CPU time limit (RLIMIT_CPU, a SIGXCPU at the soft limit, which
// [catchCPU] turns into an exit) before it reads any input. macOS, used for
// development only, refuses a data limit below what a process has reserved and does not enforce one on
// mappings, so there the memory watchdog ([watchMemory]) is the only memory bound; the executor runs on Linux
// (process_linux.go).
func setLimits(_, cpuSeconds uint64) error {
	return syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: cpuSeconds, Max: cpuSeconds + cpuGrace})
}

// noDataLimit is unused here: no data limit is set on macOS.
const noDataLimit = 0
