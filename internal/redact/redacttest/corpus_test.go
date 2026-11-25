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

package redacttest_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

func corpusDigest(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	data = fmt.Appendf(data, "%q", v)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// These digests were read from origin/main at 1f672163 before the helpers moved.
// JSON covers every field, the order of cases and the full hash-keyed index.
// A quoted rendering also preserves invalid UTF-8 bytes that JSON replaces.
func TestCorpusMatchesMain(t *testing.T) {
	if redacttest.RaceDetector != raceBuild {
		t.Errorf("race detector flag %v; want %v", redacttest.RaceDetector, raceBuild)
	}
	entries := redacttest.EntryCorpus()
	if got := corpusDigest(t, entries); len(entries) != 12957 || got != "8e44653a607a39614190b6263536b22d1fdde42ab1803e9a2868b0e3eb5b0aa6" {
		t.Errorf("entry corpus: %d cases, digest %s", len(entries), got)
	}
	cases, sampled := redacttest.CanonCorpus()
	count, want := 14310, "c055da9dc7d9afcfbde1853b90f8b7f325b03166136c4a088a92c08f87e8ab80"
	if raceBuild {
		count, want = 58, "c7f10de5745b8aad9da615c7c23bdd954c2cb000921ea58458eb34658d1a5f3b"
	} else if testing.Short() {
		count, want = 449, "1a65045f1bcd58355564e67da5a87898db19a53bf4911cf2e0bbd3259664b7c8"
	}
	if got := corpusDigest(t, cases); len(cases) != count || got != want || sampled != (raceBuild || testing.Short()) {
		t.Errorf("canonical corpus: %d cases, sampled %v, digest %s; want %d cases, digest %s", len(cases), sampled, got, count, want)
	}
	index := redacttest.CanonIndex()
	if got := corpusDigest(t, index); len(index) != 14310 || got != "cba718808237087bbea1f5918983da4638a5b0d157d53ad03ff99b938563c222" {
		t.Errorf("canonical index: %d entries, digest %s", len(index), got)
	}
}

func TestRaceSelectionMatchesMain(t *testing.T) {
	var kept []string
	for _, name := range []string{"String", "fields", "", "canonical", "unicode \u200b"} {
		for i := range 4096 {
			if redacttest.RaceKeeps(name, i) {
				kept = append(kept, fmt.Sprintf("%s:%d", name, i))
			}
		}
	}
	count, want := 20480, "faff9fa43a4ad4427740106beed89ac890f800aee2579c765ae3acd6c97dbd00"
	if raceBuild {
		count, want = 153, "56862bb3a0f5686aa183d1fc6844e965801e78cc02cf44ba40f3dffbf032b725"
	}
	if got := corpusDigest(t, kept); len(kept) != count || got != want {
		t.Errorf("race selection: %d cases, digest %s", len(kept), got)
	}
	if redacttest.RaceSampleEvery != 128 {
		t.Errorf("race sample divisor %d; want 128", redacttest.RaceSampleEvery)
	}
}

func TestParallelUnderRace(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		finished := false
		t.Run("child", func(t *testing.T) { redacttest.ParallelUnderRace(t); finished = true })
		if finished == raceBuild {
			t.Error("parallel scheduling differs from the race build mode")
		}
	})
}

func TestCorpusDigestPreservesInvalidBytes(t *testing.T) {
	if corpusDigest(t, []string{"\xff"}) == corpusDigest(t, []string{"\xfe"}) {
		t.Fatal("different invalid bytes have the same corpus digest")
	}
}
