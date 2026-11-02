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

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

const (
	// jwtBackdate is how far iat lies in the past, to absorb clock skew between this host and GitHub.
	jwtBackdate = 60 * time.Second
	// jwtLifetime is exp - iat. GitHub allows at most 10 minutes after the time it receives the JWT; exp is
	// set 9 minutes ahead of now and iat 60 seconds behind it, so the total is exactly 10 minutes counted
	// from iat and 9 from now.
	jwtLifetime = 9 * time.Minute
)

var b64 = base64.RawURLEncoding

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Iss string `json:"iss"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// buildJWT returns the signed App JWT. The signing input is hashed here with SHA-256 and the digest is
// signed through the Signer, so the private key stays in its key store.
func buildJWT(ctx context.Context, signer providers.Signer, clientID string, now time.Time) (string, error) {
	header, err := json.Marshal(jwtHeader{Alg: "RS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("encode JWT header: %w", err)
	}
	claims, err := json.Marshal(jwtClaims{
		Iss: clientID,
		Iat: now.Add(-jwtBackdate).Unix(),
		Exp: now.Add(jwtLifetime).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("encode JWT claims: %w", err)
	}
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := signer.Sign(ctx, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	if len(sig) == 0 {
		return "", fmt.Errorf("sign GitHub App JWT: signer returned an empty signature")
	}
	return input + "." + b64.EncodeToString(sig), nil
}
