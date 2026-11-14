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

package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	failedUID    = "13131313-1313-4313-8313-131313131313"
	whyUID       = "has another UID"
	whyIncident  = "names another Incident"
	whyTarget    = "names another target"
	whyNoInverse = "not exactly the inverse"
)

// failedAndRevert returns a request that ended Failed with Verified False and its exact revert.
func failedAndRevert() (failed, revert *remediationv1alpha1.RemediationRequest) {
	failed = fx.MemoryLimitRequest("default", "remediation-failed")
	failed.UID = failedUID
	failed.Status.Phase = remediationv1alpha1.PhaseFailed
	meta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
		Status: metav1.ConditionFalse, Reason: ReasonOOMKilled})
	revert = fx.MemoryLimitRequest("default", string(remediationv1alpha1.RevertName(failedUID)))
	m := revert.Spec.Action.SetContainerMemoryLimit
	m.Previous, m.Desired = m.Desired, m.Previous
	revert.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: failed.Name, UID: failedUID}
	return failed, revert
}

func TestRevertLinkProblem(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(failed, revert *remediationv1alpha1.RemediationRequest)
		why  string
	}{
		"an exact revert": {func(_, _ *remediationv1alpha1.RemediationRequest) {}, ""},
		"the same values in other units": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("402653184")
		}, ""},
		"a failed request of another UID": {func(f, _ *remediationv1alpha1.RemediationRequest) { f.UID = types.UID("x") }, whyUID},
		"not Failed": {func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Status.Phase = remediationv1alpha1.PhaseInconclusive
		}, "did not end Failed"},
		"Failed without Verified": {func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Status.Conditions = nil
		}, "Verified False"},
		"Failed with Verified Unknown": {func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Status.Conditions[0].Status = metav1.ConditionUnknown
		}, "Verified False"},
		"a revert itself": {func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: "remediation-older", UID: failedUID}
		}, "never reverted"},
		"an Incident of another name": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.IncidentRef.Name = "incident-other"
		}, whyIncident},
		"another target name": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.TargetRef.Name = "web-elsewhere"
		}, whyTarget},
		"another target UID": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.TargetRef.UID = "14141414-1414-4414-8414-141414141414"
		}, whyTarget},
		"another container": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.SetContainerMemoryLimit.Container = "sidecar"
		}, whyNoInverse},
		"the same direction": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action = fx.MemoryLimitAction()
		}, whyNoInverse},
		"another previous": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("512Mi")
		}, whyNoInverse},
		"another action type": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action = remediationv1alpha1.Action{Type: remediationv1alpha1.ActionAdjustKedaMaxReplicas,
				AdjustKedaMaxReplicas: &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: 2, DesiredValue: 1}}
		}, whyNoInverse},
		"a second action member": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.AdjustKedaMaxReplicas = &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: 2, DesiredValue: 1}
		}, whyNoInverse},
	} {
		t.Run(name, func(t *testing.T) {
			failed, revert := failedAndRevert()
			tc.edit(failed, revert)
			got := revertLinkProblem(revert, failed)
			if (got == "") != (tc.why == "") || !strings.Contains(got, tc.why) {
				t.Errorf("revertLinkProblem = %q, want %q", got, tc.why)
			}
		})
	}
}

// TestRevertApproval: an allowed revert keeps approval required even when the policy's decision would lower it
// (a decision that no longer reads spec.approvalRequired); other requests keep the decision.
func TestRevertApproval(t *testing.T) {
	_, revert := failedAndRevert()
	revert.Spec.ApprovalRequired = new(false)
	if got := revertApproval(revert, outcome{approval: new(false)}).approval; got == nil || !*got {
		t.Errorf("revert approval = %v, want true", got)
	}
	plain := fx.MemoryLimitRequest("default", "plain")
	plain.Spec.ApprovalRequired = new(false)
	if got := revertApproval(plain, outcome{approval: new(false)}).approval; got == nil || *got {
		t.Errorf("plain approval = %v, want false", got)
	}
	if got := revertApproval(revert, outcome{}).approval; got != nil {
		t.Errorf("approval of an outcome without one = %v, want none", got)
	}
}

// revertForBody returns a failed request named as the analyzer names it, with the pull request it opened, and
// its revert, both in the operator's namespace and with the status fields the pull request body reads.
func revertForBody(t *testing.T) (failed, revert *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	failed, revert = failedAndRevert()
	name, err := remediationv1alpha1.RequestName(failed.Spec)
	if err != nil {
		t.Fatal(err)
	}
	failed.Namespace, failed.Name = "shadewright-system", string(name)
	failed.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 71,
		URL: "https://github.com/example/fleet/pull/71", HeadSHA: strings.Repeat("c", 40)}
	revert.Namespace = failed.Namespace
	revert.Spec.Reverts.Name = failed.Name
	revert.UID = "14141414-1414-4414-8414-141414141414"
	revert.Status.PlanDigest = "sha256:" + strings.Repeat("a", 64)
	revert.Status.PolicyRevision = "p/1"
	revert.Status.Source = &remediationv1alpha1.ResolvedSource{Path: sourcePath, TargetBranch: mainBranch}
	return failed, revert
}

