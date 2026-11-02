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

package providers

import "context"

// SignatureAlgorithm names the algorithm a Signer uses.
type SignatureAlgorithm string

// RS256 is RSASSA-PKCS1-v1_5 with SHA-256, as GitHub App JWTs require.
const RS256 SignatureAlgorithm = "RS256"

// Signer signs with a key that never leaves its key store (ADR-0006).
type Signer interface {
	// Algorithm reports the signature algorithm of the key.
	Algorithm() SignatureAlgorithm
	// Sign signs a digest that the caller has already computed (SHA-256 for RS256).
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}
