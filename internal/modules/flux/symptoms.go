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

package flux

import (
	"encoding/json"
	"math"
	"slices"

	"github.com/pkalin/shadewright/internal/modules"
)

// fieldAttempted is where Kustomizations and HelmReleases keep the revision they last tried.
const fieldAttempted = "status.lastAttemptedRevision"

// Condition types the rules read.
const (
	conditionReady   = "Ready"
	conditionStalled = "Stalled"
)

// Reasons the rules write. The Flux reasons below are copied into a symptom only when the condition carries
// exactly one of them; any other reason gives ReasonStalled or ReasonNotReady, and the condition's own reason
// goes into the evidence.
const (
	ReasonStalled  = "Stalled"
	ReasonNotReady = "NotReady"

	reasonDependencyNotReady = "DependencyNotReady"
)

// Fixed summaries. Each is written here, never taken from the cluster.
const (
	summaryStalled  = "Flux stopped retrying this object: it reports Stalled and waits for a change to the object or its source."
	summaryNotReady = "Flux reports this object not Ready for its current spec."
	summaryBehind   = " The cluster still runs the last applied revision, not the one in Git."
)

// reasons maps the known Flux Ready=False reasons of each kind to a fixed summary.
var reasons = map[string]map[string]string{
	KindKustomization: {
		"BuildFailed":          "The Kustomization could not build the manifests at its path: kustomize build failed on the revision from Git.",
		"ReconciliationFailed": "The Kustomization could not apply the manifests: the server-side dry-run or apply of an object failed.",
		"HealthCheckFailed":    "The Kustomization applied the manifests, but the objects it checks did not become healthy in time.",
		"PruneFailed":          "The Kustomization could not delete the objects that were removed from Git.",
		"ArtifactFailed":       "The Kustomization could not get the artifact of its source.",
	},
	KindHelmRelease: {
		"InstallFailed":        "The HelmRelease could not install the chart.",
		"UpgradeFailed":        "The HelmRelease could not upgrade the release.",
		"TestFailed":           "The Helm tests of the release failed.",
		"RollbackSucceeded":    "The HelmRelease upgrade failed and Helm rolled the release back.",
		"RollbackFailed":       "The HelmRelease upgrade failed and the rollback failed too.",
		"UninstallFailed":      "The HelmRelease could not uninstall the release.",
		"ArtifactFailed":       "The HelmRelease could not get the chart artifact.",
		"InitFailed":           "The HelmRelease could not prepare the release: chart, values or Helm configuration.",
		"GetLastReleaseFailed": "The HelmRelease could not read the last Helm release from the cluster.",
	},
	KindGitRepository: {
		"GitOperationFailed":     "The GitRepository could not fetch from Git.",
		"AuthenticationFailed":   "The GitRepository could not authenticate to Git.",
		"URLInvalid":             "The GitRepository URL is invalid.",
		"StorageOperationFailed": "The GitRepository could not store the artifact.",
		"VerificationError":      "The GitRepository could not verify the signature of the commit or tag.",
		"IncludeUnavailable":     "A repository included by the GitRepository is not available.",
	},
}

// kustomizationSymptoms reads a Kustomization: lastAttemptedRevision and lastAppliedRevision.
func kustomizationSymptoms(obj map[string]any) []modules.Symptom {
	return symptoms(obj, KustomizeGroup+"/"+KustomizeVersion, KindKustomization, func(status map[string]any) []modules.Evidence {
		attempted, _ := status["lastAttemptedRevision"].(string)
		applied, _ := status["lastAppliedRevision"].(string)
		return revisions(attempted, "status.lastAppliedRevision", applied)
	})
}

// helmReleaseSymptoms reads a HelmRelease: lastAttemptedRevision (the chart version) and the chart version
// of the newest release in status.history that is deployed.
func helmReleaseSymptoms(obj map[string]any) []modules.Symptom {
	return symptoms(obj, HelmGroup+"/"+HelmVersion, KindHelmRelease, func(status map[string]any) []modules.Evidence {
		attempted, _ := status["lastAttemptedRevision"].(string)
		history, _ := status["history"].([]any)
		for _, h := range history {
			snap, _ := h.(map[string]any)
			if s, _ := snap["status"].(string); s == "deployed" {
				applied, _ := snap["chartVersion"].(string)
				return revisions(attempted, "status.history.deployed.chartVersion", applied)
			}
		}
		return revisions(attempted, "", "")
	})
}