// TestRevertPullRequestText (#720): the title and body of a revert's pull request say that it reverts a failed
// request, name that request, its pull request and the reason it failed, keep the revert's own derived name
// readable, and do not suggest reverting the revert. The marker and the branch stay as for any request.
func TestRevertPullRequestText(t *testing.T) {
	failed, revert := revertForBody(t)
	tgt := revert.Spec.TargetRef
	wantTitle := "revert: set the memory limit of container worker of " + tgt.Namespace + "/" + tgt.Name +
		" back to 256Mi"
	if got := commitTitle(revert); got != wantTitle {
		t.Errorf("title = %q, want %q", got, wantTitle)
	}
	body := pullRequestBody(revert, "", failed)
	for _, want := range []string{
		"Shadewright proposes to revert a failed change for incident `",
		"**Reverts:** RemediationRequest `shadewright-system/" + failed.Name + "` (UID `" + failedUID + "`), " +
			"pull request #71 (https://github.com/example/fleet/pull/71). It ended `Failed` with the reason " +
			"`OOMKilled`. This pull request sets the value it changed back.",
		"**Evidence:** RemediationRequest `shadewright-system/" + revert.Name + "` (UID `" + string(revert.UID) + "`)",
		"**Rollback:** do not revert this pull request: that applies `384Mi` again",
		"**Size:** a decrease of 34% of the previous value, within policy `" + string(revert.Spec.PolicyRef) +
			"`: the step of the failed request, undone.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"[REDACTED", "revert this pull request;", "the model's unverified reason"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("body holds %q:\n%s", unwanted, body)
		}
	}
	// Invariant 5: the marker and the branch derive from the UID and the plan as for any request.
	wantMarker := "<!-- shadewright request-uid=" + string(revert.UID) + " plan-digest=" + revert.Status.PlanDigest + " -->"
	if !strings.HasSuffix(body, wantMarker+"\n") || !hasMarker(body, revert) {
		t.Errorf("body does not end with the marker %q", wantMarker)
	}
	if got := freshness.BranchName(revert); got != "shadewright/"+string(revert.UID) {
		t.Errorf("branch = %q", got)
	}

	// The request it reverts is gone (or has another UID): named from spec.reverts, the rest not shown.
	body = pullRequestBody(revert, "", nil)
	if !strings.Contains(body, "**Reverts:** RemediationRequest `[REDACTED:token]` (UID `"+failedUID+"`). It is no "+
		"longer there with that UID") {
		t.Errorf("body without the reverted request:\n%s", body)
	}
	// Its status no longer shows the failure.
	failed.Status.Conditions, failed.Status.PullRequest = nil, nil
	body = pullRequestBody(revert, "", failed)
	if !strings.Contains(body, "no pull request recorded. Its status no longer shows why it failed.") {
		t.Errorf("body with an empty status of the reverted request:\n%s", body)
	}
}

// TestRevertNameRedaction (#720): a revert's "namespace/name" is shown only when the name is RevertName of the UID
// in its own spec.reverts, and the reverted request's only when it is the RequestName of its spec, so a hex
// secret written as "revert-<secret>" or "remediation-<secret>" stays redacted (invariant 4).
// redactedEvidence is the Evidence line of a body whose request name was redacted.
const redactedEvidence = "**Evidence:** RemediationRequest `[REDACTED:token]` (UID"

func TestRevertNameRedaction(t *testing.T) {
	secret24 := "3f1c9a7be2d84f06a5b1c3e9" // #nosec G101 -- made-up hex run, not a credential
	for name, tc := range map[string]struct {
		edit   func(failed, revert *remediationv1alpha1.RemediationRequest)
		secret string
		want   string
	}{
		"a revert named by the analyzer's derivation": {
			edit: func(_, _ *remediationv1alpha1.RemediationRequest) {},
			want: "**Evidence:** RemediationRequest `shadewright-system/revert-",
		},
		"a chosen secret in a revert's shape": {
			edit:   func(_, r *remediationv1alpha1.RemediationRequest) { r.Name = "revert-" + secret24 },
			secret: secret24, want: redactedEvidence,
		},
		"the derivation of another UID": {
			edit: func(_, r *remediationv1alpha1.RemediationRequest) {
				r.Name = string(remediationv1alpha1.RevertName("15151515-1515-4515-8515-151515151515"))
			},
			secret: "", want: redactedEvidence,
		},
		"a token-like namespace": {
			edit: func(f, r *remediationv1alpha1.RemediationRequest) {
				f.Namespace, r.Namespace = strings.Repeat("3f1c9a7b", 5), strings.Repeat("3f1c9a7b", 5)
			},
			secret: strings.Repeat("3f1c9a7b", 5), want: redactedEvidence,
		},
		"a chosen secret as the name of the reverted request": {
			edit: func(f, r *remediationv1alpha1.RemediationRequest) {
				f.Name, r.Spec.Reverts.Name = "remediation-"+secret24, "remediation-"+secret24
			},
			secret: secret24, want: "**Reverts:** RemediationRequest `[REDACTED:token]` (UID",
		},
	} {
		t.Run(name, func(t *testing.T) {
			failed, revert := revertForBody(t)
			tc.edit(failed, revert)
			body := pullRequestBody(revert, "", failed)
			if tc.secret != "" && strings.Contains(body, tc.secret) {
				t.Errorf("body shows %q:\n%s", tc.secret, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("body lacks %q:\n%s", tc.want, body)
			}
			if !hasMarker(body, revert) {
				t.Error("body lost its marker")
			}
		})
	}
}
