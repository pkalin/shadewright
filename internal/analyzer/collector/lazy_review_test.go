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

package collector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// reviewHost is the API server of the review tests; no request reaches it.
const reviewHost = "http://review.invalid"

// reviewTransport is an http.RoundTripper from a function.
type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// reviewResponse is a JSON response with code and body.
func reviewResponse(r *http.Request, code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// TestWatchThrottledAfterSyncIsReported (#409 review): after the first sync, every new watch is answered with
// a 429, which the reflector retries without calling its watch error handler. The watch delivers nothing, so
// it is reported as failed.
func TestWatchThrottledAfterSyncIsReported(t *testing.T) { undeliverableWatch(t, false) }

// TestWatchRefusedInStreamAfterSyncFails (#409 review): after the first sync, every new watch streams an ERROR
// event with a refusal, which the reflector logs without calling its watch error handler. The watch is stopped
// and reported as failed, with the backoff.
func TestWatchRefusedInStreamAfterSyncFails(t *testing.T) { undeliverableWatch(t, true) }

// undeliverableWatch runs a real lazy watch whose watches after the first are all refused, with a 429 or, with
// streamError, with a Forbidden ERROR event.
func undeliverableWatch(t *testing.T, streamError bool) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	var watches atomic.Int32
	httpClient := &http.Client{Transport: reviewTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("watch") == "true" {
			if watches.Add(1) == 1 {
				return reviewResponse(r, 200, `{"type":"BOOKMARK","object":{"apiVersion":"kyverno.io/v1","kind":"Policy","metadata":{"resourceVersion":"1","annotations":{"k8s.io/initial-events-end":"true"}}}}`+"\n"), nil
			}
			if streamError {
				return reviewResponse(r, 200, `{"type":"ERROR","object":{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","code":403,"message":"watch refused"}}`+"\n"), nil
			}
			return reviewResponse(r, 429, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"TooManyRequests","code":429,"message":"watch throttled"}`), nil
		}
		return reviewResponse(r, 200, `{"apiVersion":"kyverno.io/v1","kind":"PolicyList","metadata":{"resourceVersion":"1"},"items":[]}`), nil
	})}
	h.l.start = startController(&rest.Config{Host: reviewHost}, func() cache.Options {
		return cache.Options{Scheme: runtime.NewScheme(), Mapper: h.mapper, HTTPClient: httpClient, DefaultNamespaces: map[string]cache.Config{testNS: {}}}
	}, h.l.log)
	h.step()
	// Wait until the watch's own transport has recorded the outcome of the second watch, not only until the
	// fake API server answered it (#563): the 429 is recorded after the answer returns, and the refusal only
	// once the reflector reads the stream. The 429 also counts only after the informer's sync was seen, which
	// WaitForCacheSync polls for. Checks run before either would see a healthy watch and pass by too soon.
	deadline := time.Now().Add(10 * time.Second)
	for watches.Load() < 2 || !undeliverableSeen(h) {
		if time.Now().After(deadline) {
			t.Fatalf("the refusal of the second watch was not recorded, %d watches, state=%s", watches.Load(), h.state())
		}
		time.Sleep(10 * time.Millisecond)
		h.step()
	}
	for range failedAfterChecks + 1 {
		h.step()
	}
	if h.state() != WatchFailed {
		t.Fatalf("watch delivers no object events, every new watch is rejected, state=%s, want Failed", h.state())
	}
}

// undeliverableSeen reports whether the lazy source of h already stopped its watch, or runs a synced watch whose
// transport recorded an error of a request after the sync. It reads h's running watch, so it is called from the
// goroutine that steps h.
func undeliverableSeen(h *lazyHarness) bool {
	w := h.l.running
	if w == nil {
		return h.state() == WatchFailed
	}
	select {
	case <-w.synced:
	default:
		return false
	}
	fatal, transient := w.errs.take()
	return fatal != nil || transient != nil
}

// reviewBlockedReader blocks its first List until release is closed or ctx ends.
type reviewBlockedReader struct {
	client.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *reviewBlockedReader) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestStoppedWatchIsReportedBeforeTheCheck (#409 review): once a watch's controller stopped and was cleaned up,
// the kind is reported as failed before the check that follows, not only after it: no controller runs while
// it waits for the API server.
func TestStoppedWatchIsReportedBeforeTheCheck(t *testing.T) {
	h := watching(t)
	h.run().finish(errors.New("cache stopped"))
	reader := &reviewBlockedReader{Reader: h.api, entered: make(chan struct{}), release: make(chan struct{})}
	h.l.api = reader
	finished := make(chan struct{})
	go func() { h.step(); close(finished) }()
	<-reader.entered
	state := h.state()
	close(reader.release)
	<-finished
	if state == WatchWatched {
		t.Fatal("controller stopped and was cancelled, but state stayed Watched throughout the follow-up probe")
	}
}

// TestStalledDiscoveryBlocksNoOtherSourceOrStop (#409 review): a discovery request for an add-on group that
// stalls neither holds up a mapping another source reads from the manager's RESTMapper nor the lazy source's
// stop.
func TestStalledDiscoveryBlocksNoOtherSourceOrStop(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	httpClient := &http.Client{Transport: reviewTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api":
			return reviewResponse(r, 200, `{"apiVersion":"v1","kind":"APIVersions","versions":["v1"]}`), nil
		case "/apis":
			return reviewResponse(r, 200, `{"apiVersion":"v1","kind":"APIGroupList","groups":[{"name":"kyverno.io","versions":[{"groupVersion":"kyverno.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"kyverno.io/v1","version":"v1"}}]}`), nil
		case "/api/v1":
			return reviewResponse(r, 200, `{"apiVersion":"v1","kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"pods","kind":"Pod","namespaced":true}]}`), nil
		default:
			once.Do(func() { close(entered) })
			select {
			case <-release:
				return reviewResponse(r, 404, `{"apiVersion":"v1","kind":"Status","reason":"NotFound","code":404}`), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
	})}
	mapper, err := apiutil.NewDynamicRESTMapper(&rest.Config{Host: reviewHost}, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapper.RESTMapping(schema.GroupKind{Kind: kindPod}, "v1"); err != nil {
		t.Fatal(err)
	}
	if h.l.discover, err = discoverMapping(&rest.Config{Host: reviewHost}, httpClient); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { _ = h.l.Start(ctx); close(finished) }()
	<-entered
	other := make(chan struct{})
	go func() { _, _ = mapper.RESTMapping(schema.GroupKind{Kind: kindPod}, "v1"); close(other) }()
	cancel()
	select {
	case <-finished:
	case <-time.After(100 * time.Millisecond):
		t.Error("lazy source cannot stop after cancellation while discovery is blocked")
	}
	select {
	case <-other:
	case <-time.After(100 * time.Millisecond):
		t.Error("a cached Pod mapping for another source is blocked by add-on discovery")
	}
	close(release)
	<-finished
	<-other
}
