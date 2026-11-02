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
// Package fingerprint turns an observation into a deterministic incident fingerprint and a normalized,
// redacted symptom (design section 3.2, ADR-0007), so that the same problem on the same workload always
// maps to the same Incident.
//
// Both functions are pure: no I/O, no clock, no randomness. The observation text comes from the cluster
// and is untrusted (invariant 4); [Normalize] redacts anything that looks like a token or connection string
// before the text can be stored or shown to a model.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// Prefix is the algorithm prefix of every fingerprint, as required by the Incident CRD pattern.
const Prefix = "sha256:"

// version is hashed first, so a change to the inputs or the normalization rules yields new fingerprints
// instead of silently merging old and new incidents.
const version = "shadewright.io/fingerprint/v1"

// Observation is one symptom as seen by the collector: an event or an object status condition.
type Observation struct {
	// ClusterID identifies the cluster the observation comes from.
	ClusterID string
	// Namespace of the object the observation is about; empty for cluster-scoped objects.
	Namespace string
	// OwnerUID is the UID of the owning workload (for example the Deployment of a pod), when known.
	OwnerUID string
	// ResourceUID is the UID of the object itself; used when OwnerUID is empty.
	ResourceUID string
	// Reason is the event or status reason, for example FailedScheduling or OOMKilled.
	Reason string
	// Action is the event action, when set.
	Action string
	// Type is the event type: Normal or Warning.
	Type string
	// ReportingController is the component that reported the symptom.
	ReportingController string
	// Note is the free-text message (event note or status message). It is untrusted and not redacted.
	Note string
}

// Fingerprint returns "sha256:" followed by 64 lowercase hex digits, computed from the cluster ID,
// namespace, owner UID (or resource UID when there is no owner), reason, action, type, reporting
// controller and the normalized symptom. Every field is length-prefixed, so no two different inputs share
// an encoding.
func Fingerprint(o Observation) string {
	subject := o.OwnerUID
	if subject == "" {
		subject = o.ResourceUID
	}
	h := sha256.New()
	for _, field := range []string{
		version,
		o.ClusterID,
		o.Namespace,
		subject,
		o.Reason,
		o.Action,
		o.Type,
		o.ReportingController,
		Normalize(o),
	} {
		writeField(h, field)
	}
	return Prefix + hex.EncodeToString(h.Sum(nil))
}

func writeField(h hash.Hash, field string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(field)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(field))
}
