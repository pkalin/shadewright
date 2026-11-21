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

package intake

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failed size or read names the step, not the path, and keeps its class for the caller.
func TestFileSourceErrorsHoldNoPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-dir-name")
	src := &FileSource{Path: filepath.Join(dir, testRecordID+".json")}
	_, err := src.Size(context.Background())
	if err == nil || strings.Contains(err.Error(), "private-dir-name") || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("size of a missing file: %v", err)
	}
	_, err = src.Read(context.Background(), MaxRecordBytes)
	if err == nil || strings.Contains(err.Error(), "private-dir-name") || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read of a missing file: %v", err)
	}
}

// A file replaced after Size by another of the same length and modification time is not read.
func TestFileSourceRefusesAReplacementOfTheSameSizeAndTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, testRecordID+".json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mod := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	src := &FileSource{Path: path}
	if _, err := src.Size(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte(`{"b":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, mod, mod); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	if data, err := src.Read(context.Background(), MaxRecordBytes); !errors.Is(err, errChanged) {
		t.Fatalf("read = %q, %v; want errChanged", data, err)
	}
}
