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

// Package mainsymptom is a frozen copy of the stored-symptom reading of origin/main at commit 24cd00a6:
// fingerprint.Symptom, fingerprint.Value (normalize.go, copied byte for byte but its package clause) and
// modules.BoundValue (bound.go). The differential tests of the stored symptom compare the current reading with
// it: the current reading must never show a four-byte part of a secret that this reading hides. It is used by
// tests only. Do not edit it: a change here changes the baseline the stored symptom is held to.
package mainsymptom

import "github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"

// Observation is fingerprint.Observation; the frozen reading uses its Note, RegardingKind and RegardingName.
type Observation = fingerprint.Observation
