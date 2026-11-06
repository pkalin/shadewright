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

import (
	"context"
	"time"
)

// AnalysisRecord is one redacted analysis, kept outside the cluster (ADR-0015).
type AnalysisRecord struct {
	// ID is unique per analysis and chosen by the caller, so a retried write is recognised.
	ID string
	// Fingerprint is the incident fingerprint the analysis belongs to.
	Fingerprint string
	CreatedAt   time.Time
	// Data is the redacted record, typically JSON.
	Data []byte
	// Verified are values in Data that the caller made or read back from the API server and knows are not
	// secrets, such as an object name that ends in a hash. A store that checks Data with the redactor before
	// it writes keeps their whole occurrences. Never text from a model, a user or the cluster.
	Verified []string
}

// AnalysisStore keeps analysis records. Records are written once and never changed.
type AnalysisStore interface {
	// Put writes a record and returns a reference to it. Writing the same ID with the same data again
	// returns the same reference; the same ID with other data returns ErrConflict. A store whose identity
	// may not read records cannot compare, and returns ErrConflict for any ID that exists.
	Put(ctx context.Context, rec AnalysisRecord) (string, error)
}
