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

package apivalidation

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/modules"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

var seq int

// testHeadSHA is the head commit of the pull request the status specs record.
const testHeadSHA = "0123456789abcdef0123456789abcdef01234567"

const githubURLMsg = "a GitHub repository URL must be https://github.com/<owner>/<repo> in lower case, without .git"

// unique returns a fresh object name, so specs never collide.
func unique(prefix string) string {
	seq++
	return fmt.Sprintf("%s-%d", prefix, seq)
}

// expectRefused asserts the API server refuses obj as invalid, with msg in the error. An object the server
// accepted by mistake is deleted after the spec, so one failure does not make the next spec's Create fail.
func expectRefused(obj client.Object, msg string) {
	err := k8sClient.Create(ctx, obj)
	if err == nil {
		deleteAfterSpec(obj)
	}
	ExpectWithOffset(1, err).To(HaveOccurred(), "the API server accepted an invalid object")
	ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
	ExpectWithOffset(1, err.Error()).To(ContainSubstring(msg))
}

// expectAccepted asserts the API server accepts obj and deletes it after the spec, whatever the outcome.
func expectAccepted(obj client.Object) {
	ExpectWithOffset(1, k8sClient.Create(ctx, obj)).To(Succeed())
	deleteAfterSpec(obj)
}

// deleteAfterSpec deletes obj when the current spec ends, even if the spec failed.
func deleteAfterSpec(obj client.Object) {
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
	})
}

var _ = Describe("RemediationRequest", func() {
	DescribeTable("checks status.source.buildPath",
		func(buildPath string, ok bool) {
			rr := fx.RemediationRequest("default", unique("rr"))
			Expect(k8sClient.Create(ctx, rr)).To(Succeed())
			rr.Status.Source = &v1alpha1.ResolvedSource{
				RepositoryRef: "fleet", TargetBranch: "main", Path: "apps/orders/deployment.yaml",
				BuildPath: v1alpha1.GitDirPath(buildPath), BaseCommit: "0123456789abcdef0123456789abcdef01234567",
			}
			err := k8sClient.Status().Update(ctx, rr)
			if ok {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred(), "the API server accepted an invalid buildPath")
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
			Expect(err.Error()).To(ContainSubstring("status.source.buildPath"))
		},
		Entry("the repository root", ".", true),
		Entry("a nested directory", "apps/orders", true),
		Entry("absolute", "/apps", false),
		Entry("parent reference", "../apps", false),
		Entry("parent reference below", "apps/../..", false),
		Entry("hidden directory", ".github", false),
		Entry("hidden directory below", "apps/.github", false),
		Entry("option-like name", "-rf", false),
		Entry("trailing slash", "apps/", false),
		Entry("empty segment", "apps//orders", false),
		Entry("URL", "https://example.com/apps", false),
	)

	// Both commit fields of status.pullRequest take a full SHA-1 commit ID in lower case, or nothing.
	for _, field := range []struct {
		name string
		set  func(*v1alpha1.PullRequestStatus, string)
	}{
		{"renderedTargetHead", func(p *v1alpha1.PullRequestStatus, v string) { p.RenderedTargetHead = v }},
		{"mergeCommitSHA", func(p *v1alpha1.PullRequestStatus, v string) { p.MergeCommitSHA = v }},
	} {
		DescribeTable("checks status.pullRequest."+field.name,
			func(sha string, ok bool) {
				rr := fx.RemediationRequest("default", unique("rr"))
				Expect(k8sClient.Create(ctx, rr)).To(Succeed())
				rr.Status.PullRequest = &v1alpha1.PullRequestStatus{
					Number: 1, URL: "https://github.com/example/fleet/pull/1",
					HeadSHA: testHeadSHA,
				}
				field.set(rr.Status.PullRequest, sha)
				err := k8sClient.Status().Update(ctx, rr)
				if ok {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(HaveOccurred(), "the API server accepted an invalid "+field.name)
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
				Expect(err.Error()).To(ContainSubstring("status.pullRequest." + field.name))
			},
			Entry("empty", "", true),
			Entry("a full commit SHA", "89abcdef0123456789abcdef0123456789abcdef", true),
			Entry("an abbreviated SHA", "89abcdef", false),
			Entry("one character short", "89abcdef0123456789abcdef0123456789abcde", false),
			Entry("one character long", "89abcdef0123456789abcdef0123456789abcdef0", false),
			Entry("a SHA-256 object name", strings.Repeat("ab", 32), false),
			Entry("upper case", "89ABCDEF0123456789ABCDEF0123456789ABCDEF", false),
			Entry("a branch name", "refs/heads/main-0123456789abcdef012345678", false),
		)
	}

	It("keeps mergeCommitSHA and appliedObservedAt once set", func() {
		const merge = "89abcdef0123456789abcdef0123456789abcdef"
		rr := fx.RemediationRequest("default", unique("rr"))
		Expect(k8sClient.Create(ctx, rr)).To(Succeed())
		rr.Status.PullRequest = &v1alpha1.PullRequestStatus{
			Number: 1, URL: "https://github.com/example/fleet/pull/1",
			HeadSHA: testHeadSHA,
		}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		applied := metav1.NewTime(time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC))
		rr.Status.PullRequest.MergeCommitSHA = merge
		rr.Status.PullRequest.AppliedObservedAt = &applied
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), "the first write of both is allowed")

		earlier := metav1.NewTime(applied.Add(-time.Hour))
		for name, change := range map[string]func(*v1alpha1.PullRequestStatus){
			"mergeCommitSHA changed":    func(p *v1alpha1.PullRequestStatus) { p.MergeCommitSHA = strings.Repeat("c", 40) },
			"mergeCommitSHA removed":    func(p *v1alpha1.PullRequestStatus) { p.MergeCommitSHA = "" },
			"appliedObservedAt moved":   func(p *v1alpha1.PullRequestStatus) { p.AppliedObservedAt = &earlier },
			"appliedObservedAt removed": func(p *v1alpha1.PullRequestStatus) { p.AppliedObservedAt = nil },
		} {
			got := &v1alpha1.RemediationRequest{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rr), got)).To(Succeed())
			change(got.Status.PullRequest)
			err := k8sClient.Status().Update(ctx, got)
			Expect(err).To(HaveOccurred(), "the API server accepted: "+name)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: unexpected error: %v", name, err)
			Expect(err.Error()).To(ContainSubstring("is set once and never changed"), name)
		}
	})

	It("accepts the fixture and fills in safe defaults", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		Expect(k8sClient.Create(ctx, rr)).To(Succeed())

		Expect(*rr.Spec.ApprovalRequired).To(BeTrue(), "approval must default to required")
		Expect(rr.Spec.Mode).To(Equal(v1alpha1.RemediationModeGitOps))
		Expect(rr.Spec.Verification.Timeout.Duration.String()).To(Equal("10m0s"))
		Expect(rr.Spec.Verification.StableFor.Duration.String()).To(Equal("3m0s"))
	})

	It("accepts a memory limit action on a Deployment", func() {
		Expect(k8sClient.Create(ctx, fx.MemoryLimitRequest("default", unique("rr")))).To(Succeed())
	})

	It("has no field that chooses the repository, branch or file", func() {
		Expect(reflect.TypeFor[v1alpha1.RemediationRequestSpec]().NumField()).To(Equal(7),
			"a new spec field must not let the author choose where a change is made; see ADR-0012")
		for _, f := range []string{"GitOps", "RepositoryRef", "TargetBranch", "Path", "BaseCommit"} {
			_, found := reflect.TypeFor[v1alpha1.RemediationRequestSpec]().FieldByName(f)
			Expect(found).To(BeFalse(), f)
		}
	})

	It("refuses any change to the plan after creation", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		Expect(k8sClient.Create(ctx, rr)).To(Succeed())

		rr.Spec.Action.AdjustKedaMaxReplicas.DesiredValue = 100
		err := k8sClient.Update(ctx, rr)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec is immutable"))
	})

	It("refuses an action type outside the catalog", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.Type = "RunShellCommand"
		expectRefused(rr, "spec.action.type")
	})

	It("refuses parameters that do not match the action type", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.SetContainerMemoryLimit = fx.MemoryLimitAction().SetContainerMemoryLimit
		expectRefused(rr, "setContainerMemoryLimit must be set if and only if type is SetContainerMemoryLimit")
	})

	It("refuses an action without parameters", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.AdjustKedaMaxReplicas = nil
		expectRefused(rr, "adjustKedaMaxReplicas must be set if and only if type is AdjustKedaMaxReplicas")
	})

	It("refuses a no-op KEDA change", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.AdjustKedaMaxReplicas.DesiredValue = 10
		expectRefused(rr, "desiredValue must differ from previousValue")
	})

	It("refuses KEDA values out of range", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.AdjustKedaMaxReplicas.DesiredValue = 5000
		expectRefused(rr, "desiredValue")
	})

	It("refuses a no-op memory change written differently", func() {
		rr := fx.MemoryLimitRequest("default", unique("rr"))
		rr.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("268435456") // 256Mi in bytes
		expectRefused(rr, "desired must differ from previous")
	})

	DescribeTable("refuses memory limits that are not whole bytes in range",
		func(desired, msg string) {
			rr := fx.MemoryLimitRequest("default", unique("rr"))
			rr.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse(desired)
			expectRefused(rr, msg)
		},
		Entry("zero", "0", "desired must be between 1Mi and 1Ti"),
		Entry("negative", "-1Gi", "desired must be between 1Mi and 1Ti"),
		Entry("one byte", "1", "desired must be between 1Mi and 1Ti"),
		Entry("a fraction of a byte", "1m", "previous and desired must be whole bytes"),
		Entry("above 1Ti", "2Ti", "desired must be between 1Mi and 1Ti"),
	)

	It("refuses a container name that is not a DNS label", func() {
		rr := fx.MemoryLimitRequest("default", unique("rr"))
		rr.Spec.Action.SetContainerMemoryLimit.Container = "-worker"
		expectRefused(rr, "spec.action.setContainerMemoryLimit.container")
	})

	DescribeTable("refuses an action on a kind it does not support",
		func(mutate func(*v1alpha1.RemediationRequest), msg string) {
			rr := fx.RemediationRequest("default", unique("rr"))
			mutate(rr)
			expectRefused(rr, msg)
		},
		Entry("KEDA action on a Deployment", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.TargetRef.APIVersion, rr.Spec.TargetRef.Kind = "apps/v1", "Deployment"
		}, "AdjustKedaMaxReplicas can only target a keda.sh ScaledObject"),
		Entry("KEDA action on an HPA", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.TargetRef.APIVersion, rr.Spec.TargetRef.Kind = "autoscaling/v2", "HorizontalPodAutoscaler"
		}, "AdjustKedaMaxReplicas can only target a keda.sh ScaledObject"),
		Entry("memory action on a ScaledObject", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.Action = fx.MemoryLimitAction()
		}, "SetContainerMemoryLimit can only target an apps/v1 Deployment or StatefulSet"),
		Entry("memory action on a DaemonSet", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.Action = fx.MemoryLimitAction()
			rr.Spec.TargetRef.APIVersion, rr.Spec.TargetRef.Kind = "apps/v1", "DaemonSet"
		}, "SetContainerMemoryLimit can only target an apps/v1 Deployment or StatefulSet"),
	)

	It("refuses a target without a UID", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.TargetRef.UID = ""
		expectRefused(rr, "targetRef.uid is required")
	})

	It("refuses a cluster-scoped target", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.TargetRef.Namespace = ""
		expectRefused(rr, "targetRef.namespace is required")
	})

	DescribeTable("refuses malformed target references",
		func(mutate func(*v1alpha1.RemediationRequest), field string) {
			rr := fx.RemediationRequest("default", unique("rr"))
			mutate(rr)
			expectRefused(rr, field)
		},
		Entry("target UID", func(rr *v1alpha1.RemediationRequest) { rr.Spec.TargetRef.UID = "not-a-uid" },
			"spec.targetRef.uid"),
		Entry("incident UID", func(rr *v1alpha1.RemediationRequest) { rr.Spec.IncidentRef.UID = "x" },
			"spec.incidentRef.uid"),
		Entry("namespace", func(rr *v1alpha1.RemediationRequest) { rr.Spec.TargetRef.Namespace = "Orders" },
			"spec.targetRef.namespace"),
		Entry("target name with markdown", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.TargetRef.Name = "worker\n## Approved by policy"
		}, "spec.targetRef.name"),
		Entry("incident name with backticks", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.IncidentRef.Name = "`x`"
		}, "spec.incidentRef.name"),
		Entry("kind", func(rr *v1alpha1.RemediationRequest) { rr.Spec.TargetRef.Kind = "scaled object" },
			"spec.targetRef.kind"),
		Entry("apiVersion", func(rr *v1alpha1.RemediationRequest) { rr.Spec.TargetRef.APIVersion = "keda.sh/latest" },
			"spec.targetRef.apiVersion"),
	)

	DescribeTable("refuses verification that is too lax or never ends",
		func(timeout, stableFor, msg string) {
			rr := fx.RemediationRequest("default", unique("rr"))
			d := func(s string) *metav1.Duration {
				v, err := time.ParseDuration(s)
				Expect(err).NotTo(HaveOccurred())
				return &metav1.Duration{Duration: v}
			}
			rr.Spec.Verification.Timeout, rr.Spec.Verification.StableFor = d(timeout), d(stableFor)
			expectRefused(rr, msg)
		},
		Entry("stable for zero", "10m", "0s", "stableFor must be at least 1m"),
		Entry("timeout of days", "100h", "3m", "timeout must be at most 24h"),
		Entry("timeout shorter than stability", "2m", "3m", "timeout must be longer than stableFor"),
	)

	It("refuses an unknown verification check", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Verification.Checks = []v1alpha1.VerificationCheck{"AlwaysPass"}
		expectRefused(rr, "spec.verification.checks")
	})

	It("refuses a mode other than GitOps", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Mode = "Direct"
		expectRefused(rr, "spec.mode")
	})
})

