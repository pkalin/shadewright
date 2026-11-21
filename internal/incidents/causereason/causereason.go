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

// Package causereason holds the Incident reasons that more than one component reads the same way, so they keep
// one list: the collector, which decides which waiting reasons are a cause (ADR-0039 point 3), and the context
// builder, which shows the absorbed tally for an OOM kill and those waiting reasons (ADR-0039 point 18). The lists
// are read through functions that return a copy, so no reader can change what another reads. It imports only the
// standard library, so any package may use it.
package causereason

import "slices"

// OOMKilled is the reason of a container the kernel killed for memory, read from pod status: a cause (ADR-0039
// point 3) whose Incident shows its absorbed tally (point 18).
const OOMKilled = "OOMKilled"

// waiting are the waiting reasons of Waiting, in that order.
var waiting = [...]string{
	"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError",
}

// Waiting returns a copy of the waiting reasons of a container's status that are a cause (ADR-0039 point 3): an
// image that cannot be pulled, or a configuration the kubelet cannot create the container from. Any other waiting
// reason is not read, and the waiting message never is: it is free text.
func Waiting() []string {
	return slices.Clone(waiting[:])
}

// IsWaiting reports whether reason is one of Waiting.
func IsWaiting(reason string) bool {
	return slices.Contains(waiting[:], reason)
}

// Tallied returns a copy of the Incident reasons whose summary shows the absorbed tally to the model (ADR-0039
// point 18, decided 2026-11-21 on #432): OOMKilled, then Waiting. They are the reasons that showed it before #432;
// a non-zero exit joins them once its own scenario is recorded and run on the real models (#1080).
func Tallied() []string {
	return append([]string{OOMKilled}, waiting[:]...)
}

// ShowsTally reports whether reason is one of Tallied.
func ShowsTally(reason string) bool {
	return reason == OOMKilled || IsWaiting(reason)
}

// CausesUnhealthy reports whether a cause of reason may count Unhealthy events of its container (ADR-0039 point 3,
// narrowed 2026-11-21 on #432): an OOM kill or a waiting reason, as before #432. A non-zero exit is a cause of
// BackOff events only: a liveness probe failure makes the kubelet kill the container, so its exit is a
// consequence of the probe failure, not its cause.
func CausesUnhealthy(reason string) bool {
	return reason == OOMKilled || IsWaiting(reason)
}
