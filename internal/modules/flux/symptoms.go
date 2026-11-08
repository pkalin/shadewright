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
	"strings"

	"github.com/pkalin/shadewright/internal/fluxstatus"
	"github.com/pkalin/shadewright/internal/modules"
)

// Evidence fields the rules write.
const (
	fieldAttempted             = "status.lastAttemptedRevision"
	fieldApplied               = "status.lastAppliedRevision"
	fieldAttemptedConfigDigest = "status.lastAttemptedConfigDigest"
	fieldDeployedChartVersion  = "status.history.deployed.chartVersion"
	fieldDeployedConfigDigest  = "status.history.deployed.configDigest"
	fieldArtifactRevision      = "status.artifact.revision"
)

// Condition types the rules read (internal/fluxstatus).
const (
	conditionReady   = fluxstatus.ConditionReady
	conditionStalled = fluxstatus.ConditionStalled
)

// Reasons the rules write besides the Flux reasons in [reasons]. A condition whose reason the package does
// not know for the kind gives ReasonStalled or ReasonNotReady; the condition's own reason is evidence.
const (
	ReasonStalled            = "Stalled"
	ReasonNotReady           = "NotReady"
	ReasonDependencyNotReady = "DependencyNotReady"

	// reasonHealthCheckCanceled is no failure and shows nothing (fluxstatus.ReasonHealthCheckCanceled).
	reasonHealthCheckCanceled = fluxstatus.ReasonHealthCheckCanceled
)

// helmDeployed is the status of a Helm release snapshot that is the one running (helm-controller v1.7.1
// api/v2/snapshot_types.go, snapshotStatusDeployed).
const helmDeployed = "deployed"

// Fixed summaries. Each is written here, never taken from the cluster.
const (
	summaryStalled            = "Flux stopped retrying this object: it reports Stalled and waits for a change to the object or its source."
	summaryNotReady           = "Flux reports this object not Ready for its current spec."
	summaryDependencyNotReady = "Flux waits for a dependency of this object that is not ready: it may be missing, suspended, failing or not watched."
)

// sourceKinds are the Flux source kinds a summary may name (source-controller v1.10.1 api/v1). Any other
// spec.sourceRef.kind is cluster text and is called "source".
var sourceKinds = map[string]bool{
	"GitRepository": true, "OCIRepository": true, "Bucket": true, "HelmRepository": true, "HelmChart": true,
	"ExternalArtifact": true,
}

// differentSummary is the sentence added when the cluster does not run what the source declares, naming the
// source kind only when it is one of sourceKinds.
func differentSummary(kind string, spec map[string]any) string {
	var ref map[string]any
	if kind == KindHelmRelease {
		ref, _ = spec["chartRef"].(map[string]any)
		if ref == nil {
			chart, _ := spec["chart"].(map[string]any)
			chartSpec, _ := chart["spec"].(map[string]any)
			ref, _ = chartSpec["sourceRef"].(map[string]any)
		}
	} else {
		ref, _ = spec["sourceRef"].(map[string]any)
	}
	source := "source"
	if k, _ := ref["kind"].(string); sourceKinds[k] {
		source = k + " source"
	}
	if kind == KindHelmRelease {
		return " The cluster runs a different chart version or values than its " + source + " and spec declare."
	}
	return " The cluster runs a different revision than its " + source + " declares."
}

// Summaries shared by several reasons.
const (
	summaryAccessDenied   = "Flux refused a cross-namespace reference of this object (access denied by its ACL settings)."
	summaryCEL            = "A CEL expression in the spec of this object is invalid."
	summarySourceStorage  = "The GitRepository could not write or read its artifact in the source-controller storage."
	summaryInclude        = "A GitRepository included by this GitRepository is missing or has no artifact."
	summaryVerification   = "The GitRepository could not verify the signature of the commit or tag it fetched."
	summaryHelmController = "The helm-controller could not prepare the release: its client, configuration or resource manager failed."
)

