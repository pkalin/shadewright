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
// builder, which shows the absorbed tally for those reasons (ADR-0039 point 18). It imports nothing, so any
// package may use it.
package causereason

// Waiting are the waiting reasons of a container's status that are a cause (ADR-0039 point 3): an image that
// cannot be pulled, or a configuration the kubelet cannot create the container from. Any other waiting reason is
// not read, and the waiting message never is: it is free text.
var Waiting = []string{
	"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError",
}