var _ = Describe("Incident", func() {
	It("accepts the fixture", func() {
		Expect(k8sClient.Create(ctx, fx.Incident("default", unique("inc")))).To(Succeed())
	})

	It("refuses a changed fingerprint", func() {
		inc := fx.Incident("default", unique("inc"))
		Expect(k8sClient.Create(ctx, inc)).To(Succeed())
		inc.Spec.Fingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		err := k8sClient.Update(ctx, inc)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec is immutable"))
	})

	It("refuses a fingerprint in the wrong format", func() {
		inc := fx.Incident("default", unique("inc"))
		inc.Spec.Fingerprint = "abc"
		expectRefused(inc, "spec.fingerprint")
	})

	// A later occurrence (design 3.2, #251) records its number and the Incident before it, of the same
	// fingerprint, always together.
	occurrence := func(n int32, previous string) *v1alpha1.Incident {
		inc := fx.Incident("default", unique("inc"))
		inc.Spec.OccurrenceNumber = n
		inc.Spec.PreviousIncident = previous
		return inc
	}
	previousOf := func(inc *v1alpha1.Incident, n int32) string {
		return v1alpha1.IncidentName(inc.Spec.Fingerprint, n)
	}

	It("accepts a later occurrence linked to its fingerprint", func() {
		inc := occurrence(2, "")
		inc.Spec.PreviousIncident = previousOf(inc, 1)
		expectAccepted(inc)
		inc = occurrence(7, "")
		inc.Spec.PreviousIncident = previousOf(inc, 6)
		expectAccepted(inc)
	})

	DescribeTable("refuses an occurrence out of its rules",
		func(change func(*v1alpha1.Incident), msg string) {
			inc := fx.Incident("default", unique("inc"))
			change(inc)
			expectRefused(inc, msg)
		},
		Entry("a number without the previous Incident", func(inc *v1alpha1.Incident) { inc.Spec.OccurrenceNumber = 2 },
			"set together"),
		Entry("a previous Incident without a number", func(inc *v1alpha1.Incident) {
			inc.Spec.PreviousIncident = previousOf(inc, 1)
		}, "set together"),
		Entry("occurrence 1", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 1, previousOf(inc, 1)
		}, "spec.occurrenceNumber"),
		Entry("an occurrence above six digits", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 1000000, previousOf(inc, 999999)
		}, "spec.occurrenceNumber"),
		Entry("a previous Incident of another fingerprint", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber = 2
			inc.Spec.PreviousIncident = v1alpha1.IncidentName("sha256:"+strings.Repeat("b", 64), 1)
		}, "same fingerprint"),
		Entry("a later occurrence of another fingerprint", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber = 4
			inc.Spec.PreviousIncident = v1alpha1.IncidentName("sha256:"+strings.Repeat("b", 64), 3)
		}, "same fingerprint"),
		Entry("a previous Incident two occurrences back", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 4, previousOf(inc, 2)
		}, "the occurrence before this one"),
		Entry("the first occurrence as the previous of occurrence 3", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 3, previousOf(inc, 1)
		}, "the occurrence before this one"),
		Entry("a later occurrence as the previous of occurrence 2", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 2, previousOf(inc, 3)
		}, "the occurrence before this one"),
		Entry("its own name as the previous Incident", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 5, previousOf(inc, 5)
		}, "the occurrence before this one"),
		Entry("a previous Incident with a leading zero", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 3, previousOf(inc, 1)+"-02"
		}, "spec.previousIncident"),
		Entry("a previous Incident that is not an Incident name", func(inc *v1alpha1.Incident) {
			inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 2, "orders-scaling"
		}, "spec.previousIncident"),
	)

	It("refuses an explicit occurrence number 0", func() {
		inc := fx.Incident("default", unique("inc"))
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(inc)
		Expect(err).NotTo(HaveOccurred())
		obj := &unstructured.Unstructured{Object: raw}
		obj.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Incident"))
		Expect(unstructured.SetNestedField(obj.Object, int64(0), "spec", "occurrenceNumber")).To(Succeed())
		Expect(unstructured.SetNestedField(obj.Object, previousOf(inc, 1), "spec", "previousIncident")).To(Succeed())
		expectRefused(obj, "spec.occurrenceNumber")
	})

	It("keeps resolvedTime while the phase stays Resolved", func() {
		inc := fx.Incident("default", unique("inc"))
		Expect(k8sClient.Create(ctx, inc)).To(Succeed())
		resolved := metav1.NewTime(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
		inc.Status.Phase, inc.Status.ResolvedTime = v1alpha1.IncidentResolved, &resolved
		Expect(k8sClient.Status().Update(ctx, inc)).To(Succeed())

		later := metav1.NewTime(resolved.Add(time.Hour))
		moved := inc.DeepCopy()
		moved.Status.ResolvedTime = &later
		err := k8sClient.Status().Update(ctx, moved)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("resolvedTime cannot change"))

		cleared := inc.DeepCopy()
		cleared.Status.ResolvedTime = nil
		err = k8sClient.Status().Update(ctx, cleared)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("resolvedTime cannot change"))

		// Leaving Resolved frees it, as a recurrence that escalates the Incident does.
		escalated := inc.DeepCopy()
		escalated.Status.Phase = v1alpha1.IncidentEscalated
		Expect(k8sClient.Status().Update(ctx, escalated)).To(Succeed())
	})

	It("refuses adding an occurrence number later", func() {
		inc := fx.Incident("default", unique("inc"))
		Expect(k8sClient.Create(ctx, inc)).To(Succeed())
		inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident = 2, previousOf(inc, 1)
		err := k8sClient.Update(ctx, inc)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec is immutable"))
	})

	// updateStatus creates the fixture, sets its status with change and returns the status update's error.
	updateStatus := func(change func(*v1alpha1.IncidentStatus)) error {
		inc := fx.Incident("default", unique("inc"))
		Expect(k8sClient.Create(ctx, inc)).To(Succeed())
		inc.Status.Phase = v1alpha1.IncidentOpen
		change(&inc.Status)
		return k8sClient.Status().Update(ctx, inc)
	}
	record := func(id string, total int64) v1alpha1.OccurrenceRecord {
		return v1alpha1.OccurrenceRecord{ID: id, Total: total, Began: &metav1.Time{Time: time.Unix(1700000000, 0)}}
	}

	It("accepts a full status at its bounds", func() {
		Expect(updateStatus(func(s *v1alpha1.IncidentStatus) {
			s.Count = v1alpha1.MaxIncidentCount
			for i := range v1alpha1.MaxOccurrenceRecords {
				s.Occurrences = append(s.Occurrences, record(fmt.Sprintf("event:0a1b2c3d-4e5f-4a6b-8c7d-%012d", i), 2147483647))
			}
			s.Occurrences[0] = record("termination:"+strings.Repeat("ab", 16), 1)
			s.OccurrencesDroppedThrough = &metav1.Time{Time: time.Unix(1700000000, 0)}
			for i := range v1alpha1.MaxAnalysisRecords {
				s.AnalysisRecords = append(s.AnalysisRecords, v1alpha1.RecordID(fmt.Sprintf("%d-", i)+strings.Repeat("a", 190)))
			}
		})).To(Succeed())
	})

	DescribeTable("refuses a status out of its bounds",
		func(change func(*v1alpha1.IncidentStatus), msg string) {
			err := updateStatus(change)
			Expect(err).To(HaveOccurred(), "the API server accepted an invalid status")
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
			Expect(err.Error()).To(ContainSubstring(msg))
		},
		Entry("count above the saturation value", func(s *v1alpha1.IncidentStatus) {
			s.Count = v1alpha1.MaxIncidentCount + 1
		}, "status.count"),
		Entry("more occurrences than the bound", func(s *v1alpha1.IncidentStatus) {
			for i := range v1alpha1.MaxOccurrenceRecords + 1 {
				s.Occurrences = append(s.Occurrences, record(fmt.Sprintf("event:%d", i), 1))
			}
		}, "status.occurrences"),
		Entry("an occurrence of an unknown kind", func(s *v1alpha1.IncidentStatus) {
			s.Occurrences = []v1alpha1.OccurrenceRecord{record("log:abc", 1)}
		}, "status.occurrences[0].id"),
		Entry("an occurrence ID with text in it", func(s *v1alpha1.IncidentStatus) {
			s.Occurrences = []v1alpha1.OccurrenceRecord{record("event:not a uid", 1)}
		}, "status.occurrences[0].id"),
		Entry("an occurrence ID that is too long", func(s *v1alpha1.IncidentStatus) {
			s.Occurrences = []v1alpha1.OccurrenceRecord{record("event:"+strings.Repeat("a", 65), 1)}
		}, "status.occurrences[0].id"),
		Entry("a negative total", func(s *v1alpha1.IncidentStatus) {
			s.Occurrences = []v1alpha1.OccurrenceRecord{record("event:abc", -1)}
		}, "status.occurrences[0].total"),
		Entry("a total above a series count", func(s *v1alpha1.IncidentStatus) {
			s.Occurrences = []v1alpha1.OccurrenceRecord{record("event:abc", 2147483648)}
		}, "status.occurrences[0].total"),
		Entry("a record ID with a slash", func(s *v1alpha1.IncidentStatus) {
			s.AnalysisRecords = []v1alpha1.RecordID{"a/b"}
		}, "status.analysisRecords[0]"),
		Entry("a record ID in upper case", func(s *v1alpha1.IncidentStatus) {
			s.AnalysisRecords = []v1alpha1.RecordID{"Abc"}
		}, "status.analysisRecords[0]"),
		Entry("a record ID longer than the store takes", func(s *v1alpha1.IncidentStatus) {
			s.AnalysisRecords = []v1alpha1.RecordID{v1alpha1.RecordID(strings.Repeat("a", 201))}
		}, "status.analysisRecords[0]"),
		Entry("more record IDs than the bound", func(s *v1alpha1.IncidentStatus) {
			for i := range v1alpha1.MaxAnalysisRecords + 1 {
				s.AnalysisRecords = append(s.AnalysisRecords, v1alpha1.RecordID(fmt.Sprintf("r%d", i)))
			}
		}, "status.analysisRecords"),
	)
})

