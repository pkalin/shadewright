//go:build race

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

// raceBuild is true in a binary built with the race detector, which only tests are. Its shadow memory takes
// terabytes of address space, so the render process sets no data limit there ([MaxRenderData]) and allows
// eight times the CPU time; the memory its Go runtime holds stays bounded by [MaxRenderMemory].
const raceBuild = true
