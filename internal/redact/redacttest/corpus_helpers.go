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
	"slices"
)

// BlockCorpus returns block cases with their generator-owned byte ranges.
var BlockCorpus = blockCorpus

// FormCorpus returns block form cases with their generator-owned byte ranges.
var FormCorpus = formCorpus

// BlockSecretKeys returns a copy of the secret keys placed by the block generator.
func BlockSecretKeys() []string { return slices.Clone(blockSecretKeys) }

// BlockInnocentKeys returns a copy of the innocent keys placed by the block generator.
func BlockInnocentKeys() []string { return slices.Clone(blockInnocentKeys) }

// BlockBareKey reports whether the block generator treats key as a bare short key.
var BlockBareKey = isBareShortCorpusKey

// FullCanonCorpus returns all canonical cases, without sampling.
var FullCanonCorpus = canonCorpus

// CanonHash returns the fixture hash of a canonical case.
var CanonHash = canonHash

// SampleCanon selects the canonical cases for this run.
var SampleCanon = canonSample

// CanonIndexOf indexes canonical cases by their fixture hashes.
var CanonIndexOf = canonIndexOf

// CanonForms returns a copy of the canonical forms in generator order.
func CanonForms() []string { return slices.Clone(canonForms) }

// CanonPhraseWords returns a copy of the synthetic words placed in phrase secrets.
func CanonPhraseWords() []string { return slices.Clone(canonPhraseWords) }

// CanonFlagKeys returns a copy of the keys placed in flag forms.
func CanonFlagKeys() []string { return slices.Clone(canonFlagKeys) }

// CanonGlyphKeys returns a copy of the keys placed in folded forms.
func CanonGlyphKeys() []string { return slices.Clone(canonGlyphKeys) }

// CanonInvisibles returns a copy of the invisible characters placed in folded forms.
func CanonInvisibles() []string { return slices.Clone(canonInvisibles) }

// CanonSepFound returns a copy of the stored separators included in the corpus.
func CanonSepFound() []string { return slices.Clone(canonSepFound) }

// CanonSepKeys returns a copy of the keys placed in separator forms.
func CanonSepKeys() []string { return slices.Clone(canonSepKeys) }

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

// FormKeys returns a copy of the keys placed by the block form generator.
func FormKeys() []string { return slices.Clone(formKeys) }

// FormIndicators returns a copy of the block headers placed by the form generator.
func FormIndicators() []string { return slices.Clone(formIndicators) }

const (
	quotedPasswordKey = `"password"`
	credentialsKey    = "credentials"
	passwordFlag      = "--password"
)
