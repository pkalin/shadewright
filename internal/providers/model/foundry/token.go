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

package foundry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkalin/shadewright/internal/providers"
)

const maxTokenBytes = 16 << 10

// checkToken checks an access token before it is sent: a header-safe JWT whose audience is Cognitive
// Services. The payload is decoded, not verified: the endpoint verifies the signature; this check only keeps
// a token for another resource (or anything that is not a token) from being sent. Errors are fixed text
// matching providers.ErrForbidden, whichever TokenSource the token came from: the token is the identity's
// credential, so a wrong one is fixed by a person on the identity (its federation, scope or source), like a
// 401 or 403. ErrInvalidConfig stays for the settings the Provider is built from.
func checkToken(tok string) error {
	refuse := func(msg string) error { return fmt.Errorf("foundry: %s: %w", msg, providers.ErrForbidden) }
	if tok == "" || len(tok) > maxTokenBytes {
		return refuse("the access token is empty or too large")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return refuse("the access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return refuse("the access token is not a JWT")
	}
	for _, p := range []string{parts[0], parts[2]} {
		if _, err := base64.RawURLEncoding.DecodeString(p); err != nil || p == "" {
			return refuse("the access token is not a JWT")
		}
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return refuse("the access token is not a JWT")
	}
	var auds []string
	var one string
	if err := json.Unmarshal(claims.Aud, &one); err == nil {
		auds = []string{one}
	} else if err := json.Unmarshal(claims.Aud, &auds); err != nil {
		return refuse("the access token has no audience")
	}
	for _, a := range auds {
		if strings.TrimSuffix(a, "/") == cognitiveServicesAudience {
			return nil
		}
	}
	return refuse("the access token is not for the Cognitive Services audience")
}
