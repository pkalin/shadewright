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

// Package fluxstatus holds what Shadewright knows of Flux's API and how it reads the status of a Flux object:
// whether the object is Ready for its current generation, which condition says it is failing, and which
// revision it applied, attempted or fetched. It is the one definition used by the source mapping before a
// write (internal/gitops/sourcemap, ADR-0012), the Flux module's symptom rules (internal/modules/flux), the
// Flux delivery adapter after a merge (internal/providers/delivery/flux, design 7.3 and 8.2 step 8), and the
// rollout check that follows delivery (#532). It lives in a package of its own, like internal/kedaready, so
// that the core can use it without depending on the module registry or on an adapter.
//
// Names were read from the Flux sources at these tags: kustomize-controller v1.10.1, helm-controller v1.7.1,
// source-controller v1.10.1 and fluxcd/pkg apis/meta v1.38.0 (conditions.go: ReadyCondition, StalledCondition).
//
// It reads objects as plain JSON, as decoded into a map by encoding/json or by the API machinery, and only
// reads them. Every function fails closed: a field of another type than expected reads as absent, and an
// object of an unexpected shape is never Ready.
package fluxstatus

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/pkalin/shadewright/internal/providers"
)

// Flux API groups, versions and kinds read.
const (
	KustomizeGroup = "kustomize.toolkit.fluxcd.io"
	HelmGroup      = "helm.toolkit.fluxcd.io"
	SourceGroup    = "source.toolkit.fluxcd.io"

	KustomizeVersion = "v1"
	HelmVersion      = "v2"
	SourceVersion    = "v1"

	KindKustomization = "Kustomization"
	KindHelmRelease   = "HelmRelease"
	KindGitRepository = "GitRepository"
)

// Condition types and the status read (fluxcd/pkg apis/meta v1.38.0 conditions.go).
const (
	ConditionReady   = "Ready"
	ConditionStalled = "Stalled"
	statusTrue       = "True"
	statusFalse      = "False"
)

// ReasonHealthCheckCanceled is set on Ready=False only while a new reconciliation replaces the running one
// (kustomize-controller v1.10.1 internal/controller/kustomization_controller.go, helm-controller v1.7.1
// internal/controller/helmrelease_controller.go). It is no failure.
const ReasonHealthCheckCanceled = "HealthCheckCanceled"

// Generation returns metadata.generation, and false when it is absent or not an integer.
func Generation(obj map[string]any) (int64, bool) {
	meta, _ := obj["metadata"].(map[string]any)
	return Integer(meta["generation"])
}

// Status returns the object's status, or nil when it has none of the JSON shape expected.
func Status(obj map[string]any) map[string]any {
	status, _ := obj["status"].(map[string]any)
	return status
}

// ObservedGeneration returns status.observedGeneration, and false when it is absent or not an integer.
func ObservedGeneration(obj map[string]any) (int64, bool) {
	return Integer(Status(obj)["observedGeneration"])
}

// Ready reports whether the object's status describes its current generation (status.observedGeneration
// equals metadata.generation, an absent generation counting as 0) and its first Ready condition says True
// without naming another generation; an observedGeneration on it that is not an integer is not Ready.
func Ready(obj map[string]any) bool {
	generation, _ := Generation(obj)
	observed, found := ObservedGeneration(obj)
	if !found || observed != generation {
		return false
	}
	cond := firstCondition(Status(obj), ConditionReady)
	if cond == nil {
		return false
	}
	// An observedGeneration that is present but not an integer cannot show the condition is current.
	if v, present := cond["observedGeneration"]; present {
		if g, ok := Integer(v); !ok || g != generation {
			return false
		}
	}
	return cond["status"] == statusTrue
}

// Stale reports whether a condition describes an older spec: the object has a generation and the condition
// names another one in its observedGeneration. A condition without an observedGeneration is never stale.
func Stale(cond map[string]any, generation int64, hasGeneration bool) bool {
	observed, set := Integer(cond["observedGeneration"])
	return hasGeneration && set && observed != generation
}

// Failing returns the condition that says the object fails, and its type: Stalled=True first, then the first
// Ready=False, or nil when neither is set. Conditions that are [Stale] are skipped before choosing, so a stale
// Stalled=True never hides a current Ready=False. The caller decides what a Ready=False with
// [ReasonHealthCheckCanceled] means.
func Failing(status map[string]any, generation int64, hasGeneration bool) (map[string]any, string) {
	var ready map[string]any
	for _, cond := range conditions(status) {
		if Stale(cond, generation, hasGeneration) {
			continue
		}
		typ, _ := cond["type"].(string)
		st, _ := cond["status"].(string)
		switch {
		case typ == ConditionStalled && st == statusTrue:
			return cond, ConditionStalled
		case typ == ConditionReady && st == statusFalse && ready == nil:
			ready = cond
		}
	}
	if ready == nil {
		return nil, ""
	}
	return ready, ConditionReady
}

