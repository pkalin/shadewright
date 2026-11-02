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

// Package digest computes the plan digest of a RemediationRequest (design section 5.1, ADR-0003).
//
// The digest binds an approval to one exact plan: the typed action, the target UID, the resolved source with
// the repository URL and provider installation, and the policy UID and generation. The same plan always gives
// the same digest, and a change to any input gives a different one.
//
// The input is written in a canonical encoding before hashing. Every field is written explicitly, in a fixed
// order, as a labelled length-prefixed value, so the result does not depend on Go struct field order, map
// order or JSON formatting, and no two different inputs share an encoding. Quantities are written by value,
// so 256Mi and 268435456 give the same digest. The encoding starts with a version label; a change to it is a
// new version, which supersedes every plan digested with the old one.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

// Prefix starts every digest.
const Prefix = "sha256:"

// encodingVersion labels the canonical encoding. Change it whenever the encoding changes.
const encodingVersion = "remediation.shadewright.io/plan-digest/v1"

// Input holds everything a plan digest covers.
type Input struct {
	// Action is the typed change: its type and parameters.
	Action v1alpha1.Action

	// TargetUID is the UID of the live object the change is about.
	TargetUID v1alpha1.UID

	// Source is where the change is made, with the repository settings it depends on.
	Source Source

	// PolicyUID is the UID of the policy that allowed the plan.
	PolicyUID v1alpha1.UID

	// PolicyGeneration is the policy's metadata.generation when it allowed the plan.
	PolicyGeneration int64
}

// Source is the resolved source of a plan (ADR-0012) together with the repository URL and provider
// installation taken from RemediationSettings.
type Source struct {
	// RepositoryRef is the RemediationSettings repository entry.
	RepositoryRef v1alpha1.Name

	// URL is the repository URL from RemediationSettings.
	URL string

	// InstallationID is the provider installation that covers the repository, for example the GitHub App
	// installation ID.
	InstallationID int64

	// TargetBranch is the branch the pull request targets.
	TargetBranch v1alpha1.GitBranch

	// Path is the file that declares the target.
	Path v1alpha1.GitFilePath

	// BaseCommit is the full SHA of the target branch the change is made against.
	BaseCommit string
}

// Compute returns the plan digest of in: "sha256:" followed by 64 lower-case hex characters.
func Compute(in Input) string {
	sum := sha256.Sum256(encode(in))
	return Prefix + hex.EncodeToString(sum[:])
}

// encode writes in in the canonical encoding. Each field is a label and a value, both written as
// "<length>:<bytes>", so the byte stream decodes in exactly one way. Optional members are written as
// "present" or "absent" before their fields.
func encode(in Input) []byte {
	var e encoder
	e.field("version", encodingVersion)

	e.field("action.type", string(in.Action.Type))
	if p := in.Action.AdjustKedaMaxReplicas; p != nil {
		e.field("action.adjustKedaMaxReplicas", "present")
		e.field("action.adjustKedaMaxReplicas.previousValue", strconv.FormatInt(int64(p.PreviousValue), 10))
		e.field("action.adjustKedaMaxReplicas.desiredValue", strconv.FormatInt(int64(p.DesiredValue), 10))
	} else {
		e.field("action.adjustKedaMaxReplicas", "absent")
	}
	if p := in.Action.SetContainerMemoryLimit; p != nil {
		e.field("action.setContainerMemoryLimit", "present")
		e.field("action.setContainerMemoryLimit.container", p.Container)
		e.field("action.setContainerMemoryLimit.previous", canonicalQuantity(p.Previous))
		e.field("action.setContainerMemoryLimit.desired", canonicalQuantity(p.Desired))
	} else {
		e.field("action.setContainerMemoryLimit", "absent")
	}

	e.field("targetUID", string(in.TargetUID))

	e.field("source.repositoryRef", string(in.Source.RepositoryRef))
	e.field("source.url", in.Source.URL)
	e.field("source.installationID", strconv.FormatInt(in.Source.InstallationID, 10))
	e.field("source.targetBranch", string(in.Source.TargetBranch))
	e.field("source.path", string(in.Source.Path))
	e.field("source.baseCommit", in.Source.BaseCommit)

	e.field("policy.uid", string(in.PolicyUID))
	e.field("policy.generation", strconv.FormatInt(in.PolicyGeneration, 10))

	return []byte(e.b.String())
}

// canonicalQuantity writes a quantity as a plain decimal number with no exponent, suffix, trailing
// fractional zeros or leading plus sign, so equal values always give the same text.
func canonicalQuantity(q resource.Quantity) string {
	s := q.AsDec().String()
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

type encoder struct {
	b strings.Builder
}

func (e *encoder) field(label, value string) {
	e.bytes(label)
	e.bytes(value)
}

func (e *encoder) bytes(s string) {
	e.b.WriteString(strconv.Itoa(len(s)))
	e.b.WriteByte(':')
	e.b.WriteString(s)
}