var _ = Describe("RemediationPolicy", func() {
	It("accepts the fixture with safe defaults", func() {
		p := fx.RemediationPolicy(unique("policy"))
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		Expect(p.Spec.Approval).To(Equal(v1alpha1.ApprovalRequired))
		Expect(p.Spec.RequestNamespaces).To(ConsistOf(v1alpha1.Name("shadewright-system")))
		b := p.Spec.Actions[0].AdjustKedaMaxReplicas
		Expect(b.Min).To(BeEquivalentTo(1))
		Expect(b.MaxIncreasePercent).To(BeEquivalentTo(50))
		Expect(*b.MaxDecreasePercent).To(BeEquivalentTo(0), "decreases must be refused unless allowed")
	})

	It("accepts optional approval and memory bounds", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Approval = v1alpha1.ApprovalOptional
		p.Spec.Actions = append(p.Spec.Actions, fx.MemoryLimitPolicyAction())
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		Expect(*p.Spec.Actions[1].SetContainerMemoryLimit.MaxDecreasePercent).To(BeEquivalentTo(0))
	})

	It("refuses KEDA bounds with min above max", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Actions[0].AdjustKedaMaxReplicas.Min = 30
		expectRefused(p, "min must not be greater than max")
	})

	DescribeTable("refuses memory bounds that are not whole bytes in range",
		func(minQ, maxQ, msg string) {
			p := fx.RemediationPolicy(unique("policy"))
			a := fx.MemoryLimitPolicyAction()
			a.SetContainerMemoryLimit.Min, a.SetContainerMemoryLimit.Max = resource.MustParse(minQ), resource.MustParse(maxQ)
			p.Spec.Actions = []v1alpha1.PolicyAction{a}
			expectRefused(p, msg)
		},
		Entry("zero min", "0", "2Gi", "min must be at least 1Mi"),
		Entry("negative max", "128Mi", "-1Gi", "min must not be greater than max"),
		Entry("milli bytes", "128Mi", "1500m", "min and max must be whole bytes"),
		Entry("max above 1Ti", "128Mi", "2Ti", "max must be at most 1Ti"),
	)

	It("refuses an unbounded decrease", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Actions[0].AdjustKedaMaxReplicas.MaxDecreasePercent = ptr.To[int32](100)
		expectRefused(p, "maxDecreasePercent")
	})

	It("falls back to shadewright-system when request namespaces are left empty", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.RequestNamespaces = []v1alpha1.Name{}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		Expect(p.Spec.RequestNamespaces).To(ConsistOf(v1alpha1.Name("shadewright-system")))
	})

	It("refuses a request namespace that is not a DNS label", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.RequestNamespaces = []v1alpha1.Name{"Team_A"}
		expectRefused(p, "spec.requestNamespaces[0]")
	})

	It("refuses a name longer than a DNS label", func() {
		p := fx.RemediationPolicy("p" + strings.Repeat("x", 63))
		expectRefused(p, "name must be at most 63 characters")
	})

	It("refuses a policy without namespaces", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Targets.Namespaces = nil
		expectRefused(p, "spec.targets.namespaces")
	})

	It("refuses bounds that do not match the action type", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Actions[0].Type = v1alpha1.ActionSetContainerMemoryLimit
		expectRefused(p, "setContainerMemoryLimit must be set if and only if type is SetContainerMemoryLimit")
	})

	It("refuses two entries for the same action type", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Actions = append(p.Spec.Actions, p.Spec.Actions[0])
		expectRefused(p, "Duplicate value")
	})

	DescribeTable("accepts narrow path patterns",
		func(pattern string) {
			p := fx.RemediationPolicy(unique("policy"))
			p.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{v1alpha1.GitPathPattern(pattern)}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		},
		Entry("one segment wildcard", "apps/*/scaledobject.yaml"),
		Entry("recursive under a directory", "apps/**/*.yaml"),
	)

	DescribeTable("refuses path patterns that are too wide or escape the repository",
		func(pattern string) {
			p := fx.RemediationPolicy(unique("policy"))
			p.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{v1alpha1.GitPathPattern(pattern)}
			expectRefused(p, "spec.gitOps.allowedPaths[0]")
		},
		Entry("whole repository", "**"),
		Entry("every YAML file", "**/*.yaml"),
		Entry("wildcard first segment", "*/scaledobject.yaml"),
		Entry("absolute", "/etc/passwd"),
		Entry("parent reference", "apps/../../x.yaml"),
		Entry("parent reference inside a segment", "apps/a..b/x.yaml"),
		Entry("hidden directory", ".github/workflows/*.yml"),
		Entry("hidden directory below", "apps/.github/x.yaml"),
		Entry("option-like name", "-rf.yaml"),
		Entry("shell characters", "apps/$(id).yaml"),
		Entry("empty segment", "apps//x.yaml"),
		Entry("trailing slash", "apps/"),
	)
})

