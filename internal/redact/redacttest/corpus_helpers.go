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

package redacttest

import (
	"math/rand/v2"
)

// BlockCorpus returns block cases with their generator-owned byte ranges.
var BlockCorpus = blockCorpus

// FormCorpus returns block form cases with their generator-owned byte ranges.
var FormCorpus = formCorpus

// BlockSecretKeys are the secret keys placed by the block generator.
var BlockSecretKeys = blockSecretKeys

// BlockInnocentKeys are the innocent keys placed by the block generator.
var BlockInnocentKeys = blockInnocentKeys

// BlockBareKey reports the bare short keys in the fixed block corpus.
var BlockBareKey = isBareShortCorpusKey

// FullCanonCorpus returns all canonical cases, without sampling.
var FullCanonCorpus = canonCorpus

// CanonHash returns the fixture hash of a canonical case.
var CanonHash = canonHash

// SampleCanon selects the canonical cases for this run.
var SampleCanon = canonSample

// CanonIndexOf indexes canonical cases by their fixture hashes.
var CanonIndexOf = canonIndexOf

// CanonForms lists the canonical forms in generator order.
var CanonForms = canonForms

// CanonPhraseWords are the synthetic words placed in phrase secrets.
var CanonPhraseWords = canonPhraseWords

// CanonFlagKeys are the keys placed in flag forms.
var CanonFlagKeys = canonFlagKeys

// CanonGlyphKeys are the keys placed in folded forms.
var CanonGlyphKeys = canonGlyphKeys

// CanonInvisibles are the invisible characters placed in folded forms.
var CanonInvisibles = canonInvisibles

// CanonSepFound are the stored separators included in the corpus.
var CanonSepFound = canonSepFound

// CanonSepKeys are the keys placed in separator forms.
var CanonSepKeys = canonSepKeys

// CanonPartialMin is the minimum prefix or suffix checked for a partial leak.
const CanonPartialMin = canonPartialMin

// CanonSampleDivisor is the share selected in short mode.
const CanonSampleDivisor = canonSampleDivisor

// CanonPick draws one string with the caller's seeded generator.
var CanonPick = canonPick

// CanonSeparatorPlain reports the ordinary ASCII separators.
var CanonSeparatorPlain = sepRunPlain

// CanonSeparatorRun draws one separator with the caller's seeded generator.
func CanonSeparatorRun(rng *rand.Rand) string { return (&canonGen{rng: rng}).sepRun() }

const (
	clientSecretKey = "client_secret"
	nlIndent        = "\n  "
)
const canonCheckSecret = "secret"

const nlDash = "\n- "

// FormKeys are the keys placed by the block form generator.
var FormKeys = formKeys

// FormIndicators are the block headers placed by the form generator.
var FormIndicators = formIndicators

const (
	quotedPasswordKey = `"password"`
	credentialsKey    = "credentials"
	passwordFlag      = "--password"
)
