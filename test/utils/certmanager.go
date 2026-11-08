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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	certmanagerVersion = "v1.21.1"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	// certmanagerSHA256 is the SHA-256 of cert-manager.yaml of the release above. Update it together with the
	// version and certmanagerImageDigests: download the release asset, run sha256sum on it and read the image
	// digests with `docker buildx imagetools inspect <image>:<version>` (or `crane digest`).
	certmanagerSHA256 = "5f6a499b8c1857d57f560f536e0dcc830914b45c420899fe7ad0692c8624e408"

	// certmanagerMaxBytes bounds the download; the release manifest is about 1 MiB.
	certmanagerMaxBytes = 16 << 20
)

// certmanagerImageDigests maps every image the manifest names to the digest of that tag, so a re-pushed tag
// cannot change what runs in the e2e cluster.
var certmanagerImageDigests = map[string]string{
	"quay.io/jetstack/cert-manager-controller": "sha256:416a2d76870d996460e62bd7f521bf14fa017be9e3e904aab92163a331fcb61a",
	"quay.io/jetstack/cert-manager-cainjector": "sha256:ccf6b919ec0500745a47a910118f834f9636d0aac1ff221245cd2557ed8c7c98",
	"quay.io/jetstack/cert-manager-webhook":    "sha256:d8b3961b51c8c7320633f8208dc46bf88aa13804d0f7cbe48a096b2c523cee42",
	"quay.io/jetstack/cert-manager-acmesolver": "sha256:dbc7cc1354f603918e7c5af7f55a0a620537394452c93a565bde75c6f48e8837",
}

// verifySHA256 returns an error unless the SHA-256 of data equals the expected hex digest.
func verifySHA256(data []byte, expected string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("checksum mismatch: expected sha256 %s, got %s; refusing to use the manifest", expected, got)
	}
	return nil
}

// pinImages rewrites every `<image>:<tag>` reference named in digests to `<image>:<tag>@<digest>`. It fails
// when a listed image does not appear in the manifest (stale pin) and when the manifest names a quay.io image
// that is not pinned (new image, would stay unpinned).
func pinImages(manifest string, version string, digests map[string]string) (string, error) {
	for image, digest := range digests {
		ref := image + ":" + version
		if !strings.Contains(manifest, ref) {
			return "", fmt.Errorf("image %s not found in the manifest; update the pinned digests", ref)
		}
		manifest = strings.ReplaceAll(manifest, ref, ref+"@"+digest)
	}
	for line := range strings.SplitSeq(manifest, "\n") {
		for _, field := range strings.FieldsFunc(line, func(r rune) bool { return r == '"' || r == '=' || r == ' ' }) {
			if strings.HasPrefix(field, "quay.io/") && !strings.Contains(field, "@sha256:") {
				return "", fmt.Errorf("image %s in the manifest is not pinned by digest", field)
			}
		}
	}
	return manifest, nil
}

// Download retries: the release CDN answered 503 and 504 for about two hours on 2026-11-08 (#543).
const (
	downloadAttempts   = 5
	downloadAttemptMax = time.Minute
	downloadRetryDelay = 10 * time.Second
)

// fetchCertManagerManifest downloads the pinned release manifest, verifies its checksum and returns it with the
// images pinned by digest. Nothing is returned for a manifest that fails verification.
func fetchCertManagerManifest() (string, error) {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	data, err := download(ctx, http.DefaultClient, url, certmanagerMaxBytes, downloadAttempts, downloadRetryDelay)
	if err != nil {
		return "", err
	}
	if err := verifySHA256(data, certmanagerSHA256); err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	return pinImages(string(data), certmanagerVersion, certmanagerImageDigests)
}

// errPermanent marks a download failure that a retry cannot fix.
type errPermanent struct{ err error }

func (e errPermanent) Error() string { return e.err.Error() }
func (e errPermanent) Unwrap() error { return e.err }

// download fetches url, at most maxBytes, trying up to attempts times. A network error or a 5xx answer is
// retried after delay; any other status, an oversized body or the end of ctx stops at once.
func download(ctx context.Context, client *http.Client, url string, maxBytes, attempts int,
	delay time.Duration) ([]byte, error) {
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		var data []byte
		data, err = downloadOnce(ctx, client, url, maxBytes)
		if err == nil {
			return data, nil
		}
		var permanent errPermanent
		if errors.As(err, &permanent) || attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (after %d attempts, last: %w)", ctx.Err(), attempt, err)
		case <-time.After(delay):
		}
	}
	return nil, err
}

func downloadOnce(ctx context.Context, client *http.Client, url string, maxBytes int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadAttemptMax)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errPermanent{err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("downloading %s: unexpected status %s", url, resp.Status)
		if resp.StatusCode >= http.StatusInternalServerError {
			return nil, err
		}
		return nil, errPermanent{err}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	if len(data) > maxBytes {
		return nil, errPermanent{fmt.Errorf("%s is larger than %d bytes", url, maxBytes)}
	}
	return data, nil
}