var _ = Describe("RemediationSettings", func() {
	It("accepts the fixture with automation enabled and conservative limits", func() {
		s := fx.RemediationSettings()
		expectAccepted(s)
		Expect(*s.Spec.Paused).To(BeFalse())
		Expect(s.Spec.MaxConcurrentRemediations).To(BeEquivalentTo(1))
		Expect(*s.Spec.Analysis.MaxAnalysesPerDay).To(BeEquivalentTo(100))
	})

	It("refuses any name but default", func() {
		s := fx.RemediationSettings()
		s.Name = "other"
		expectRefused(s, "named default")
	})

	It("refuses a repository URL that is not HTTPS", func() {
		s := fx.RemediationSettings()
		s.Spec.Repositories[0].URL = "git@github.com:example/fleet.git"
		expectRefused(s, "spec.repositories[0].url")
	})

	It("defaults the target branch to main", func() {
		s := fx.RemediationSettings()
		expectAccepted(s)
		Expect(s.Spec.Repositories[0].TargetBranch).To(BeEquivalentTo("main"))
	})

	DescribeTable("refuses target branches git could misread",
		func(branch string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].TargetBranch = v1alpha1.GitBranch(branch)
			expectRefused(s, "spec.repositories[0].targetBranch")
		},
		Entry("option-like", "-main"),
		Entry("parent reference", "release/../main"),
		Entry("full ref", "refs/heads/main"),
		Entry("HEAD", "HEAD"),
		Entry("lock file", "main.lock"),
		Entry("trailing slash", "main/"),
		Entry("Shadewright's own branch", "shadewright/11111111-1111-4111-8111-111111111111"),
	)

	DescribeTable("refuses target branches the GitHub adapter refuses",
		func(branch string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].TargetBranch = v1alpha1.GitBranch(branch)
			expectRefused(s, "spec.repositories[0].targetBranch")
		},
		Entry("leading underscore", "_main"),
		Entry("longer than 200 characters", "b"+strings.Repeat("x", 200)),
		Entry("trailing dot", "main."),
		Entry("trailing dot after a slash", "release/v1."),
	)

	DescribeTable("accepts target branches at the GitHub adapter's bounds",
		func(branch string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].TargetBranch = v1alpha1.GitBranch(branch)
			expectAccepted(s)
		},
		Entry("200 characters", "b"+strings.Repeat("x", 199)),
		Entry("nested with dots and underscores", "release/v1.2_rc"),
		Entry("underscore after a slash", "release/_next"),
		Entry("digit first", "2026-release"),
	)

	DescribeTable("refuses repository URLs that carry credentials or point elsewhere",
		func(url, msg string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].URL = url
			expectRefused(s, msg)
		},
		Entry("credentials", "https://x-access-token:ghp_secret@github.com/example/fleet", "spec.repositories[0].url"),
		Entry("query", "https://github.com/example/fleet?token=x", "spec.repositories[0].url"),
		Entry("another host for a GitHub repository", "https://evil.example.com/example/fleet", githubURLMsg),
		Entry("lookalike host", "https://github.com.evil.example/example/fleet", githubURLMsg),
		Entry("extra path", "https://github.com/example/fleet/tree/main", githubURLMsg),
		Entry(".git suffix", "https://github.com/example/fleet.git", githubURLMsg),
		Entry("upper case", "https://github.com/Example/fleet", githubURLMsg),
		Entry("dot owner", "https://github.com/./fleet", "spec.repositories[0].url"),
		Entry("dot repository", "https://github.com/example/.", "spec.repositories[0].url"),
		Entry("parent repository", "https://github.com/example/..", "spec.repositories[0].url"),
		Entry("dot-led .git name", "https://github.com/example/.git", githubURLMsg),
		Entry("dot in owner", "https://github.com/ex.ample/fleet", githubURLMsg),
		Entry("owner starting with a hyphen", "https://github.com/-example/fleet", githubURLMsg),
		Entry("owner longer than 39 characters", "https://github.com/"+strings.Repeat("o", 40)+"/fleet", githubURLMsg),
		Entry("name longer than 100 characters", "https://github.com/example/"+strings.Repeat("n", 101), githubURLMsg),
	)

	DescribeTable("accepts GitHub repository URLs at the adapter's bounds",
		func(url string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].URL = url
			expectAccepted(s)
		},
		Entry("owner of 39 characters", "https://github.com/"+strings.Repeat("o", 39)+"/fleet"),
		Entry("name of 100 characters", "https://github.com/example/"+strings.Repeat("n", 100)),
		Entry("one-character owner and name", "https://github.com/e/f"),
		Entry("digit-first owner with hyphens", "https://github.com/1-example/fleet_repo.v2"),
		Entry("name starting with a dot", "https://github.com/example/.github"),
	)

	DescribeTable("refuses App client IDs the GitHub adapter refuses",
		func(id string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].GitHub.AppClientID = id
			expectRefused(s, "spec.repositories[0].github.appClientID")
		},
		Entry("space", "Iv23 example"),
		Entry("slash", "Iv23/example"),
		Entry("colon", "Iv23:example"),
		Entry("non-ASCII letter", "Iv23\u00e9xample"),
		Entry("longer than 64 characters", strings.Repeat("a", 65)),
	)

	DescribeTable("accepts App client IDs the GitHub adapter accepts",
		func(id string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].GitHub.AppClientID = id
			expectAccepted(s)
		},
		Entry("classic", "Iv1.0123456789abcdef"),
		Entry("all allowed characters", "Iv23li.A_b-9"),
		Entry("64 characters", strings.Repeat("a", 64)),
	)

	It("refuses two repositories with the same URL", func() {
		s := fx.RemediationSettings()
		second := *s.Spec.Repositories[0].DeepCopy()
		second.Name = "fleet-copy"
		s.Spec.Repositories = append(s.Spec.Repositories, second)
		expectRefused(s, "repository URLs must be unique")
	})

	It("refuses a GitHub repository without App settings", func() {
		s := fx.RemediationSettings()
		s.Spec.Repositories[0].GitHub = nil
		expectRefused(s, "github must be set if and only if provider is GitHub")
	})

	It("refuses a signer key outside Azure Key Vault", func() {
		s := fx.RemediationSettings()
		s.Spec.Repositories[0].GitHub.Signer.AzureKeyVault.KeyID = "https://evil.example.com/keys/github-app"
		expectRefused(s, "keyID")
	})

	DescribeTable("refuses key IDs outside the Key Vault naming rules",
		func(keyID, msg string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].GitHub.Signer.AzureKeyVault.KeyID = keyID
			expectRefused(s, msg)
		},
		Entry("vault name of 2 characters", "https://kv.vault.azure.net/keys/github-app", "keyID"),
		Entry("vault name of 25 characters",
			"https://k"+strings.Repeat("v", 24)+".vault.azure.net/keys/github-app", "keyID"),
		Entry("vault name starting with a digit", "https://1kv-example.vault.azure.net/keys/github-app", "keyID"),
		Entry("vault name starting with a hyphen", "https://-kv-example.vault.azure.net/keys/github-app", "keyID"),
		Entry("vault name ending with a hyphen", "https://kv-example-.vault.azure.net/keys/github-app", "keyID"),
		Entry("consecutive hyphens in the vault name", "https://kv--example.vault.azure.net/keys/github-app",
			"the vault name must not contain consecutive hyphens"),
		Entry("key name longer than 127 characters",
			"https://kv-example.vault.azure.net/keys/"+strings.Repeat("k", 128), "keyID"),
	)

	DescribeTable("accepts key IDs at the Key Vault naming bounds",
		func(keyID string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].GitHub.Signer.AzureKeyVault.KeyID = keyID
			expectAccepted(s)
		},
		Entry("vault name of 3 characters", "https://kv1.vault.azure.net/keys/github-app"),
		Entry("vault name of 24 characters", "https://k"+strings.Repeat("v", 23)+".vault.azure.net/keys/github-app"),
		Entry("key name of 127 characters with consecutive hyphens",
			"https://kv-example.vault.azure.net/keys/"+strings.Repeat("k", 125)+"--"),
		Entry("with a version", "https://kv-example.vault.azure.net/keys/github-app/0123456789abcdef0123456789abcdef"),
	)

	Describe("analysis", func() {
		model := func() *v1alpha1.ModelDeployment {
			return &v1alpha1.ModelDeployment{
				Endpoint:        "https://example.services.ai.azure.com/",
				Deployment:      "small-model",
				APIVersion:      "2024-10-21",
				MaxTokensPerDay: ptr.To[int64](500000),
			}
		}
		strong := func() *v1alpha1.ModelDeployment {
			m := model()
			m.Deployment = "strong-model"
			return m
		}
		withAnalysis := func(change func(*v1alpha1.AnalysisSettings)) *v1alpha1.RemediationSettings {
			s := fx.RemediationSettings()
			s.Spec.Analysis = &v1alpha1.AnalysisSettings{}
			change(s.Spec.Analysis)
			return s
		}
		withModel := func(change func(*v1alpha1.ModelDeployment)) *v1alpha1.RemediationSettings {
			return withAnalysis(func(a *v1alpha1.AnalysisSettings) {
				m := model()
				change(m)
				a.Models = &v1alpha1.AnalysisModels{Triage: m}
			})
		}

		It("fills in safe defaults when analysis is left out, with no model configured", func() {
			s := fx.RemediationSettings()
			expectAccepted(s)
			a := s.Spec.Analysis
			Expect(a).NotTo(BeNil())
			Expect(*a.MaxContextBytes).To(BeEquivalentTo(32768))
			Expect(*a.MaxToolSteps).To(BeEquivalentTo(4))
			Expect(*a.MaxOutputTokens).To(BeEquivalentTo(2048))
			Expect(*a.MaxConcurrentAnalyses).To(BeEquivalentTo(1))
			Expect(*a.MaxAnalysesPerDay).To(BeEquivalentTo(100))
			Expect(a.Models).To(BeNil())
		})

		It("defaults maxAnalysesPerDay to 100 when analysis is set without it", func() {
			s := withAnalysis(func(a *v1alpha1.AnalysisSettings) { a.MaxToolSteps = ptr.To[int32](2) })
			expectAccepted(s)
			Expect(*s.Spec.Analysis.MaxAnalysesPerDay).To(BeEquivalentTo(100))
		})

		It("refuses the deprecated spec.maxAnalysesPerDay on create, naming the new path", func() {
			s := fx.RemediationSettings()
			s.Spec.MaxAnalysesPerDay = ptr.To[int32](0)
			expectRefused(s, "spec.maxAnalysesPerDay moved to spec.analysis.maxAnalysesPerDay")
		})

		DescribeTable("accepts maxAnalysesPerDay at its bounds",
			func(n int32) {
				expectAccepted(withAnalysis(func(a *v1alpha1.AnalysisSettings) { a.MaxAnalysesPerDay = new(n) }))
			},
			Entry("0, which disables the model", int32(0)),
			Entry("10000", int32(10000)),
		)

		It("accepts a triage and an escalation model with their own caps", func() {
			expectAccepted(withAnalysis(func(a *v1alpha1.AnalysisSettings) {
				a.Models = &v1alpha1.AnalysisModels{Triage: model(), Escalation: strong()}
			}))
		})

		It("accepts every limit at its lower bound", func() {
			expectAccepted(withAnalysis(func(a *v1alpha1.AnalysisSettings) {
				a.MaxContextBytes, a.MaxToolSteps = ptr.To[int32](4096), ptr.To[int32](0)
				a.MaxOutputTokens, a.MaxConcurrentAnalyses = ptr.To[int32](256), ptr.To[int32](1)
				a.Models = &v1alpha1.AnalysisModels{Triage: model()}
				a.Models.Triage.MaxTokensPerDay = ptr.To[int64](0)
			}))
		})

		It("accepts every limit at its upper bound", func() {
			expectAccepted(withAnalysis(func(a *v1alpha1.AnalysisSettings) {
				a.MaxContextBytes, a.MaxToolSteps = ptr.To[int32](262144), ptr.To[int32](16)
				a.MaxOutputTokens, a.MaxConcurrentAnalyses = ptr.To[int32](16384), ptr.To[int32](8)
				a.Models = &v1alpha1.AnalysisModels{Triage: model()}
				a.Models.Triage.MaxTokensPerDay = ptr.To[int64](1000000000)
			}))
		})

		DescribeTable("refuses limits out of their bounds",
			func(change func(*v1alpha1.AnalysisSettings), msg string) {
				expectRefused(withAnalysis(change), msg)
			},
			Entry("a context of 0 bytes", func(a *v1alpha1.AnalysisSettings) { a.MaxContextBytes = ptr.To[int32](0) },
				"spec.analysis.maxContextBytes"),
			Entry("a context below 4096 bytes", func(a *v1alpha1.AnalysisSettings) { a.MaxContextBytes = ptr.To[int32](4095) },
				"spec.analysis.maxContextBytes"),
			Entry("a context above 262144 bytes", func(a *v1alpha1.AnalysisSettings) { a.MaxContextBytes = ptr.To[int32](262145) },
				"spec.analysis.maxContextBytes"),
			Entry("negative tool steps", func(a *v1alpha1.AnalysisSettings) { a.MaxToolSteps = ptr.To[int32](-1) },
				"spec.analysis.maxToolSteps"),
			Entry("more than 16 tool steps", func(a *v1alpha1.AnalysisSettings) { a.MaxToolSteps = ptr.To[int32](17) },
				"spec.analysis.maxToolSteps"),
			Entry("0 output tokens", func(a *v1alpha1.AnalysisSettings) { a.MaxOutputTokens = ptr.To[int32](0) },
				"spec.analysis.maxOutputTokens"),
			Entry("fewer than 256 output tokens", func(a *v1alpha1.AnalysisSettings) { a.MaxOutputTokens = ptr.To[int32](255) },
				"spec.analysis.maxOutputTokens"),
			Entry("more than 16384 output tokens", func(a *v1alpha1.AnalysisSettings) { a.MaxOutputTokens = ptr.To[int32](16385) },
				"spec.analysis.maxOutputTokens"),
			Entry("0 concurrent analyses", func(a *v1alpha1.AnalysisSettings) { a.MaxConcurrentAnalyses = ptr.To[int32](0) },
				"spec.analysis.maxConcurrentAnalyses"),
			Entry("more than 8 concurrent analyses", func(a *v1alpha1.AnalysisSettings) { a.MaxConcurrentAnalyses = ptr.To[int32](9) },
				"spec.analysis.maxConcurrentAnalyses"),
			Entry("negative analyses per day", func(a *v1alpha1.AnalysisSettings) { a.MaxAnalysesPerDay = ptr.To[int32](-1) },
				"spec.analysis.maxAnalysesPerDay"),
			Entry("more than 10000 analyses per day", func(a *v1alpha1.AnalysisSettings) { a.MaxAnalysesPerDay = ptr.To[int32](10001) },
				"spec.analysis.maxAnalysesPerDay"),
			Entry("a triage model without a daily token cap", func(a *v1alpha1.AnalysisSettings) {
				a.Models = &v1alpha1.AnalysisModels{Triage: model()}
				a.Models.Triage.MaxTokensPerDay = nil
			}, "spec.analysis.models.triage.maxTokensPerDay"),
			Entry("an escalation model without a daily token cap", func(a *v1alpha1.AnalysisSettings) {
				a.Models = &v1alpha1.AnalysisModels{Triage: model(), Escalation: strong()}
				a.Models.Escalation.MaxTokensPerDay = nil
			}, "spec.analysis.models.escalation.maxTokensPerDay"),
			Entry("a negative daily token cap", func(a *v1alpha1.AnalysisSettings) {
				a.Models = &v1alpha1.AnalysisModels{Triage: model()}
				a.Models.Triage.MaxTokensPerDay = ptr.To[int64](-1)
			}, "spec.analysis.models.triage.maxTokensPerDay"),
			Entry("a daily token cap above 1,000,000,000", func(a *v1alpha1.AnalysisSettings) {
				a.Models = &v1alpha1.AnalysisModels{Triage: model()}
				a.Models.Triage.MaxTokensPerDay = ptr.To[int64](1000000001)
			}, "spec.analysis.models.triage.maxTokensPerDay"),
			Entry("an escalation cap above 1,000,000,000", func(a *v1alpha1.AnalysisSettings) {
				a.Models = &v1alpha1.AnalysisModels{Triage: model(), Escalation: strong()}
				a.Models.Escalation.MaxTokensPerDay = ptr.To[int64](1000000001)
			}, "spec.analysis.models.escalation.maxTokensPerDay"),
			Entry("an escalation model without a triage model", func(a *v1alpha1.AnalysisSettings) {
				a.Models = &v1alpha1.AnalysisModels{Escalation: strong()}
			}, "an escalation model needs a triage model"),
		)

		DescribeTable("refuses triage and escalation on the same deployment",
			func(change func(*v1alpha1.ModelDeployment)) {
				expectRefused(withAnalysis(func(a *v1alpha1.AnalysisSettings) {
					e := model()
					change(e)
					a.Models = &v1alpha1.AnalysisModels{Triage: model(), Escalation: e}
				}), "triage and escalation must not name the same endpoint and deployment")
			},
			Entry("written the same", func(*v1alpha1.ModelDeployment) {}),
			Entry("without the trailing slash", func(m *v1alpha1.ModelDeployment) {
				m.Endpoint = "https://example.services.ai.azure.com"
			}),
			Entry("with the deployment in upper case", func(m *v1alpha1.ModelDeployment) { m.Deployment = "Small-Model" }),
		)

		It("accepts the same deployment name on another endpoint", func() {
			expectAccepted(withAnalysis(func(a *v1alpha1.AnalysisSettings) {
				e := model()
				e.Endpoint = "https://other.services.ai.azure.com/"
				a.Models = &v1alpha1.AnalysisModels{Triage: model(), Escalation: e}
			}))
		})

		DescribeTable("accepts model endpoints",
			func(endpoint string) {
				expectAccepted(withModel(func(m *v1alpha1.ModelDeployment) { m.Endpoint = endpoint }))
			},
			Entry("a Foundry resource", "https://example.services.ai.azure.com/"),
			Entry("an Azure OpenAI v1 path", "https://example.openai.azure.com/openai/v1"),
			Entry("a port and a path with dots", "https://models.example.org:8443/api/v1.2/"),
			Entry("port 1", "https://models.example.org:1/"),
			Entry("port 65535", "https://models.example.org:65535/"),
			Entry("the longest URL", "https://"+strings.Repeat("a", 60)+"."+strings.Repeat("b", 60)+".io/"+
				strings.Repeat("p", 256-len("https://")-121-len(".io/"))),
		)

		DescribeTable("refuses model endpoints that are not plain HTTPS URLs",
			func(endpoint string) {
				expectRefused(withModel(func(m *v1alpha1.ModelDeployment) { m.Endpoint = endpoint }),
					"spec.analysis.models.triage.endpoint")
			},
			Entry("empty", ""),
			Entry("plain HTTP", "http://example.openai.azure.com/"),
			Entry("no scheme", "example.openai.azure.com"),
			Entry("user info", "https://user:pass@example.com/"),
			Entry("a query", "https://example.openai.azure.com/?api-key=secret"),
			Entry("a fragment", "https://example.openai.azure.com/#x"),
			Entry("an IP address", "https://10.0.0.1/"),
			Entry("a single-label host", "https://localhost/"),
			Entry("an upper-case host", "https://Example.openai.azure.com/"),
			Entry("a dot segment", "https://example.openai.azure.com/../x"),
			Entry("a hidden segment", "https://example.openai.azure.com/.env"),
			Entry("an empty segment", "https://example.openai.azure.com//x"),
			Entry("a space", "https://example.openai.azure.com/a b"),
			Entry("port 0", "https://example.openai.azure.com:0/"),
			Entry("port 65536", "https://example.openai.azure.com:65536/"),
			Entry("port 99999", "https://example.openai.azure.com:99999/"),
			Entry("an empty port", "https://example.openai.azure.com:/"),
			Entry("longer than 256 characters", "https://example.openai.azure.com/"+strings.Repeat("p", 224)),
		)

		DescribeTable("refuses model deployment names",
			func(name string) {
				expectRefused(withModel(func(m *v1alpha1.ModelDeployment) { m.Deployment = name }),
					"spec.analysis.models.triage.deployment")
			},
			Entry("empty", ""),
			Entry("a slash", "gpt/mini"),
			Entry("a leading dot", ".hidden"),
			Entry("a leading dash", "-mini"),
			Entry("a space", "gpt mini"),
			Entry("longer than 64 characters", "a"+strings.Repeat("b", 64)),
		)

		It("accepts a deployment name of 64 characters", func() {
			expectAccepted(withModel(func(m *v1alpha1.ModelDeployment) { m.Deployment = "a" + strings.Repeat("b", 63) }))
		})

		DescribeTable("accepts API versions",
			func(v string) {
				expectAccepted(withModel(func(m *v1alpha1.ModelDeployment) { m.APIVersion = v }))
			},
			Entry(nil, "v1"), Entry(nil, "2024-10-21"), Entry(nil, "2025-04-01-preview"), Entry(nil, "2024-12-31"),
			Entry(nil, "2024-01-01"),
		)

		DescribeTable("refuses API versions",
			func(v string) {
				expectRefused(withModel(func(m *v1alpha1.ModelDeployment) { m.APIVersion = v }),
					"spec.analysis.models.triage.apiVersion")
			},
			Entry(nil, ""), Entry(nil, "v2"), Entry(nil, "2024-10"), Entry(nil, "2024-10-21-beta"),
			Entry(nil, "2024-10-21&api-key=x"), Entry(nil, "latest"), Entry(nil, "2024-13-01"), Entry(nil, "2024-01-32"),
			Entry(nil, "2024-00-10"), Entry(nil, "2024-10-00"),
		)

		// The model configuration authenticates with workload identity; a field for a key would put a
		// credential in a cluster-scoped object many can read.
		It("has no field for a key, secret or credential", func() {
			var check func(t reflect.Type)
			check = func(t reflect.Type) {
				for t.Kind() == reflect.Pointer {
					t = t.Elem()
				}
				if t.Kind() != reflect.Struct {
					return
				}
				for f := range t.Fields() {
					n := strings.ToLower(f.Name)
					for _, bad := range []string{"key", "secret", "password", "credential", "auth"} {
						Expect(n).NotTo(ContainSubstring(bad), "%s.%s", t.Name(), f.Name)
					}
					check(f.Type)
				}
			}
			check(reflect.TypeFor[v1alpha1.AnalysisSettings]())
		})
	})

	Describe("modules", func() {
		const aks, keda, flux v1alpha1.ModuleName = "aks", "keda", "flux"
		withModules := func(m v1alpha1.Modules) *v1alpha1.RemediationSettings {
			s := fx.RemediationSettings()
			s.Spec.Modules = &m
			return s
		}

		It("enables nothing when modules are left out", func() {
			s := fx.RemediationSettings()
			expectAccepted(s)
			Expect(s.Spec.Modules).To(BeNil())
		})

		It("accepts platforms and integrations, a shared name and a name no module has yet", func() {
			expectAccepted(withModules(v1alpha1.Modules{
				Platforms:    []v1alpha1.ModuleName{aks, "eks", "gke", flux},
				Integrations: []v1alpha1.ModuleName{keda, flux, "secrets-store-csi-driver", "a" + v1alpha1.ModuleName(strings.Repeat("b", 62))},
			}))
		})

		manyNames := func(n int) []v1alpha1.ModuleName {
			out := make([]v1alpha1.ModuleName, n)
			for i := range out {
				out[i] = v1alpha1.ModuleName(fmt.Sprintf("m%d", i))
			}
			return out
		}

		It("accepts the most names each list allows", func() {
			expectAccepted(withModules(v1alpha1.Modules{Platforms: manyNames(8), Integrations: manyNames(32)}))
		})

		DescribeTable("refuses module lists",
			func(m v1alpha1.Modules, msg string) {
				expectRefused(withModules(m), msg)
			},
			Entry("an empty platform name", v1alpha1.Modules{Platforms: []v1alpha1.ModuleName{""}}, "spec.modules.platforms[0]"),
			Entry("a platform name in upper case", v1alpha1.Modules{Platforms: []v1alpha1.ModuleName{"AKS"}}, "spec.modules.platforms[0]"),
			Entry("an integration name starting with a digit",
				v1alpha1.Modules{Integrations: []v1alpha1.ModuleName{"1keda"}}, "spec.modules.integrations[0]"),
			Entry("an integration name ending with a dash",
				v1alpha1.Modules{Integrations: []v1alpha1.ModuleName{"keda-"}}, "spec.modules.integrations[0]"),
			Entry("an integration name with a dot",
				v1alpha1.Modules{Integrations: []v1alpha1.ModuleName{"keda.sh"}}, "spec.modules.integrations[0]"),
			Entry("a name longer than 63 characters",
				v1alpha1.Modules{Integrations: []v1alpha1.ModuleName{v1alpha1.ModuleName("k" + strings.Repeat("a", 63))}},
				"spec.modules.integrations[0]"),
			Entry("the generic platform",
				v1alpha1.Modules{Platforms: []v1alpha1.ModuleName{aks, v1alpha1.GenericPlatform}},
				"the generic platform is always on and is not listed"),
			Entry("a platform listed twice", v1alpha1.Modules{Platforms: []v1alpha1.ModuleName{aks, aks}},
				"spec.modules.platforms[1]"),
			Entry("an integration listed twice", v1alpha1.Modules{Integrations: []v1alpha1.ModuleName{keda, keda}},
				"spec.modules.integrations[1]"),
			Entry("more than 8 platforms", v1alpha1.Modules{Platforms: manyNames(9)}, "spec.modules.platforms"),
			Entry("more than 32 integrations", v1alpha1.Modules{Integrations: manyNames(33)}, "spec.modules.integrations"),
		)

		// The schema and internal/modules must agree on module names, so a name a compiled-in module may have
		// can always be enabled and a name the schema accepts is one a module may have.
		DescribeTable("agrees with internal/modules on names",
			func(name string) {
				s := withModules(v1alpha1.Modules{Platforms: []v1alpha1.ModuleName{v1alpha1.ModuleName(name)}})
				err := k8sClient.Create(ctx, s)
				schemaOK := err == nil
				if schemaOK {
					deleteAfterSpec(s)
				} else {
					Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
				}
				Expect(schemaOK).To(Equal(modules.ValidName(name)), "the schema and internal/modules disagree on %q", name)
			},
			Entry(nil, string(aks)), Entry(nil, "a"), Entry(nil, "k8s"), Entry(nil, "secrets-store-csi-driver"),
			Entry(nil, "a"+strings.Repeat("b", 62)), Entry(nil, "a"+strings.Repeat("b", 63)),
			Entry(nil, ""), Entry(nil, "AKS"), Entry(nil, "1aks"), Entry(nil, "-aks"), Entry(nil, "aks-"),
			Entry(nil, "a_b"), Entry(nil, "a.b"), Entry(nil, "a b"), Entry(nil, "a--b"),
		)

		// updateStatus creates the fixture, sets its status with change and returns the status update's error.
		updateStatus := func(change func(*v1alpha1.RemediationSettingsStatus)) error {
			s := fx.RemediationSettings()
			expectAccepted(s)
			change(&s.Status)
			return k8sClient.Status().Update(ctx, s)
		}

		It("accepts a status with the detected platform and integrations", func() {
			Expect(updateStatus(func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Detected: aks, Active: aks}
				st.Integrations = []v1alpha1.IntegrationStatus{
					{Name: keda, Detected: true, Version: "keda.sh/v1alpha1", Active: true},
					{Name: flux, Detected: true, Version: "kustomize.toolkit.fluxcd.io/v1"},
					{Name: "kyverno"},
				}
			})).To(Succeed())
		})

		It("accepts a detected platform that is not enabled, with generic in use", func() {
			Expect(updateStatus(func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Detected: aks, Active: v1alpha1.GenericPlatform}
			})).To(Succeed())
		})

		It("accepts no detected platform, with generic in use", func() {
			Expect(updateStatus(func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Active: v1alpha1.GenericPlatform}
			})).To(Succeed())
		})

		DescribeTable("refuses a status that contradicts itself or holds a bad name",
			func(change func(*v1alpha1.RemediationSettingsStatus), msg string) {
				err := updateStatus(change)
				Expect(err).To(HaveOccurred(), "the API server accepted an invalid status")
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
				Expect(err.Error()).To(ContainSubstring(msg))
			},
			Entry("a bad detected platform name", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Detected: "Open_Shift", Active: v1alpha1.GenericPlatform}
			}, "status.platform.detected"),
			Entry("a bad active platform name", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Active: "Open_Shift"}
			}, "status.platform.active"),
			Entry("no active platform", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Detected: aks}
			}, "status.platform.active"),
			Entry("generic reported as detected", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Detected: v1alpha1.GenericPlatform, Active: v1alpha1.GenericPlatform}
			}, "detected is left out, not generic, when no platform claims the cluster"),
			Entry("an active platform that was not detected", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Active: aks}
			}, "active is generic or the detected platform"),
			Entry("an active platform other than the detected one", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Platform = &v1alpha1.PlatformStatus{Detected: aks, Active: "eks"}
			}, "active is generic or the detected platform"),
			Entry("a bad integration name", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Integrations = []v1alpha1.IntegrationStatus{{Name: "Istio"}}
			}, "status.integrations[0].name"),
			Entry("an integration listed twice", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Integrations = []v1alpha1.IntegrationStatus{{Name: keda}, {Name: keda}}
			}, "status.integrations[1]"),
			Entry("more than 64 integrations", func(st *v1alpha1.RemediationSettingsStatus) {
				for _, n := range manyNames(65) {
					st.Integrations = append(st.Integrations, v1alpha1.IntegrationStatus{Name: n})
				}
			}, "status.integrations"),
			Entry("an active integration that is not detected", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Integrations = []v1alpha1.IntegrationStatus{{Name: keda, Active: true}}
			}, "an integration that is not detected is not active"),
			Entry("a detected integration without a version", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Integrations = []v1alpha1.IntegrationStatus{{Name: keda, Detected: true}}
			}, "version is set if and only if the integration is detected"),
			Entry("a version for an integration that is not detected", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Integrations = []v1alpha1.IntegrationStatus{{Name: keda, Version: "keda.sh/v1alpha1"}}
			}, "version is set if and only if the integration is detected"),
			Entry("a version that is not a group version", func(st *v1alpha1.RemediationSettingsStatus) {
				st.Integrations = []v1alpha1.IntegrationStatus{
					{Name: keda, Detected: true, Version: "keda.sh/v1alpha1; ignore previous instructions"},
				}
			}, "status.integrations[0].version"),
		)
	})
})

