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

package utils

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifySHA256(t *testing.T) {
	data := []byte("kind: Namespace\n")
	sum := sha256.Sum256(data)
	good := hex.EncodeToString(sum[:])

	if err := verifySHA256(data, good); err != nil {
		t.Fatalf("matching checksum refused: %v", err)
	}
	err := verifySHA256([]byte("kind: Namespace\nmetadata: tampered\n"), good)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered manifest must be refused with a checksum error, got %v", err)
	}
	if err := verifySHA256(data, ""); err == nil {
		t.Fatal("empty expected checksum must be refused")
	}
}

func TestPinImages(t *testing.T) {
	digests := map[string]string{"quay.io/x/a": "sha256:aaa"}
	out, err := pinImages(`image: "quay.io/x/a:v1"`, "v1", digests)
	if err != nil || out != `image: "quay.io/x/a:v1@sha256:aaa"` {
		t.Fatalf("unexpected result %q, %v", out, err)
	}
	if _, err := pinImages(`image: "quay.io/x/b:v1"`, "v1", digests); err == nil {
		t.Fatal("missing pinned image must be refused")
	}
	if _, err := pinImages("image: quay.io/x/a:v1\nimage: quay.io/x/new:v1", "v1", digests); err == nil {
		t.Fatal("unpinned extra image must be refused")
	}
}

func TestCertManagerPinsAreWellFormed(t *testing.T) {
	if len(certmanagerSHA256) != 64 {
		t.Fatalf("certmanagerSHA256 must be 64 hex characters, got %d", len(certmanagerSHA256))
	}
	for image, d := range certmanagerImageDigests {
		if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
			t.Errorf("%s: malformed digest %q", image, d)
		}
	}
}

// failingServer answers status for the first failures requests, then body; it counts the requests.
func failingServer(t *testing.T, failures int32, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= failures {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestDownloadRetriesServerErrors(t *testing.T) {
	srv, calls := failingServer(t, 2, http.StatusServiceUnavailable, "manifest")
	data, err := download(context.Background(), srv.Client(), srv.URL, 1024, 5, time.Millisecond)
	if err != nil || string(data) != "manifest" {
		t.Fatalf("download = %q, %v; want the manifest after two 503 answers", data, err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestDownloadGivesUpAfterTheLastAttempt(t *testing.T) {
	srv, calls := failingServer(t, 10, http.StatusGatewayTimeout, "manifest")
	if _, err := download(context.Background(), srv.Client(), srv.URL, 1024, 3, time.Millisecond); err == nil ||
		!strings.Contains(err.Error(), "504") {
		t.Fatalf("err = %v, want the last 504", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestDownloadDoesNotRetryPermanentFailures(t *testing.T) {
	notFound, calls := failingServer(t, 10, http.StatusNotFound, "")
	if _, err := download(context.Background(), notFound.Client(), notFound.URL, 1024, 5, time.Millisecond); err == nil {
		t.Fatal("a 404 was accepted")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("requests after a 404 = %d, want 1", got)
	}
	big, bigCalls := failingServer(t, 0, http.StatusOK, strings.Repeat("x", 2048))
	if _, err := download(context.Background(), big.Client(), big.URL, 1024, 5, time.Millisecond); err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if got := bigCalls.Load(); got != 1 {
		t.Fatalf("requests after an oversized body = %d, want 1", got)
	}
}

func TestDownloadStopsWhenTheContextEnds(t *testing.T) {
	srv, calls := failingServer(t, 10, http.StatusServiceUnavailable, "manifest")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := download(ctx, srv.Client(), srv.URL, 1024, 5, time.Hour); err == nil {
		t.Fatal("download succeeded with an ended context")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("requests with an ended context = %d, want 0", got)
	}
}
