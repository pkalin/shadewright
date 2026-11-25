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

package redact

import (
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// CanonCase is the shared canonical corpus case for entry-point tests.
type CanonCase = redacttest.CanonCase

// CanonCorpus returns the shared corpus or its seeded sample.
var CanonCorpus = redacttest.CanonCorpus

// CanonIndex returns the full corpus index by hash.
var CanonIndex = redacttest.CanonIndex

// CanonPartialMin is the shortest prefix or suffix checked for a partial leak.
const CanonPartialMin = redacttest.CanonPartialMin