// reasons maps, per kind, the Ready=False reasons the Flux controllers set to a fixed summary. They were read
// from the Flux sources at these tags:
//
//   - Kustomization: kustomize-controller v1.10.1 internal/controller/kustomization_controller.go (MarkFalse on
//     Ready), with the constants of fluxcd/pkg apis/meta v1.38.0 conditions.go, apis/acl v0.12.0
//     conditions.go (AccessDenied) and source-controller api/v1 condition_types.go (DirectoryCreationFailed).
//   - HelmRelease: helm-controller v1.7.1 api/v2/condition_types.go (the Released, Remediated and TestSuccess
//     reasons that internal/reconcile/release.go summarize copies into Ready),
//     internal/controller/helmrelease_controller.go and internal/reconcile/atomic_release.go (string reasons).
//   - GitRepository: source-controller v1.10.1 api/v1/condition_types.go, api/v1/gitrepository_types.go and
//     internal/controller/gitrepository_controller.go: Ready summarizes the negative conditions FetchFailed,
//     StorageOperationFailed, IncludeUnavailable and SourceVerified, and takes their reasons.
//
// DependencyNotReady is handled on its own (ReasonDependencyNotReady) and HealthCheckCanceled shows nothing.
var reasons = map[string]map[string]string{
	KindKustomization: {
		"ArtifactFailed":          "The Kustomization could not get the artifact of its source.",
		"BuildFailed":             "The Kustomization could not build the manifests at its path: kustomize build failed on the revision from its source.",
		"ReconciliationFailed":    "The Kustomization could not apply the manifests: the server-side dry-run or apply of an object failed.",
		"PruneFailed":             "The Kustomization could not delete the objects that were removed from its source.",
		"HealthCheckFailed":       "The Kustomization applied the manifests, but the objects it checks did not become healthy in time.",
		"DirectoryCreationFailed": "The kustomize-controller could not create a working directory for the build.",
		"InvalidCELExpression":    summaryCEL,
		"AccessDenied":            summaryAccessDenied,
	},
	KindHelmRelease: {
		"InstallFailed":        "The HelmRelease could not install the chart.",
		"UpgradeFailed":        "The HelmRelease could not upgrade the release.",
		"TestFailed":           "The Helm tests of the release failed.",
		"RollbackSucceeded":    "A release action of the HelmRelease failed and Helm rolled the release back.",
		"RollbackFailed":       "A release action of the HelmRelease failed and the rollback failed too.",
		"UninstallSucceeded":   "A release action of the HelmRelease failed and the release was uninstalled as remediation.",
		"UninstallFailed":      "The HelmRelease could not uninstall the release.",
		"ArtifactFailed":       "The HelmRelease could not get or load the chart artifact.",
		"SourceNotReady":       "The source of the HelmRelease chart is not ready.",
		"ValuesError":          "The HelmRelease could not compose its values from the spec and the referenced ConfigMaps or Secrets.",
		"ChartMutateError":     "The helm-controller could not prepare the chart for the release.",
		"StateError":           "The helm-controller could not determine the state of the release.",
		"ReconcileError":       "A release action of the HelmRelease failed.",
		"RESTClientError":      summaryHelmController,
		"ResourceManagerError": summaryHelmController,
		"FactoryError":         summaryHelmController,
		"ConfigFactoryErr":     summaryHelmController,
		"InvalidCELExpression": summaryCEL,
		"AccessDenied":         summaryAccessDenied,
	},
	KindGitRepository: {
		"GitOperationFailed":           "The GitRepository could not fetch from Git.",
		"AuthenticationFailed":         "The GitRepository could not authenticate to Git.",
		"URLInvalid":                   "The GitRepository URL is invalid.",
		"InvalidProviderConfiguration": "The GitRepository provider settings are invalid.",
		"DirectoryCreationFailed":      summarySourceStorage,
		"StatOperationFailed":          summarySourceStorage,
		"ArchiveOperationFailed":       summarySourceStorage,
		"SymlinkUpdateFailed":          summarySourceStorage,
		"InvalidPath":                  summarySourceStorage,
		"NotFound":                     summaryInclude,
		"NoArtifact":                   summaryInclude,
		"VerificationError":            summaryVerification,
		"InvalidCommitSignature":       summaryVerification,
		"InvalidTagSignature":          summaryVerification,
		"InvalidGitObject":             summaryVerification,
		"InvalidVerificationMode":      "The verification mode of the GitRepository does not fit what it fetches.",
	},
}

// waitsForDependencies lists the kinds whose controllers set DependencyNotReady (spec.dependsOn).
var waitsForDependencies = map[string]bool{KindKustomization: true, KindHelmRelease: true}

// kustomizationSymptoms reads a Kustomization: lastAttemptedRevision and lastAppliedRevision.
func kustomizationSymptoms(obj map[string]any) []modules.Symptom {
	return symptoms(obj, KustomizeGroup+"/"+KustomizeVersion, KindKustomization, kustomizationRevisions)
}

func kustomizationRevisions(status map[string]any) ([]modules.Evidence, bool) {
	attempted := fluxstatus.AttemptedRevision(status)
	applied := fluxstatus.AppliedRevision(status)
	var out []modules.Evidence
	if attempted != "" {
		out = append(out, modules.Evidence{Field: fieldAttempted, Value: attempted})
	}
	behind := attempted != "" && applied != "" && applied != attempted
	if behind {
		out = append(out, modules.Evidence{Field: fieldApplied, Value: applied})
	}
	return out, behind
}

// helmReleaseSymptoms reads a HelmRelease: the chart version and values digest of the last attempt, against
// those of the newest deployed release in status.history.
func helmReleaseSymptoms(obj map[string]any) []modules.Symptom {
	return symptoms(obj, HelmGroup+"/"+HelmVersion, KindHelmRelease, helmReleaseRevisions)
}

