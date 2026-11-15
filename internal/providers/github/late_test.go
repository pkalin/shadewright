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
	"io"
	"net/http"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// endInBody passes requests to the script and ends a context on the first read of the answer's body for path,
// which then reads whole: the body of an answer taken after the call's context ended. No clock is needed.
type endInBody struct {
	inner http.RoundTripper
	path  string
	end   context.CancelFunc
}

func (e endInBody) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := e.inner.RoundTrip(r)
	if err == nil && r.URL.EscapedPath() == repoPath+e.path {
		resp.Body = io.NopCloser(io.MultiReader(endingReader{e.end}, resp.Body))
	}
	return resp, err
}

// endingReader ends a context on its first read and then reports EOF, so a MultiReader goes on to the body.
type endingReader struct{ end context.CancelFunc }

func (e endingReader) Read([]byte) (int, error) {
	e.end()
	return 0, io.EOF
}

func wantCanceled(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) || providers.IsNotFound(err) {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	assertSafe(t, err)
}

// A call whose context ended reports that end, whatever answer arrived after it (#779): neither a branch head
// nor a not found taken from that answer.
func TestAnswerAfterContextEnd(t *testing.T) {
	for _, st := range []step{get(mainRef, "ref_main.json"), status(http.MethodGet, mainRef, http.StatusNotFound, fxForbidden)} {
		t.Run(http.StatusText(st.status), func(t *testing.T) {
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			// The check runs inside the transport, before the answer is returned.
			st.check = func(*testing.T, map[string]any) { cancel() }
			h := newHarness(t, st)
			sha, err := h.p.BranchHead(cctx, target)
			if sha != "" {
				t.Errorf("BranchHead returned %q from an answer after the context ended", sha)
			}
			wantCanceled(t, err)
		})
	}
}

// A 2xx body read whole after the context ended is not taken, buffered or streamed (#779).
func TestBodyAfterContextEnd(t *testing.T) {
	t.Run("buffered", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		h := newHarness(t, get(mainRef, "ref_main.json"))
		h.p.client.Transport = endInBody{inner: h.s, path: mainRef, end: cancel}
		sha, err := h.p.BranchHead(cctx, target)
		if sha != "" {
			t.Errorf("BranchHead returned %q from a body read after the context ended", sha)
		}
		wantCanceled(t, err)
	})
	t.Run("streamed", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		h := newHarness(t, ownPR(), step{method: http.MethodGet, path: commentsPath, query: pageQuery(1),
			status: http.StatusOK, raw: "[]"})
		h.p.client.Transport = endInBody{inner: h.s, path: commentsPath, end: cancel}
		got, err := h.p.ListComments(cctx, prNumber, providers.FeedbackCursor{})
		if got.Complete || len(got.Items) != 0 {
			t.Errorf("ListComments returned %+v from a body read after the context ended", got)
		}
		wantCanceled(t, err)
	})
}