// gitRepositorySymptoms reads a GitRepository: the revision of the artifact it still serves.
func gitRepositorySymptoms(obj map[string]any) []modules.Symptom {
	return symptoms(obj, SourceGroup+"/"+SourceVersion, KindGitRepository, func(status map[string]any) []modules.Evidence {
		artifact, _ := status["artifact"].(map[string]any)
		served, _ := artifact["revision"].(string)
		if served == "" {
			return nil
		}
		return []modules.Evidence{{Field: "status.artifact.revision", Value: served}}
	})
}

// revisions returns the attempted revision and, when it is set and differs, the applied one.
func revisions(attempted, appliedField, applied string) []modules.Evidence {
	var out []modules.Evidence
	if attempted != "" {
		out = append(out, modules.Evidence{Field: fieldAttempted, Value: attempted})
	}
	if applied != "" && applied != attempted {
		out = append(out, modules.Evidence{Field: appliedField, Value: applied})
	}
	return out
}

// symptoms applies the rules of the package doc to one object. evidence returns the revisions from its status.
// Reason and Summary are always written here; cluster text goes only into the evidence.
func symptoms(obj map[string]any, apiVersion, kind string,
	evidence func(status map[string]any) []modules.Evidence) []modules.Symptom {
	if av, _ := obj["apiVersion"].(string); av != apiVersion {
		return nil
	}
	if k, _ := obj["kind"].(string); k != kind {
		return nil
	}
	meta, _ := obj["metadata"].(map[string]any)
	generation, _ := integer(meta["generation"])
	spec, _ := obj["spec"].(map[string]any)
	if suspended, _ := spec["suspend"].(bool); suspended {
		return nil
	}
	status, _ := obj["status"].(map[string]any)
	cond, typ := failing(status)
	if cond == nil {
		return nil
	}
	if observed, set := integer(cond["observedGeneration"]); set && observed != generation {
		return nil
	}
	got, _ := cond["reason"].(string)
	if typ == conditionReady && got == reasonDependencyNotReady {
		return nil
	}
	s := modules.Symptom{Reason: ReasonNotReady, Summary: summaryNotReady}
	if typ == conditionStalled {
		s = modules.Symptom{Reason: ReasonStalled, Summary: summaryStalled}
	} else if summary, ok := reasons[kind][got]; ok {
		s = modules.Symptom{Reason: got, Summary: summary}
	}
	prefix := "status.conditions." + typ
	if got != "" {
		s.Evidence = append(s.Evidence, modules.Evidence{Field: prefix + ".reason", Value: got})
	}
	if msg, _ := cond["message"].(string); msg != "" {
		s.Evidence = append(s.Evidence, modules.Evidence{Field: prefix + ".message", Value: msg})
	}
	revs := evidence(status)
	s.Evidence = append(s.Evidence, revs...)
	if kind != KindGitRepository && slices.ContainsFunc(revs, func(e modules.Evidence) bool { return e.Field != fieldAttempted }) {
		s.Summary += summaryBehind
	}
	return []modules.Symptom{s}
}

// failing returns the condition to report and its type: Stalled=True first, then the first Ready=False, or
// nil when neither is set.
func failing(status map[string]any) (map[string]any, string) {
	conds, _ := status["conditions"].([]any)
	var ready map[string]any
	for _, c := range conds {
		cond, _ := c.(map[string]any)
		typ, _ := cond["type"].(string)
		st, _ := cond["status"].(string)
		switch {
		case typ == conditionStalled && st == "True":
			return cond, conditionStalled
		case typ == conditionReady && st == "False" && ready == nil:
			ready = cond
		}
	}
	if ready == nil {
		return nil, ""
	}
	return ready, conditionReady
}

// integer reads a JSON integer as decoded by encoding/json (float64 or json.Number) or by the Kubernetes
// decoder (int64). It reports false for anything else, a fraction included.
func integer(v any) (int64, bool) {
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