// helmReleaseRevisions compares the last attempt with the release that runs. Field names are those of
// helm-controller v1.7.1: HelmReleaseStatus.LastAttemptedRevision and LastAttemptedConfigDigest
// (api/v2/helmrelease_types.go), Snapshot.Version, Status, ChartVersion and ConfigDigest
// (api/v2/snapshot_types.go). For an OCIRepository source the controller appends "+" and the start of the
// digest to the chart version, so versions are compared without what follows "+": a failure of the same
// version from an OCI source shows no difference.
func helmReleaseRevisions(status map[string]any) ([]modules.Evidence, bool) {
	attempted := fluxstatus.AttemptedRevision(status)
	attemptedConfig, _ := status["lastAttemptedConfigDigest"].(string)
	var out []modules.Evidence
	if attempted != "" {
		out = append(out, modules.Evidence{Field: fieldAttempted, Value: attempted})
	}
	deployed := newestDeployed(status)
	if deployed == nil {
		return out, false
	}
	chart, _ := deployed["chartVersion"].(string)
	config, _ := deployed["configDigest"].(string)
	chartDiffers := attempted != "" && chart != "" && withoutBuild(chart) != withoutBuild(attempted)
	configDiffers := attemptedConfig != "" && config != "" && config != attemptedConfig
	if chartDiffers {
		out = append(out, modules.Evidence{Field: fieldDeployedChartVersion, Value: chart})
	}
	if configDiffers {
		out = append(out,
			modules.Evidence{Field: fieldAttemptedConfigDigest, Value: attemptedConfig},
			modules.Evidence{Field: fieldDeployedConfigDigest, Value: config})
	}
	return out, chartDiffers || configDiffers
}

// newestDeployed returns the snapshot in status.history with status "deployed" and the highest release
// version, or nil. The order of the list is not relied on.
func newestDeployed(status map[string]any) map[string]any {
	history, _ := status["history"].([]any)
	var best map[string]any
	var bestVersion int64
	for _, h := range history {
		snap, _ := h.(map[string]any)
		if s, _ := snap["status"].(string); s != helmDeployed {
			continue
		}
		v, _ := modules.JSONInteger(snap["version"])
		if best == nil || v > bestVersion {
			best, bestVersion = snap, v
		}
	}
	return best
}

// withoutBuild cuts a chart version at "+", the start of semver build metadata.
func withoutBuild(version string) string {
	v, _, _ := strings.Cut(version, "+")
	return v
}

// gitRepositorySymptoms reads a GitRepository: the revision of the artifact it still serves. A failing
// GitRepository does not say which revision it tried, so nothing is compared.
func gitRepositorySymptoms(obj map[string]any) []modules.Symptom {
	return symptoms(obj, SourceGroup+"/"+SourceVersion, KindGitRepository, func(status map[string]any) ([]modules.Evidence, bool) {
		served := fluxstatus.ArtifactRevision(status)
		if served == "" {
			return nil, false
		}
		return []modules.Evidence{{Field: fieldArtifactRevision, Value: served}}, false
	})
}

// symptoms applies the rules of the package doc to one object. revisions returns the revision evidence from
// its status and whether the cluster runs a different revision than the source declares. Reason and Summary
// are always written here; cluster text goes only into the evidence.
func symptoms(obj map[string]any, apiVersion, kind string,
	revisions func(status map[string]any) ([]modules.Evidence, bool)) []modules.Symptom {
	if av, _ := obj["apiVersion"].(string); av != apiVersion {
		return nil
	}
	if k, _ := obj["kind"].(string); k != kind {
		return nil
	}
	generation, hasGeneration := fluxstatus.Generation(obj)
	spec, _ := obj["spec"].(map[string]any)
	if suspended, _ := spec["suspend"].(bool); suspended {
		return nil
	}
	status := fluxstatus.Status(obj)
	cond, typ := fluxstatus.Failing(status, generation, hasGeneration)
	if cond == nil {
		return nil
	}
	got, _ := cond["reason"].(string)
	s := modules.Symptom{Reason: ReasonNotReady, Summary: summaryNotReady}
	switch {
	case typ == conditionStalled:
		s = modules.Symptom{Reason: ReasonStalled, Summary: summaryStalled}
	case got == reasonHealthCheckCanceled:
		return nil
	case got == ReasonDependencyNotReady && waitsForDependencies[kind]:
		s = modules.Symptom{Reason: ReasonDependencyNotReady, Summary: summaryDependencyNotReady}
	default:
		if summary, ok := reasons[kind][got]; ok {
			s = modules.Symptom{Reason: got, Summary: summary}
		}
	}
	prefix := "status.conditions." + typ
	if got != "" {
		s.Evidence = append(s.Evidence, modules.Evidence{Field: prefix + ".reason", Value: got})
	}
	if msg, _ := cond["message"].(string); msg != "" {
		s.Evidence = append(s.Evidence, modules.Evidence{Field: prefix + ".message", Value: msg})
	}
	revs, differs := revisions(status)
	s.Evidence = append(s.Evidence, revs...)
	if differs {
		s.Summary += differentSummary(kind, spec)
	}
	return []modules.Symptom{s}
}
