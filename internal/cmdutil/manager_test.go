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

package cmdutil

import (
	"flag"
	"slices"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/cmdutil/sharedflags"
)

// TestSharedBoolFlagsMatchBindFlags ties the list kubectl-shadewright reads the analyzer's arguments with to the
// flags BindFlags registers, so a boolean flag added here cannot be missed there.
func TestSharedBoolFlagsMatchBindFlags(t *testing.T) {
	fs := flag.NewFlagSet("shared", flag.ContinueOnError)
	var opts Options
	opts.BindFlags(fs)
	var got []string
	fs.VisitAll(func(f *flag.Flag) {
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			got = append(got, f.Name)
		}
	})
	slices.Sort(got)
	if want := sharedflags.BoolFlags(); !slices.Equal(got, want) {
		t.Fatalf("boolean flags of BindFlags = %v; sharedflags.BoolFlags() = %v", got, want)
	}
}
