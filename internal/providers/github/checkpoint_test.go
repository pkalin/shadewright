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

package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// errStopped stands for the kill switch's refusal: a class the adapter must keep, wrapped in fixed text.
var errStopped = errors.New("writes are paused")

// blobsPath is where the commit's first write goes.
const blobsPath = "/git/blobs"

// deployYAML is apps/deploy.yaml at shaBase (blob shaDeploy).
const deployYAML = "replicas: 2\n"

// pauseAfter returns a context whose write checkpoint allows the first n writes and refuses every later one,
// and a pointer to the number of times it was asked.
func pauseAfter(n int) (context.Context, *int) {
	asked := 0
	return providers.WithWriteCheckpoint(ctx, func(context.Context) error {
		asked++
		if asked > n {
			return errStopped
		}
		return nil
	}, errStopped), &asked
}

// requests renders steps as the script records the requests that went out.
func requests(steps []step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.method+" "+s.path)
	}
	return out
}

func assertSent(t *testing.T, h *harness, want []string) {
	t.Helper()
	if !slices.Equal(h.s.sent, want) {
		t.Errorf("requests sent:\n%v\nwant:\n%v", h.s.sent, want)
	}
}

// TestPauseAfterRetryableRejectionSendsNoRetry: a write refused with a transient status is not sent again
// after the backoff once the checkpoint refuses, whichever loop would send it again: the retry of a transient
// failure, or the repeat of a create after a 422 the state does not explain.
func TestPauseAfterRetryableRejectionSendsNoRetry(t *testing.T) {
	t.Run("server error on create branch", func(t *testing.T) {
		steps := []step{get(mainRef, "ref_main.json"),
			status(http.MethodPost, "/git/refs", http.StatusBadGateway, "")}
		h := newHarness(t, steps...)
		c, asked := pauseAfter(1)
		err := h.p.CreateBranch(c, branch, shaBase)
		if !errors.Is(err, errStopped) || providers.IsRetryable(err) {
			t.Fatalf("got %v, want the checkpoint's error", err)
		}
		assertSent(t, h, requests(steps))
		if *asked != 2 {
			t.Errorf("checkpoint asked %d times, want 2 (the write and its retry)", *asked)
		}
	})
	t.Run("server error on the branch update", func(t *testing.T) {
		limited := status(http.MethodPost, graphQLPath, http.StatusServiceUnavailable, "")
		limited.header = map[string]string{hRetry: "1"}
		steps := append(append(commitSteps(), get("", "repo.json")), limited)
		h := newHarness(t, steps...)
		c, asked := pauseAfter(4)
		_, err := h.p.CommitFile(c, branch, shaBase, change)
		if !errors.Is(err, errStopped) {
			t.Fatalf("got %v, want the checkpoint's error", err)
		}
		assertSent(t, h, requests(steps))
		if *asked != 5 {
			t.Errorf("checkpoint asked %d times, want 5", *asked)
		}
	})
	t.Run("unexplained 422 on open pull request", func(t *testing.T) {
		steps := []step{listPulls("open", "pulls_empty.json"),
			status(http.MethodPost, pullsPath, http.StatusUnprocessableEntity, "error_validation.json"),
			listPulls("open", "pulls_empty.json"), get(branchRef, "ref_branch.json")}
		h := newHarness(t, steps...)
		c, _ := pauseAfter(1)
		_, err := h.p.OpenChangeRequest(c, newCR)
		if !errors.Is(err, errStopped) || providers.IsRejected(err) {
			t.Fatalf("got %v, want the checkpoint's error", err)
		}
		assertSent(t, h, requests(steps))
	})
}

// TestPauseBetweenCommitWrites: CommitFile makes four writes (blob, tree, commit, branch update). A pause seen
// before any of them stops it there: nothing after it is sent, the branch is never moved, and the reads in
// between still go out.
func TestPauseBetweenCommitWrites(t *testing.T) {
	all := append(commitSteps(), updateSteps("graphql_update_refs.json")...)
	writes := []string{blobsPath, "/git/trees", "/git/commits", graphQLPath}
	for n, w := range writes {
		t.Run("before "+w, func(t *testing.T) {
			// The requests up to the write the pause stops, that write excluded.
			idx := slices.IndexFunc(all, func(s step) bool { return s.method == http.MethodPost && s.path == w })
			steps := slices.Clone(all[:idx])
			h := newHarness(t, steps...)
			c, asked := pauseAfter(n)
			_, err := h.p.CommitFile(c, branch, shaBase, change)
			if !errors.Is(err, errStopped) {
				t.Fatalf("got %v, want the checkpoint's error", err)
			}
			assertSent(t, h, requests(steps))
			if *asked != n+1 {
				t.Errorf("checkpoint asked %d times, want %d", *asked, n+1)
			}
		})
	}
	t.Run("allowed asks once per write", func(t *testing.T) {
		h := newHarness(t, all...)
		c, asked := pauseAfter(len(writes))
		if _, err := h.p.CommitFile(c, branch, shaBase, change); err != nil {
			t.Fatal(err)
		}
		assertSent(t, h, requests(all))
		if *asked != len(writes) {
			t.Errorf("checkpoint asked %d times, want %d", *asked, len(writes))
		}
	})
}

