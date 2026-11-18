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

package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// limitSweepTable records, for each pod of limitSweep, whether the release before #845 (origin/main at 0701cd3a)
// answered get_resource with valid JSON. It is written on that release with LIMIT_SWEEP_WRITE=1.
const limitSweepTable = "testdata/limit-sweep-main.tsv"

// limitSweepSecrets are the arguments before the filler, each with a replacement that is longer, as long as or
// shorter than the one the release before wrote: an escaped quote hidden on the answer as written (two bytes, one
// on the value), a secret holding quotes, a token, a value the release before found only in its second pass, a
// line break before a key, and none.
var limitSweepSecrets = map[string][]string{
	"escaped quote": {"\nauthorization\": \""},
	"quoted secret": {"--password", `p"q"r"s"t"u"v"w"x"y`},
	"token":         {"token=Zq8LmT3vXr9KpW2sYd6HnB4cJf7GaE1U"},
	"second pass":   {"192.168.1.1--password\nUNIQUESecretProbe938"},
	"line break":    {"a\n\tpassword: \\\"hunter2\\\""},
	"none":          nil,
}

// limitFiller is a filler argument of n bytes: words "x" between blanks.
func limitFiller(n int) string {
	f := strings.Repeat("x ", n/2+1)[:n]
	if strings.HasSuffix(f, " ") {
		f = f[:n-1] + "y"
	}
	return f
}

// limitAnswer calls get_resource on a pod made of the secret of limitSweepSecrets named name and a filler
// argument of n bytes, and returns the answer.
func limitAnswer(t *testing.T, name string, n int) string {
	t.Helper()
	args := append(append([]string{}, limitSweepSecrets[name]...), limitFiller(n))
	e := newEnv(t, []client.Object{minimalPod(args)})
	r := e.call(t, NameGetResource, fmt.Sprintf(`{"kind":"Pod","namespace":%q,"name":%q}`, testNS, podName))
	if r.Status != StatusOK {
		t.Fatalf("get_resource answered %s: %s", r.Status, r.Text)
	}
	return r.Text
}

// The answer of get_resource is valid JSON whenever the answer of the release before was, byte by byte across
// its limit, for replacements longer and shorter than the ones it wrote (#845, thirteenth decision of
// 2026-11-18), and never longer than the limit. The rows of the table are the 25 sizes of the filler around the
// largest one at which the release before answered valid JSON for each secret (around the size that makes the
// pod's JSON as long as the limit for a secret it never answered whole for). Under -race the row of that size and
// the next of each secret are called.
func TestGetResourceFitsWhereTheReleaseBeforeFit(t *testing.T) {
	if os.Getenv("LIMIT_SWEEP_WRITE") == "1" {
		var b strings.Builder
		b.WriteString("# Whether get_resource answered valid JSON for each pod of TestGetResourceFitsWhereTheReleaseBeforeFit, at main 0701cd3a (#845).\n")
		b.WriteString("# secret\tbytes of filler\tvalid or cut\n")
		for _, name := range slices.Sorted(maps.Keys(limitSweepSecrets)) {
			base, err := json.MarshalIndent(minimalPod(append(append([]string{}, limitSweepSecrets[name]...), "")), "", " ")
			if err != nil {
				t.Fatal(err)
			}
			mid := resourceBytes - len(base)
			valid := map[int]bool{}
			last := mid
			for n := mid - 120; n <= mid+20; n++ {
				if valid[n] = json.Valid([]byte(limitAnswer(t, name, n))); valid[n] {
					last = n
				}
			}
			for n := last - 12; n <= last+12; n++ {
				state := "cut"
				if valid[n] {
					state = "valid"
				}
				fmt.Fprintf(&b, "%s\t%d\t%s\n", name, n, state)
			}
		}
		if err := os.WriteFile(limitSweepTable, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := readLimitSweepTable(t)
	// Under -race only the largest valid row of each secret and the row after it are called.
	last := map[string]int{}
	for key, ok := range want {
		name, size, _ := strings.Cut(key, "\t")
		if n, err := strconv.Atoi(size); err == nil && (ok || last[name] == 0) {
			last[name] = max(last[name], n)
		}
	}
	valid, cut := 0, 0
	for _, key := range slices.Sorted(maps.Keys(want)) {
		name, size, _ := strings.Cut(key, "\t")
		n, err := strconv.Atoi(size)
		if err != nil {
			t.Fatalf("row %q: %v", key, err)
		}
		if raceDetector && (n < last[name] || n > last[name]+1) {
			continue
		}
		text := limitAnswer(t, name, n)
		if len(text) > resourceBytes {
			t.Errorf("%s: answer of %d bytes, over the limit", key, len(text))
		}
		if !want[key] {
			cut++
			continue
		}
		valid++
		if !json.Valid([]byte(text)) {
			t.Errorf("%s: the release before answered valid JSON, this answer of %d bytes is not", key, len(text))
		}
	}
	if valid == 0 || cut == 0 {
		t.Errorf("the sweep does not cross the limit: %d rows valid, %d cut on the release before", valid, cut)
	}
}

// readLimitSweepTable reads limitSweepTable: by row key, whether the release before answered valid JSON.
func readLimitSweepTable(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(limitSweepTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, '\t')
		out[line[:i]] = line[i+1:] == "valid"
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
