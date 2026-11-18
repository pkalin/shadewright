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

package openaicompatible

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// These tests check decision 7 of #834: a key file belongs to one declaration. Two declarations naming one
// file, or a declaration naming the Anthropic key file, by path or through a link, are refused at start,
// and a file that appears later as a link to another declared file is refused at its read.

const (
	keyContent    = "content-marker-834-keyfile-0001"
	anthropicFlag = "--anthropic-api-key-file"
	declA         = "https://a.example.com/v1"
	declB         = "https://b.example.com/v1"
)

func TestValidateKeyFiles(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(keyContent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sym := filepath.Join(dir, "sym")
	if err := os.Symlink(a, sym); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "hard")
	if err := os.Link(a, hard); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	decl := func(host, key string) Endpoint {
		return Endpoint{URL: "https://" + host + "/v1", API: APIChatCompletions, Models: []string{testModel}, KeyFile: key}
	}
	anthropic := func(p string) []NamedKeyFile { return []NamedKeyFile{{Name: anthropicFlag, Path: p}} }
	for _, tc := range []struct {
		name      string
		endpoints []Endpoint
		others    []NamedKeyFile
		refused   bool
		names     []string
	}{
		{"distinct files", []Endpoint{decl("a.example.com", a), decl("b.example.com", b)}, anthropic(missing), false, nil},
		{"no key files", []Endpoint{decl("a.example.com", ""), decl("b.example.com", "")}, anthropic(a), false, nil},
		{"same path", []Endpoint{decl("a.example.com", a), decl("b.example.com", a)}, nil, true,
			[]string{declA, declB}},
		{"symlink", []Endpoint{decl("a.example.com", a), decl("b.example.com", sym)}, nil, true,
			[]string{declA, declB}},
		{"hard link", []Endpoint{decl("a.example.com", hard), decl("b.example.com", a)}, nil, true,
			[]string{declA, declB}},
		{"missing, same path", []Endpoint{decl("a.example.com", missing), decl("b.example.com", missing)}, nil, true,
			[]string{declA, declB}},
		{"missing, other path", []Endpoint{decl("a.example.com", missing), decl("b.example.com", a)}, nil, false, nil},
		{"anthropic path", []Endpoint{decl("a.example.com", a)}, anthropic(a), true,
			[]string{declA, anthropicFlag}},
		{"anthropic symlink", []Endpoint{decl("a.example.com", sym)}, anthropic(a), true,
			[]string{declA, anthropicFlag}},
		{"anthropic hard link", []Endpoint{decl("a.example.com", a)}, anthropic(hard), true,
			[]string{declA, anthropicFlag}},
		{"anthropic missing", []Endpoint{decl("a.example.com", a)}, anthropic(missing), false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKeyFiles(tc.endpoints, tc.others)
			if (err != nil) != tc.refused {
				t.Fatalf("refused %v, want %v: %v", err != nil, tc.refused, err)
			}
			if err == nil {
				return
			}
			for _, n := range tc.names {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("the error does not name %q: %v", n, err)
				}
			}
			if strings.Contains(err.Error(), keyContent) {
				t.Errorf("the error repeats the file content: %v", err)
			}
		})
	}
}

// A declared key file that does not exist at start is compared by path only. When it appears later as a link
// to another declared key file, the read before each attempt refuses it: no request is sent with that key.
func TestKeyFileThatAppearsAsALinkIsRefusedAtRead(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "anthropic")
	if err := os.WriteFile(other, []byte(keyContent), 0o600); err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(dir, "late")
	if err := ValidateKeyFiles([]Endpoint{{URL: endpoint, API: APIChatCompletions, Models: []string{testModel}, KeyFile: late}},
		[]NamedKeyFile{{Name: anthropicFlag, Path: other}}); err != nil {
		t.Fatalf("a missing file was refused at start: %v", err)
	}
	if err := os.Symlink(other, late); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, KeyFile: late, OtherKeyFiles: []string{other},
		MaxAttempts: 1}, testHooks{sleep: noSleep, transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return response(200, "application/json", chatOK), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(t.Context(), request())
	if calls != 0 || !errors.Is(err, providers.ErrCredentialUnavailable) || !providers.IsNotSent(err) {
		t.Fatalf("%d requests sent with another declaration's key, err %v", calls, err)
	}
	if strings.Contains(err.Error(), keyContent) || strings.Contains(err.Error(), dir) {
		t.Fatalf("the error repeats the content or the path: %v", err)
	}
	// Control: the same provider without the other file declared sends the request.
	p, err = newProvider(Config{Endpoint: endpoint, Deployment: testModel, KeyFile: late, MaxAttempts: 1},
		testHooks{sleep: noSleep, transport: roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			return response(200, "application/json", chatOK), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(t.Context(), request()); err != nil || calls != 1 {
		t.Fatalf("control: %d requests, err %v", calls, err)
	}
}