// ReadyMessage returns the message of the first Ready condition, or "". It is the controller's text and
// untrusted.
func ReadyMessage(obj map[string]any) string {
	msg, _ := firstCondition(Status(obj), ConditionReady)["message"].(string)
	return msg
}

// AppliedRevision returns status.lastAppliedRevision of a Kustomization or HelmRelease, or "".
func AppliedRevision(status map[string]any) string {
	s, _ := status["lastAppliedRevision"].(string)
	return s
}

// AttemptedRevision returns status.lastAttemptedRevision of a Kustomization or HelmRelease, or "".
func AttemptedRevision(status map[string]any) string {
	s, _ := status["lastAttemptedRevision"].(string)
	return s
}

// ArtifactRevision returns status.artifact.revision of a source, or "".
func ArtifactRevision(status map[string]any) string {
	artifact, _ := status["artifact"].(map[string]any)
	s, _ := artifact["revision"].(string)
	return s
}

// ParseRevision splits a revision of a Git source as Flux writes it: the reference (a branch or tag), "@",
// then the commit as sha1:<40 hex> or sha256:<64 hex>, lower case. It returns the reference and the commit ID,
// and false for anything else, an older or abbreviated format included.
func ParseRevision(revision string) (ref, commit string, ok bool) {
	i := strings.LastIndexByte(revision, '@')
	if i <= 0 {
		return "", "", false
	}
	id, ok := parseDigest(revision[i+1:])
	if !ok {
		return "", "", false
	}
	return revision[:i], id, true
}

// parseDigest returns the commit ID of sha1:<40 hex> or sha256:<64 hex>, lower case.
func parseDigest(digest string) (string, bool) {
	algo, id, found := strings.Cut(digest, ":")
	sized := algo == "sha1" && len(id) == providers.SHA1HexLen || algo == "sha256" && len(id) == providers.SHA256HexLen
	if !found || !sized || !providers.IsObjectID(id) {
		return "", false
	}
	return id, true
}

// ParseCommitRevision is [ParseRevision] that also accepts a revision naming only the commit,
// sha1:<40 hex> or sha256:<64 hex>, as Flux writes for a source fetched at a commit; its ref is empty.
func ParseCommitRevision(revision string) (ref, commit string, ok bool) {
	if !strings.Contains(revision, "@") {
		commit, ok = parseDigest(revision)
		return "", commit, ok
	}
	return ParseRevision(revision)
}

// RevisionOfBranch reports whether a revision of a Git source was fetched from branch (see [ParseRevision]).
func RevisionOfBranch(revision, branch string) bool {
	ref, _, ok := ParseRevision(revision)
	return ok && ref == branch
}

// AppliedFetched reports whether a Kustomization has applied the revision its GitRepository last fetched, and
// that revision was fetched from branch. Readiness of either object is not checked here.
func AppliedFetched(kustomization, source map[string]any, branch string) bool {
	applied := AppliedRevision(Status(kustomization))
	fetched := ArtifactRevision(Status(source))
	return applied != "" && applied == fetched && RevisionOfBranch(fetched, branch)
}

// SourceKind returns spec.sourceRef.kind of a Kustomization, or "".
func SourceKind(kustomization map[string]any) string {
	spec, _ := kustomization["spec"].(map[string]any)
	ref, _ := spec["sourceRef"].(map[string]any)
	kind, _ := ref["kind"].(string)
	return kind
}

// Integer returns a JSON number as an int64: an int64 or int as the API machinery decodes it, a float64
// without a fraction as encoding/json decodes it, or a json.Number. Anything else, or a value out of range,
// gives false.
func Integer(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n != math.Trunc(n) || n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// conditions returns the entries of status.conditions that are JSON objects, in order.
func conditions(status map[string]any) []map[string]any {
	list, _ := status["conditions"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		if cond, ok := c.(map[string]any); ok {
			out = append(out, cond)
		}
	}
	return out
}

// firstCondition returns the first condition of type typ, or nil.
func firstCondition(status map[string]any, typ string) map[string]any {
	for _, cond := range conditions(status) {
		if cond["type"] == typ {
			return cond
		}
	}
	return nil
}
