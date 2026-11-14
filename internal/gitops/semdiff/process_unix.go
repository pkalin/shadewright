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
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// killedBySIGKILL reports whether err, from the wait for a render process, says the process ended by a SIGKILL.
func killedBySIGKILL(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// catchCPU makes the render process exit with exitCPU on SIGXCPU, which the Go runtime otherwise ignores.
func catchCPU() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGXCPU)
	go func() {
		<-ch
		os.Exit(exitCPU)
	}()
}
