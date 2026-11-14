//go:build !linux && !darwin

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
)

// setLimits fails where the render process's limits are not implemented, so no render runs without them.
func setLimits(uint64, uint64) error {
	return errors.New("render process limits are not supported here")
}

func catchCPU() {}

func preferOOMKill() {}

// noDataLimit is unused here: setLimits fails.
const noDataLimit = 0

// residentBytes reads nothing: no render process runs here (setLimits fails), so the parent does not watch the render process's resident memory.
func residentBytes(int) (uint64, bool) { return 0, false }

// killedBySIGKILL reports false: no render process runs here.
func killedBySIGKILL(error) bool { return false }
