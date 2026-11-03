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
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
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