// TestCheckpointGatesEveryWriteMethod: each method that writes asks the checkpoint before its first write and
// sends nothing when it refuses; reads go out.
func TestCheckpointGatesEveryWriteMethod(t *testing.T) {
	cases := []struct {
		name  string
		reads []step
		call  func(context.Context, *Provider) error
	}{
		{"CreateBranch", []step{get(mainRef, "ref_main.json")},
			func(c context.Context, p *Provider) error { return p.CreateBranch(c, branch, shaBase) }},
		{"CommitFile", append([]step{get(branchRef, "ref_branch.json")}, readSteps()...),
			func(c context.Context, p *Provider) error {
				_, err := p.CommitFile(c, branch, shaBase, change)
				return err
			}},
		{"OpenChangeRequest", []step{listPulls("open", "pulls_empty.json")},
			func(c context.Context, p *Provider) error { _, err := p.OpenChangeRequest(c, newCR); return err }},
		{"CloseChangeRequest", []step{get("/pulls/12", "pull_open.json")},
			func(c context.Context, p *Provider) error { return p.CloseChangeRequest(c, 12) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.reads...)
			c, asked := pauseAfter(0)
			if err := tc.call(c, h.p); !errors.Is(err, errStopped) {
				t.Fatalf("got %v, want the checkpoint's error", err)
			}
			assertSent(t, h, requests(tc.reads))
			if *asked != 1 {
				t.Errorf("checkpoint asked %d times, want 1", *asked)
			}
		})
	}
}

// TestCheckpointDoesNotGateReads: reads go out while the checkpoint refuses, and do not ask it.
func TestCheckpointDoesNotGateReads(t *testing.T) {
	blob := get("/git/blobs/"+shaDeploy, "")
	blob.raw = deployYAML
	h := newHarness(t, append(readSteps(), blob)...)
	c, asked := pauseAfter(0)
	if _, err := h.p.ReadFile(c, shaBase, "apps/deploy.yaml"); err != nil {
		t.Fatal(err)
	}
	if *asked != 0 {
		t.Errorf("a read asked the checkpoint %d times", *asked)
	}
}

// TestCheckpointAfterCancellation: a call canceled while its token is fetched reports the cancellation, not
// the checkpoint's answer, and sends nothing.
func TestCheckpointAfterCancellation(t *testing.T) {
	h := newHarness(t)
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	h.p.token = func(context.Context) (string, error) { cancel(); return testToken, nil }
	asked := 0
	c = providers.WithWriteCheckpoint(c, func(context.Context) error { asked++; return errStopped }, errStopped)
	_, err := h.p.do(c, call{op: opCommitFile, method: http.MethodPost, path: blobsPath})
	if !errors.Is(err, context.Canceled) || errors.Is(err, errStopped) || asked != 0 {
		t.Fatalf("got %v after %d checks, want the cancellation without a check", err, asked)
	}
	assertSent(t, h, nil)
}

// TestPauseDuringTokenFetchSendsNothing: the check comes after the token, which may take a signature and a
// request to mint, so a pause set while it is fetched stops the write.
func TestPauseDuringTokenFetchSendsNothing(t *testing.T) {
	steps := []step{get(mainRef, "ref_main.json")}
	h := newHarness(t, steps...)
	paused := false
	h.p.token = func(context.Context) (string, error) {
		if len(h.s.sent) == len(steps) { // the token for the create request
			paused = true
		}
		return testToken, nil
	}
	c := providers.WithWriteCheckpoint(ctx, func(context.Context) error {
		if paused {
			return errStopped
		}
		return nil
	}, errStopped)
	if err := h.p.CreateBranch(c, branch, shaBase); !errors.Is(err, errStopped) {
		t.Fatalf("got %v, want the checkpoint's error", err)
	}
	assertSent(t, h, requests(steps))
}

// TestCheckpointErrorCarriesOnlyItsClass: the adapter's error matches the class of the refusal but holds none of
// its text (an API server error with a URL); a refusal of no known class is ErrWriteStopped.
func TestCheckpointErrorCarriesOnlyItsClass(t *testing.T) {
	const leak = "https://10.0.0.1:6443/apis/remediation.shadewright.io/v1alpha1/remediationsettings/default"
	for _, tc := range []struct {
		name  string
		err   error
		class error
	}{
		{"known class", fmt.Errorf("%w: get settings: Get %q: dial tcp: timeout", errStopped, leak), errStopped},
		{"unknown class", fmt.Errorf("Get %q: connection refused", leak), providers.ErrWriteStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := providers.WithWriteCheckpoint(ctx, func(context.Context) error { return tc.err }, errStopped)
			_, err := h.p.do(c, call{op: opCommitFile, method: http.MethodPost, path: blobsPath})
			if !errors.Is(err, tc.class) || providers.IsRetryable(err) {
				t.Fatalf("got %v, want the class %v", err, tc.class)
			}
			if strings.Contains(err.Error(), "10.0.0.1") || strings.Contains(err.Error(), "dial") ||
				strings.Contains(err.Error(), "connection refused") {
				t.Fatalf("the checkpoint's text reached the adapter's error: %v", err)
			}
			assertSafe(t, err)
		})
	}
}
