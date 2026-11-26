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

package azurekv

import (
	"context"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/printguard"
)

// A tokenFile that holds the projected service account token it read shows none of it in any printed,
// logged or encoded form, and still hands it out.
func TestTokenFileNeverPrintsTheToken(t *testing.T) {
	tf := &tokenFile{path: writeTokenFile(t, testAssertion)}
	if got, err := tf.assertion(context.Background()); err != nil || got != testAssertion {
		t.Fatalf("assertion = %q, %v", got, err)
	}
	printguard.Check(t, tf, testAssertion)
	if got, err := tf.assertion(context.Background()); err != nil || got != testAssertion {
		t.Errorf("reused assertion = %q, %v; want the token", got, err)
	}
}

// A Signer whose workload credential has read the token file and fetched an access token, and which has
// signed with it, shows neither in any printed, logged or encoded form.
func TestSignerNeverPrintsTheCredential(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app"
	entra := &fakeEntra{}
	s, v := setupWith(t, kid, Options{MaxRetries: -1}, realCredential(t, writeTokenFile(t, testAssertion), entra), nil,
		clientConfig{})
	v.respond = v.signOK(kid + "/" + testVersion)
	if _, err := s.Sign(context.Background(), digestOf("x")); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(entra.assertions) != 1 {
		t.Fatalf("%d token requests, want 1", len(entra.assertions))
	}
	printguard.Check(t, s, testAssertion)
	printguard.Check(t, s, testToken)
}
