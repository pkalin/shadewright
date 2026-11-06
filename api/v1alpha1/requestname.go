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

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// RequestName is the name the analyzer gives the RemediationRequest of a plan: "remediation-" and, in hex, the
// first 12 bytes of the SHA-256 of spec.incidentRef.uid, "/" and the JSON of the plan's part of spec. The plan's
// part is every field but verification.timeout and verification.stableFor, which the analyzer leaves to the API
// server's defaults, so the name computed from a request read back from the API server is the one the analyzer
// computed before it created it. The analyzer names its requests with it, so a retried create finds the request
// already made (invariant 5); the executor shows a request's name unredacted in a pull request body only when
// it is this derivation of the request's own spec, never a name its creator chose.
func RequestName(spec RemediationRequestSpec) (Name, error) {
	plan := spec
	plan.Verification = Verification{Checks: spec.Verification.Checks}
	b, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encoding the request spec: %w", err)
	}
	h := sha256.Sum256(append([]byte(string(spec.IncidentRef.UID)+"/"), b...))
	return Name("remediation-" + hex.EncodeToString(h[:12])), nil
}
