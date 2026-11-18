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

// Package keyfile holds the check that keeps a model provider's key file to one declaration (ADR-0042): the
// OpenAI-compatible and Anthropic adapters run it on the handle they read the key from, before each read.
package keyfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Shared reports whether the key file at path, opened and described by st (from the handle that is read, so
// a file swapped after the check is not the one read), is the key file of another declaration in others, by
// cleaned path or by file identity (a symbolic or hard link counts). A file of another declaration that does
// not exist is not this one; any other failure to read its identity is an error (the key is unavailable for
// now, to retry).
func Shared(path string, st os.FileInfo, others []string) (bool, error) {
	path = filepath.Clean(path)
	for _, o := range others {
		if filepath.Clean(o) == path {
			return true, nil
		}
		ost, err := os.Stat(o)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if os.SameFile(st, ost) {
			return true, nil
		}
	}
	return false, nil
}
