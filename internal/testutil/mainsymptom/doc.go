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

// Package mainsymptom is the stored-symptom reading of origin/main at commit 24cd00a6, for tests only. What is
// frozen here is the code around the redaction: fingerprint.Symptom and fingerprint.Value with their normalisation
// (normalize.go, copied byte for byte but its package clause) and modules.BoundValue (bound.go). The redaction
// itself is not copied: these functions call the live redact package (redact.ReplaceCheckedShort and
// redact.StringShort), so a change of a redaction rule changes this reading too.
//
// That part is pinned by origin/main's real outputs instead, written by building that commit from an archive
// (hack/symptom-main-baseline.sh): internal/modules/testdata/symptom-main-regressions.tsv holds its Symptom, Value
// and BoundValue of each of the 61 regression inputs of the differential, and symptom-main-generated.sha256 one
// SHA-256 over its outputs on the 60,000 generated inputs. TestFrozenReadingIsOriginMain (package modules) fails
// when this reading no longer reproduces them, and TestStoredSymptomRegressionsShowNothingMainHides compares the
// current reading with the table itself. The differential tests of the stored symptom compare the current reading
// with this one: the current reading must never show a four-byte part of a secret that this reading hides. Do not
// edit this package, and do not regenerate the baseline to make a test pass: either changes what the stored
// symptom is held to, and is a reviewed decision.
package mainsymptom

import "github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"

// Observation is fingerprint.Observation; the frozen reading uses its Note, RegardingKind and RegardingName.
type Observation = fingerprint.Observation
