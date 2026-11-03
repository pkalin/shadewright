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

package sourcemap_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
)

// counts records the provider calls and the cluster reads of each run of a fixture.
type counts struct {
	mu  sync.Mutex
	git map[string]int
	get int
}

func (c *counts) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.git, c.get = map[string]int{}, 0
}

func (c *counts) of(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.git[op]
}

func (c *counts) gets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.get
}

// counted makes every Git call and every successful cluster read of f count in the returned counts. fail,
// when set, decides the outcome of each Git call.
func counted(f *fixture, fail func(op string) error) *counts {
	c := &counts{}
	c.reset()
	f.gitFail = func(op string) error {
		c.mu.Lock()
		c.git[op]++
		c.mu.Unlock()
		if fail != nil {
			return fail(op)
		}
		return nil
	}
	f.afterGet = func(client.Client, client.Object) {
		c.mu.Lock()
		c.get++
		c.mu.Unlock()
	}
	return c
}

func newCache() *sourcemap.ReadCache {
	return sourcemap.NewReadCache(sourcemap.DefaultCacheEntries, sourcemap.DefaultCacheBytes)
}

// sameWithCache runs the fixture that change gives three times through one cache and fails unless every run
// returns want, the result of a run without a cache: a cold, a partly warm and a warm cache all give the
// verdict an uncached mapping gives.
func sameWithCache(t *testing.T, change func(*fixture), want sourcemap.Result) {
	t.Helper()
	cache := newCache()
	for i := range 3 {
		f := newFixture()
		if change != nil {
			change(f)
		}
		f.cache = cache
		got, _, err := f.run(t)
		if err != nil {
			t.Fatalf("run %d with the cache: %v", i+1, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d with the cache = %+v, want %+v", i+1, got, want)
		}
	}
}

// A recheck of an unchanged delivery reads no file again: only the branch head and the cluster objects, which
// can change at any time, are read on every run.
func TestResolveReusesFileReadsAtTheSameCommit(t *testing.T) {
	f := newFixture()
	f.cache = newCache()
	c := counted(f, nil)

	first, _, err := f.run(t)
	if err != nil || first.Outcome != sourcemap.Resolved {
		t.Fatalf("first run = %+v, %v", first, err)
	}
	firstReads, firstGets := c.of(opReadFile), c.gets()
	if firstReads == 0 || c.of("BranchHead") != 1 {
		t.Fatalf("first run: %d file reads, %d head reads", firstReads, c.of("BranchHead"))
	}

	// From the second run on, nothing at the commit is read again: neither the files nor the .sourceignore
	// and kustomization file names the adapter found absent from the tree.
	for run := 2; run <= 3; run++ {
		c.reset()
		got, _, err := f.run(t)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d = %+v, want %+v", run, got, first)
		}
		if c.of("BranchHead") != 1 {
			t.Errorf("run %d read the branch head %d times, want 1", run, c.of("BranchHead"))
		}
		if c.gets() != firstGets {
			t.Errorf("run %d read %d cluster objects, want %d as in the first run", run, c.gets(), firstGets)
		}
		if c.of(opReadFile) != 0 {
			t.Errorf("run %d read %d files, want 0", run, c.of(opReadFile))
		}
	}
}

// A merge to the branch moves its head, and the next mapping reads the files at the new head: a cached read
// at the old commit never answers for it.
func TestResolveReadsTheNewHeadAfterAMerge(t *testing.T) {
	f := newFixture()
	f.cache = newCache()
	for range 3 {
		if res, _, err := f.run(t); err != nil || res.Outcome != sourcemap.Resolved {
			t.Fatalf("run = %+v, %v", res, err)
		}
	}
	f.files[soFile] = strings.Replace(scaledObjectYAML, "maxReplicaCount: 10", "maxReplicaCount: 12", 1)
	res, _, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != sourcemap.NeedsIntervention || res.Reason != sourcemap.ReasonValueMismatch {
		t.Fatalf("after the merge = %s/%s (%s), want NeedsIntervention/ValueMismatch", res.Outcome, res.Reason, res.Message)
	}
}

