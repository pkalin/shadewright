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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/importguard"
)

// testRecordID is a record ID in the shape the name check gives.
const testRecordID = "0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5b-20261120t093000z"

func TestDirLedgerSumsTheNamesOfItsMonthAndFamily(t *testing.T) {
	ctx := context.Background()
	l := DirLedger{Root: t.TempDir()}
	if rs, err := l.List(ctx, testMonth, FamilyOpenAI); err != nil || Spent(rs) != 0 {
		t.Fatalf("an empty ledger: %v, %v", rs, err)
	}
	for _, r := range []Reservation{
		{Month: testMonth, Day: "20261121", Record: testRecordID, Family: FamilyOpenAI, Run: "a", Call: 1, Tokens: 100},
		{Month: testMonth, Day: "20261121", Record: testRecordID, Family: FamilyOpenAI, Run: "a", Call: 3, Tokens: 50},
		{Month: testMonth, Day: "20261121", Record: testRecordID, Family: FamilyAnthropic, Run: "a", Call: 2, Tokens: 7},
		{Month: "2026-10", Day: "20261031", Record: testRecordID, Family: FamilyOpenAI, Run: "b", Call: 1, Tokens: 1000},
	} {
		if err := l.Reserve(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if rs, err := l.List(ctx, testMonth, FamilyOpenAI); err != nil || Spent(rs) != 150 {
		t.Fatalf("spent = %d, %v; want 150", Spent(rs), err)
	}
	err := l.Reserve(ctx, Reservation{Month: testMonth, Day: "20261121", Record: testRecordID, Family: FamilyOpenAI,
		Run: "a", Call: 1, Tokens: 100})
	if !errors.Is(err, ErrReservationExists) {
		t.Fatalf("a second reservation of the same name: %v", err)
	}
	good := Reservation{Month: testMonth, Day: "20261121", Record: testRecordID, Family: FamilyOpenAI, Run: "c", Call: 1}
	for name, mod := range map[string]func(*Reservation){
		"month":         func(r *Reservation) { r.Month = "../x" },
		"family":        func(r *Reservation) { r.Family = "other" },
		"run":           func(r *Reservation) { r.Run = "../a" },
		"call":          func(r *Reservation) { r.Call = 4 },
		"no day":        func(r *Reservation) { r.Day = "" },
		"other month":   func(r *Reservation) { r.Day = "20261201" },
		"no such day":   func(r *Reservation) { r.Day = "20261131" },
		"no record":     func(r *Reservation) { r.Record = "" },
		"record dotted": func(r *Reservation) { r.Record = testRecordID + ".x" },
	} {
		bad := good
		mod(&bad)
		if err := l.Reserve(ctx, bad); err == nil {
			t.Errorf("%s: reservation %+v was created", name, bad)
		}
	}
	// An entry that is not a reservation makes the sum untrustworthy: an error, never a lower sum.
	if err := os.WriteFile(filepath.Join(l.Root, "spend", testMonth, "openai", "note.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.List(ctx, testMonth, FamilyOpenAI); err == nil {
		t.Fatal("a foreign entry was ignored")
	}
}

func TestFileSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	src := &FileSource{Path: path}
	if n, err := src.Size(ctx); err != nil || n != 7 || src.Name() != "rec.json" {
		t.Fatalf("size %d, %v, name %q", n, err, src.Name())
	}
	if data, err := src.Read(ctx, 7); err != nil || string(data) != `{"a":1}` {
		t.Fatalf("read %q, %v", data, err)
	}
	if _, err := src.Read(ctx, 6); err == nil {
		t.Fatal("a read past the limit succeeded")
	}
	if err := os.WriteFile(path, []byte(`{"a":12}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Read(ctx, 100); !errors.Is(err, errChanged) {
		t.Fatalf("a file changed after its size: %v", err)
	}
	if _, err := (&FileSource{Path: dir}).Size(ctx); !errors.Is(err, errNotRegular) {
		t.Fatalf("a directory: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileSource{Path: link}).Size(ctx); !errors.Is(err, errNotRegular) {
		t.Fatalf("a symbolic link: %v", err)
	}
}

// TestImportsNoControllerOrGitOps keeps the intake out of the cluster's write paths (ADR-0047): nothing it
// depends on, however indirectly, is the controller, the GitOps executor or a Git provider.
func TestImportsNoControllerOrGitOps(t *testing.T) {
	importguard.NoForbiddenDeps(t, ".", ".")
	for _, p := range importguard.Deps(t, ".", ".", "../../cmd/intake") {
		for _, banned := range []string{"/internal/controller", "/internal/gitops", "/internal/providers/github"} {
			if strings.Contains(p.ImportPath, "github.com/shadewright-labs/shadewright"+banned) {
				t.Errorf("the intake depends on %s", p.ImportPath)
			}
		}
	}
}
