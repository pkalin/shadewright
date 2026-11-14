//go:build linux || darwin

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
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// catchCPU makes the render process exit with exitCPU on SIGXCPU, which the Go runtime otherwise ignores.
func catchCPU() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGXCPU)
	go func() {
		<-ch
		os.Exit(exitCPU)
	}()
}

// outOfCPU reports whether the render process was ended by SIGXCPU itself, before catchCPU was set.
func outOfCPU(err *exec.ExitError) bool {
	ws, ok := err.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGXCPU
}
