//go:build unix

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

package azurekv

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestTokenFileRefusesPipesAndFollowsLinks checks that a named pipe is refused without blocking, and that a
// symbolic link to a regular file (how a projected volume presents the token) is read.
func TestTokenFileRefusesPipesAndFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readTokenFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errTokenFileNotRegular) {
			t.Errorf("pipe: err = %v, want the not regular error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reading a pipe blocked")
	}

	target := writeTokenFile(t, testAssertion)
	link := filepath.Join(dir, "azure-identity-token")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got, err := readTokenFile(link); err != nil || got != testAssertion {
		t.Errorf("link: %q, %v; want the token", got, err)
	}
}