// Reads made through one settings entry are never reused through another, even for the same commit and path:
// another URL or other credentials each read the files again.
func TestResolveKeepsCachedReadsPerRepositoryEntry(t *testing.T) {
	tests := []struct {
		name   string
		change func(f *fixture)
	}{
		{"another installation", func(f *fixture) { f.in.Repositories[1].GitHub.InstallationID = 2 }},
		{"another App", func(f *fixture) { f.in.Repositories[1].GitHub.AppClientID = "Iv1.other" }},
		{"another signing key", func(f *fixture) {
			f.in.Repositories[1].GitHub.Signer.AzureKeyVault.KeyID = "https://kv.vault.azure.net/keys/other/1"
		}},
		{"another URL", func(f *fixture) {
			const url = fleetURL + "-mirror"
			f.in.Repositories[1].URL = url
			set(f.src, url, "spec", "url")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.in.Repositories[1].GitHub = &v1alpha1.GitHubRepository{
				AppClientID: "Iv1.fleet", InstallationID: 1,
				Signer: v1alpha1.Signer{AzureKeyVault: &v1alpha1.AzureKeyVaultKey{
					KeyID: "https://kv.vault.azure.net/keys/fleet/1",
				}},
			}
			f.cache = newCache()
			c := counted(f, nil)
			cold := 0
			for i := range 3 {
				if res, _, err := f.run(t); err != nil || res.Outcome != sourcemap.Resolved {
					t.Fatalf("run = %+v, %v", res, err)
				}
				if i == 0 {
					cold = c.of(opReadFile)
				}
			}
			tc.change(f)
			c.reset()
			res, _, err := f.run(t)
			if err != nil || res.Outcome != sourcemap.Resolved {
				t.Fatalf("run through the other entry = %+v, %v", res, err)
			}
			if c.of(opReadFile) != cold {
				t.Errorf("the other entry read %d files, want %d as a mapping with an empty cache: "+
					"it was answered from the first entry's reads", c.of(opReadFile), cold)
			}
		})
	}
}

// Only reads that can never change are kept. A failed read is read again, and so is a plain not found:
// GitProvider gives ErrNotFound for a commit the host does not have yet.
func TestResolveDoesNotKeepReadsThatMayChange(t *testing.T) {
	tests := []struct {
		name string
		fail error
	}{
		{"host error", &providers.RetryableError{Err: errors.New("502")}},
		{"commit not found yet", fmt.Errorf("commit: %w", providers.ErrNotFound)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.cache = newCache()
			counted(f, func(op string) error {
				if op == opReadFile {
					return tc.fail
				}
				return nil
			})
			if res, _, err := f.run(t); err == nil && res.Outcome == sourcemap.Resolved {
				t.Fatalf("first run resolved with every read failing: %+v", res)
			}
			f.gitFail = nil
			res, _, err := f.run(t)
			if err != nil || res.Outcome != sourcemap.Resolved {
				t.Fatalf("run after the host recovered = %s/%s (%s), %v; want Resolved",
					res.Outcome, res.Reason, res.Message, err)
			}
		})
	}
}

// Strings the tests share.
const (
	opReadFile           = "ReadFile"
	ksWorkers            = "resources:\n  - workers\n"
	ksParentScaledObject = "resources:\n  - ../scaledobject.yaml\n"
	ksEmpty              = "resources: []\n"
)

// errHost404 is what the GitHub adapter returns for HTTP 404 on any read, and for a token or tree it cannot
// get: ErrNotFound, although the path may well be in the tree. Only ErrPathNotInTree says it is not.
var errHost404 = fmt.Errorf("ReadFile: GitHub returned status 404: %w", providers.ErrNotFound)

// A host that serves files and then answers 404 to every read (an outage, a revoked installation) must not
// leave its not founds in the cache: once it recovers, the next mapping reads again and gives the verdict an
// uncached mapping gives, so a missing .sourceignore or a second kustomization file never fails open.
func TestResolveDoesNotKeepNotFoundsFromAHostOutage(t *testing.T) {
	tests := []struct {
		name   string
		change func(f *fixture)
	}{
		{"unchanged delivery", nil},
		{".sourceignore in a nested kustomization directory", func(f *fixture) {
			f.files[ksFile] = ksWorkers
			f.files["apps/orders/workers/kustomization.yaml"] = ksParentScaledObject
			f.files["apps/orders/workers/.sourceignore"] = "kustomization.yaml\n"
		}},
		{".sourceignore in a directory a resource file lives in", func(f *fixture) {
			f.files[ksFile] = "resources:\n  - ../base/so.yaml\n"
			f.files["apps/base/so.yaml"] = scaledObjectYAML
			f.files["apps/base/.sourceignore"] = "so.yaml\n"
		}},
		{"two kustomization files", func(f *fixture) {
			f.files["apps/orders/kustomization.yml"] = ksEmpty
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setup := func() *fixture {
				f := newFixture()
				if tc.change != nil {
					tc.change(f)
				}
				return f
			}
			uncached := setup()
			c := counted(uncached, nil)
			want, _, err := uncached.run(t)
			if err != nil {
				t.Fatal(err)
			}
			total := c.of(opReadFile)
			// The outage starts after each number of served reads in turn.
			for served := range total {
				f := setup()
				f.cache = newCache()
				reads := 0
				counted(f, func(op string) error {
					if op != opReadFile {
						return nil
					}
					if reads++; reads > served {
						return errHost404
					}
					return nil
				})
				_, _, _ = f.run(t)
				f.gitFail = nil
				for run := 1; run <= 2; run++ {
					got, _, err := f.run(t)
					if err != nil {
						t.Fatalf("outage after %d reads, run %d after recovery: %v", served, run, err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("outage after %d reads, run %d after recovery = %s/%s (%s), want %s/%s (%s) "+
							"as without the cache", served, run, got.Outcome, got.Reason, got.Message,
							want.Outcome, want.Reason, want.Message)
					}
				}
			}
		})
	}
}
