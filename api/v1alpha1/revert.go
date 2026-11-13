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
)

// Reasons of a revert (ADR-0030 section 8).
const (
	// ReasonInvalidRevert: a request with spec.reverts was Denied before the policy check because the link does
	// not hold: the named request is missing, has another UID, did not end Failed with Verified False, is a
	// revert itself, names another Incident, target, action type or container, or this request is not exactly
	// its inverse or not named RevertName of its UID. Such a request is never treated as a revert.
	ReasonInvalidRevert = "InvalidRevert"
	// ReasonRevertApplied: a revert is never checked for clearing (Cleared Unknown), and it ends Inconclusive
	// with this reason once rolled out, configured and available held for stableFor (Verified Unknown).
	ReasonRevertApplied = "RevertApplied"
)

// RevertName is the name of the revert of the request with UID failed: "revert-" and, in hex, the first 12
// bytes of the SHA-256 of "revert/" and that UID. The name derives from the failed request only, so a retried or
// repeated create finds the revert already made and a failed request has at most one revert (invariant 5).
func RevertName(failed UID) Name {
	h := sha256.Sum256([]byte("revert/" + string(failed)))
	return Name("revert-" + hex.EncodeToString(h[:12]))
}

// IsRevert reports whether rr is a revert: its spec.reverts is set.
func IsRevert(rr *RemediationRequest) bool {
	return rr != nil && rr.Spec.Reverts != nil
}
