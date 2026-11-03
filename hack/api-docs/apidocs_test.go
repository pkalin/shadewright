// Copyright 2026 The Shadewright Authors.
// SPDX-License-Identifier: Apache-2.0

// Package apidocs checks the generated API reference (docs/reference/api.md, make api-docs).
package apidocs

import (
	"os"
	"strings"
	"testing"
)

// separators counts the cell separators of a table row: every '|' that is not escaped as '\|'.
func separators(row string) int {
	n := 0
	for i := 0; i < len(row); i++ {
		switch row[i] {
		case '\\':
			i++
		case '|':
			n++
		}
	}
	return n
}

// TestTableRowsHaveHeaderCellCount fails when a table row has a different number of cell separators than the
// header of its table, which is how an unescaped '|' in a Pattern or description splits a row on GitHub.
func TestTableRowsHaveHeaderCellCount(t *testing.T) {
	data, err := os.ReadFile("../../docs/reference/api.md")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for i, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "|") {
			want = 0
			continue
		}
		got := separators(line)
		if want == 0 {
			want = got // the first row of a table is its header
			continue
		}
		if got != want {
			t.Errorf("docs/reference/api.md:%d: %d cell separators, header has %d (escape '|' as '\\|'): %.100s", i+1, got, want, line)
		}
	}
}
