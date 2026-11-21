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
	"io"
	"os"
	"path/filepath"
)

// Source is where one record comes from: a file today, a blob of the records container later (#991). Name is
// known before any read; Size reads the length without the content (a blob's properties), and Read reads the
// content once, bounded.
type Source interface {
	// Name is the base name of the record, "<record ID>.json" for a record the analyzer wrote. It is untrusted.
	Name() string
	// Size returns the length of the record in bytes without reading it.
	Size(ctx context.Context) (int64, error)
	// Read returns the content, reading at most limit+1 bytes. It fails when the content is longer than limit
	// or changed since Size (a blob read with the ETag of its properties); either is a failed read for a retry.
	Read(ctx context.Context, limit int64) ([]byte, error)
}

// Errors of a FileSource. They are fixed text: no path or content.
var (
	errNotRegular = errors.New("the record is not a regular file")
	errChanged    = errors.New("the record changed between its size and its read")
	errTooLong    = errors.New("the record is longer than the read limit")
)

// FileSource is a record in a local file, for a run on a laptop or on downloaded copies of records.
type FileSource struct {
	Path string
	// size is what Size read, compared with the file Read opens.
	size int64
	mod  int64
	seen bool
}

// Name returns the file's base name.
func (f *FileSource) Name() string { return filepath.Base(f.Path) }

// Size returns the file's length. A path that is not a regular file (a directory, a device, a pipe or a
// symbolic link) is an error, so nothing blocks or reads past the bound.
func (f *FileSource) Size(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	st, err := os.Lstat(f.Path)
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, errNotRegular
	}
	f.size, f.mod, f.seen = st.Size(), st.ModTime().UnixNano(), true
	return st.Size(), nil
}

// Read reads the file, at most limit+1 bytes. The file must still be the one Size described (same length and
// modification time) and no longer than limit.
func (f *FileSource) Read(ctx context.Context, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errNotRegular
	}
	if f.seen && (st.Size() != f.size || st.ModTime().UnixNano() != f.mod) {
		return nil, errChanged
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLong
	}
	if f.seen && int64(len(data)) != f.size {
		return nil, errChanged
	}
	return data, nil
}