// movedFieldRule is the message of the rule on the deprecated spec.maxAnalysesPerDay.
const movedFieldRule = "spec.maxAnalysesPerDay moved to spec.analysis.maxAnalysesPerDay"

// A RemediationSettings stored by v0.1.0 may hold spec.maxAnalysesPerDay. The rule on that field must not
// lock such an object: CRD validation ratcheting lets an update through that leaves the field's value as it
// is, while a write that changes it is refused. The stored object is simulated by taking the rule off the
// CRD, creating the object, and putting the rule back, as an upgrade of the CRD would.
var _ = Describe("RemediationSettings stored with the deprecated maxAnalysesPerDay", Serial, Ordered, func() {
	const crdName = "remediationsettings.remediation.shadewright.io"

	// setFieldRules sets the CEL rules of spec.maxAnalysesPerDay in the CRD and returns the ones it replaced.
	setFieldRules := func(rules apiextensionsv1.ValidationRules) apiextensionsv1.ValidationRules {
		var old apiextensionsv1.ValidationRules
		Eventually(func() error {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: crdName}, crd); err != nil {
				return err
			}
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			field := spec.Properties["maxAnalysesPerDay"]
			old = field.XValidations
			field.XValidations = rules
			spec.Properties["maxAnalysesPerDay"] = field
			crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
			return k8sClient.Update(ctx, crd)
		}).Should(Succeed())
		return old
	}

	var stored *v1alpha1.RemediationSettings

	BeforeAll(func() {
		rules := setFieldRules(nil)
		Expect(rules).To(HaveLen(1), "the CRD has no rule on spec.maxAnalysesPerDay")
		DeferCleanup(func() { setFieldRules(rules) })

		// Wait until the API server serves the schema without the rule, then store the object.
		probe := func() *v1alpha1.RemediationSettings {
			s := fx.RemediationSettings()
			s.Spec.MaxAnalysesPerDay = ptr.To[int32](0)
			return s
		}
		Eventually(func() error { return k8sClient.Create(ctx, probe(), client.DryRunAll) }).Should(Succeed())
		stored = probe()
		Expect(k8sClient.Create(ctx, stored)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, stored))).To(Succeed()) })

		// Put the rule back and wait until a change of the field is refused again.
		setFieldRules(rules)
		Eventually(func() error {
			s := stored.DeepCopy()
			s.Spec.MaxAnalysesPerDay = ptr.To[int32](1)
			return k8sClient.Update(ctx, s, client.DryRunAll)
		}).Should(MatchError(ContainSubstring(movedFieldRule)))
	})

	It("accepts an update elsewhere that keeps the stored value", func() {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stored), stored)).To(Succeed())
		stored.Spec.Paused = new(true)
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		Expect(*stored.Spec.MaxAnalysesPerDay).To(BeEquivalentTo(0))
		Expect(*stored.Spec.Analysis.MaxAnalysesPerDay).To(BeEquivalentTo(100))
	})

	It("refuses an update that changes the stored value", func() {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stored), stored)).To(Succeed())
		s := stored.DeepCopy()
		s.Spec.MaxAnalysesPerDay = ptr.To[int32](5)
		err := k8sClient.Update(ctx, s)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err).To(MatchError(ContainSubstring(movedFieldRule)))
	})

	It("accepts removing the stored value", func() {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stored), stored)).To(Succeed())
		stored.Spec.MaxAnalysesPerDay = nil
		stored.Spec.Analysis.MaxAnalysesPerDay = ptr.To[int32](0)
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		Expect(stored.Spec.MaxAnalysesPerDay).To(BeNil())
	})
})
