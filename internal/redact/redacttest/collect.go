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

package redacttest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Counting the failures of a run. go test -fuzz stops at the first failing input; with the environment variable
// CollectEnv naming a directory, a failing input that is not a known failure is written there instead, under
// <target>/<signature hash>, the smallest one of each signature kept, and the input passes, so that one run finds
// every kind it can in its time. [Minimize] then makes each one small.

// CollectEnv names the directory failing inputs are collected in.
const CollectEnv = "FUZZ845_COLLECT"

// Signature is the kind of a failure: each failure's property and what failed, with the shape of the text before
// a leaked probe ([LeakLine]) and without the numbers, probes and texts that differ between inputs of one kind.
func Signature(fails []string) string {
	sigs := make([]string, 0, len(fails))
	for _, f := range fails {
		head := f
		if i := strings.Index(f, " (after "); i >= 0 {
			j := strings.Index(f[i:], "): ")
			if j < 0 {
				j = len(f) - i
			}
			head = cutProbes(f[:i]) + f[i:i+j+1]
		} else if i := strings.IndexAny(f, "\"["); i >= 0 {
			head = f[:i]
		}
		prop, rest, _ := strings.Cut(head, ":")
		sigs = append(sigs, prop+":"+numbers.ReplaceAllString(rest, "9"))
	}
	return strings.Join(sigs, "\n")
}

var (
	numbers   = regexp.MustCompile(`[0-9]+`)
	probeList = regexp.MustCompile(`\[` + ProbePrefix + `[0-9 A-Z]*\]`)
)

func cutProbes(s string) string { return probeList.ReplaceAllString(s, "[probes]") }

// collect writes data under the collect directory when CollectEnv is set, and reports whether it did.
func collect(t testing.TB, data []byte, fails []string) bool {
	dir := os.Getenv(CollectEnv)
	if dir == "" {
		return false
	}
	target, _, _ := strings.Cut(t.Name(), "/")
	sig := Signature(fails)
	h := sha256.Sum256([]byte(target + "\n" + sig))
	d := filepath.Join(dir, target)
	if err := os.MkdirAll(d, 0o750); err != nil { // #nosec G703 -- under the directory the person running the fuzzer names
		t.Fatal(err)
	}
	path := filepath.Join(d, hex.EncodeToString(h[:8]))
	if old, err := os.Stat(path); err == nil && old.Size() <= int64(len(data)) { // #nosec G703 -- as above
		return true
	}
	tmp := path + "." + hex.EncodeToString(h[8:12]) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // #nosec G703 -- as above
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sig", []byte(sig+"\n"), 0o600); err != nil { // #nosec G703 -- under the directory the person running the fuzzer names
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil { // #nosec G703 -- under the directory the person running the fuzzer names
		t.Fatal(err)
	}
	return true
}

// maxMinimizeRuns bounds the runs of one Minimize.
const maxMinimizeRuns = 1500

// Minimize returns a smaller input than data whose failures have the signature sig, by removing runs of bytes
// (halves first, then smaller ones, down to single bytes) while run(input) keeps that signature.
func Minimize(data []byte, sig string, run func([]byte) []string) []byte {
	cur := append([]byte(nil), data...)
	runs := 0
	for n := max(len(cur)/2, 1); n >= 1; n /= 2 {
		for i := 0; i+n <= len(cur); {
			if runs++; runs > maxMinimizeRuns {
				return cur
			}
			try := append(append([]byte(nil), cur[:i]...), cur[i+n:]...)
			if fails := run(try); len(fails) > 0 && Signature(fails) == sig {
				cur = try
				continue
			}
			i += n
		}
		if n == 1 {
			break
		}
	}
	return cur
}

// MinimizeEnv names the directory of a collecting run whose inputs MinimizeCollected makes small.
const MinimizeEnv = "FUZZ845_MINIMIZE"

// MinimizeCollected is the test that makes the collected inputs of a package's targets small (bodies, by target)
// and prints each kind: its target, signature, size before and after and the input, and the signatures of the
// known failures, so that a run is counted by kind beyond them. It skips without MinimizeEnv.
func MinimizeCollected(t *testing.T, bodies map[string]func([]byte) ([]string, bool),
	known map[string]map[string]KnownFailure) {
	dir := os.Getenv(MinimizeEnv)
	if dir == "" {
		t.Skip(MinimizeEnv + " is not set")
	}
	for target, body := range bodies {
		run := func(d []byte) []string { f, _ := body(d); return f }
		for seed := range known[target] {
			t.Logf("KNOWN %s %q", target, Signature(run([]byte(seed))))
		}
		files, _ := filepath.Glob(filepath.Join(dir, target, "*"))
		for _, path := range files {
			if filepath.Ext(path) != "" {
				continue
			}
			data, err := os.ReadFile(path) // #nosec G304 G703 -- a file of the directory the person running the test names
			if err != nil {
				t.Fatal(err)
			}
			fails := run(data)
			if len(fails) == 0 {
				t.Logf("NOFAIL %s %s", target, filepath.Base(path))
				continue
			}
			sig := Signature(fails)
			small := Minimize(data, sig, run)
			t.Logf("KIND %s %s %d->%d sig=%q\n  input=%q\n  %s", target, filepath.Base(path), len(data), len(small), sig,
				small, Clip(strings.Join(run(small), "\n")))
		}
	}
}
