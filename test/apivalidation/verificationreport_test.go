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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// reportContainer is the action's container the report specs name.
const reportContainer = "app"

// reportTime is a time on the verifier's clock in the report specs.
var reportTime = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

func reportUID(i int) v1alpha1.UID {
	return v1alpha1.UID(fmt.Sprintf("c0000000-0000-4000-8000-%012d", i))
}

func newReport() *v1alpha1.VerificationReport {
	return &v1alpha1.VerificationReport{
		ObjectMeta: metav1.ObjectMeta{Name: unique("vr"), Namespace: "default"},
		Spec: v1alpha1.VerificationReportSpec{
			RequestUID: reportUID(1), MergeCommitSHA: testHeadSHA, TargetUID: reportUID(2),
			TemplateRevision: "2", Container: reportContainer,
		},
	}
}

// reportWithStatus creates a report and writes a status with samples, an open window, one reset, an OOM kill
// and a baseline, the state the refusal specs start from.
func reportWithStatus() *v1alpha1.VerificationReport {
	vr := newReport()
	expectAccepted(vr)
	start, at := metav1.NewTime(reportTime), metav1.NewTime(reportTime.Add(time.Minute))
	vr.Status = v1alpha1.VerificationReportStatus{
		Sample: 5, SampledAt: &at, WindowStart: &start, DesiredReplicas: 1, PodsCovered: 1, Resets: 2,
		LastReset:     &v1alpha1.WindowReset{Reason: v1alpha1.ResetPodNotReady, Time: metav1.NewTime(reportTime.Add(-time.Minute))},
		TroubleResets: 1,
		LastTroubleReset: &v1alpha1.WindowReset{
			Reason: v1alpha1.ResetRestarted, Time: metav1.NewTime(reportTime.Add(-2 * time.Minute)),
		},
		OOMKill:   &v1alpha1.OOMKill{PodUID: reportUID(3), Container: reportContainer},
		Baselines: []v1alpha1.PodBaseline{{PodUID: reportUID(3), Restarts: 1}},
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, vr)).To(Succeed())
	return vr
}

// expectReportStatusRefused asserts the API server refuses the status write of vr, with msg in the error.
func expectReportStatusRefused(vr *v1alpha1.VerificationReport, msg string) {
	err := k8sClient.Status().Update(ctx, vr)
	ExpectWithOffset(1, err).To(HaveOccurred(), "the API server accepted an invalid status")
	ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
	ExpectWithOffset(1, err.Error()).To(ContainSubstring(msg))
}

var _ = Describe("VerificationReport", func() {
	It("accepts a report and a later sample", func() {
		vr := reportWithStatus()
		later := metav1.NewTime(reportTime.Add(2 * time.Minute))
		vr.Status.Sample, vr.Status.SampledAt, vr.Status.WindowStart = 6, &later, &later
		vr.Status.Resets = 3
		Expect(k8sClient.Status().Update(ctx, vr)).To(Succeed())
	})

	It("accepts a reset without a class, which follows from its reason", func() {
		vr := newReport()
		expectAccepted(vr)
		vr.Status.Resets = 1
		vr.Status.LastReset = &v1alpha1.WindowReset{Reason: v1alpha1.ResetRestarted, Time: metav1.NewTime(reportTime)}
		Expect(k8sClient.Status().Update(ctx, vr)).To(Succeed())
	})

	DescribeTable("refuses a changed spec",
		func(mod func(*v1alpha1.VerificationReportSpec)) {
			vr := newReport()
			expectAccepted(vr)
			mod(&vr.Spec)
			err := k8sClient.Update(ctx, vr)
			Expect(err).To(HaveOccurred(), "the API server accepted a changed spec")
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
			Expect(err.Error()).To(ContainSubstring("spec is immutable"))
		},
		Entry("requestUID", func(s *v1alpha1.VerificationReportSpec) { s.RequestUID = reportUID(9) }),
		Entry("mergeCommitSHA", func(s *v1alpha1.VerificationReportSpec) {
			s.MergeCommitSHA = "89abcdef0123456789abcdef0123456789abcdef"
		}),
		Entry("targetUID", func(s *v1alpha1.VerificationReportSpec) { s.TargetUID = reportUID(9) }),
		Entry("templateRevision", func(s *v1alpha1.VerificationReportSpec) { s.TemplateRevision = "3" }),
		Entry("container", func(s *v1alpha1.VerificationReportSpec) { s.Container = "other" }),
		Entry("container removed", func(s *v1alpha1.VerificationReportSpec) { s.Container = "" }),
	)

	DescribeTable("refuses an invalid spec",
		func(mod func(*v1alpha1.VerificationReportSpec), msg string) {
			vr := newReport()
			mod(&vr.Spec)
			expectRefused(vr, msg)
		},
		Entry("a short merge commit", func(s *v1alpha1.VerificationReportSpec) { s.MergeCommitSHA = "abc1234" }, "spec.mergeCommitSHA"),
		Entry("a request UID that is not a UID", func(s *v1alpha1.VerificationReportSpec) { s.RequestUID = "web" }, "spec.requestUID"),
		Entry("no template revision", func(s *v1alpha1.VerificationReportSpec) { s.TemplateRevision = "" }, "spec.templateRevision"),
		Entry("a container name that is not a DNS label", func(s *v1alpha1.VerificationReportSpec) { s.Container = "App_1" }, "spec.container"),
	)

	DescribeTable("refuses a status that moves back",
		func(mod func(*v1alpha1.VerificationReportStatus), msg string) {
			vr := reportWithStatus()
			mod(&vr.Status)
			expectReportStatusRefused(vr, msg)
		},
		Entry("sample decreases", func(s *v1alpha1.VerificationReportStatus) { s.Sample = 4 }, "status.sample never decreases"),
		Entry("sample removed", func(s *v1alpha1.VerificationReportStatus) { s.Sample = 0 }, "status.sample never decreases"),
		Entry("windowStart moves back", func(s *v1alpha1.VerificationReportStatus) {
			s.WindowStart = &metav1.Time{Time: reportTime.Add(-time.Second)}
		}, "status.windowStart only moves forward"),
		Entry("windowStart moves back under another time zone", func(s *v1alpha1.VerificationReportStatus) {
			// One second earlier, written with an offset that sorts later as text.
			s.WindowStart = &metav1.Time{Time: reportTime.Add(-time.Second).In(time.FixedZone("CET", 3600))}
		}, "status.windowStart only moves forward"),
		Entry("windowStart removed", func(s *v1alpha1.VerificationReportStatus) { s.WindowStart = nil }, "status.windowStart only moves forward"),
		Entry("resets decreases", func(s *v1alpha1.VerificationReportStatus) { s.Resets = 1 }, "status.resets never decreases"),
		Entry("resets removed", func(s *v1alpha1.VerificationReportStatus) { s.Resets = 0 }, "status.resets never decreases"),
		Entry("oomKill changed", func(s *v1alpha1.VerificationReportStatus) {
			s.OOMKill = &v1alpha1.OOMKill{PodUID: reportUID(4), Container: reportContainer}
		}, "status.oomKill is set once and never changed"),
		Entry("lastReset removed", func(s *v1alpha1.VerificationReportStatus) { s.LastReset = nil }, "status.lastReset is kept once set"),
		Entry("lastReset moves back", func(s *v1alpha1.VerificationReportStatus) {
			s.LastReset.Time = metav1.NewTime(reportTime.Add(-time.Minute - time.Second))
		}, "status.lastReset is kept once set and its time never moves back"),
		Entry("troubleResets decreases", func(s *v1alpha1.VerificationReportStatus) { s.TroubleResets = 0 }, "status.troubleResets never decreases"),
		Entry("lastTroubleReset removed", func(s *v1alpha1.VerificationReportStatus) { s.LastTroubleReset = nil },
			"status.lastTroubleReset is kept once set"),
		Entry("lastTroubleReset moves back", func(s *v1alpha1.VerificationReportStatus) {
			s.LastTroubleReset.Time = metav1.NewTime(reportTime.Add(-3 * time.Minute))
		}, "status.lastTroubleReset is kept once set and its time never moves back"),
		Entry("oomKill removed", func(s *v1alpha1.VerificationReportStatus) { s.OOMKill = nil }, "status.oomKill is set once and never changed"),
	)

	It("refuses removing the whole status", func() {
		vr := reportWithStatus()
		vr.Status = v1alpha1.VerificationReportStatus{}
		expectReportStatusRefused(vr, "never decreases")
	})

	DescribeTable("refuses an invalid status",
		func(mod func(*v1alpha1.VerificationReportStatus), msg string) {
			vr := newReport()
			expectAccepted(vr)
			mod(&vr.Status)
			expectReportStatusRefused(vr, msg)
		},
		Entry("more than 200 baselines", func(s *v1alpha1.VerificationReportStatus) {
			for i := range v1alpha1.MaxReportBaselines + 1 {
				s.Baselines = append(s.Baselines, v1alpha1.PodBaseline{PodUID: reportUID(100 + i)})
			}
		}, "status.baselines: Too many"),
		Entry("a baseline twice", func(s *v1alpha1.VerificationReportStatus) {
			s.Baselines = []v1alpha1.PodBaseline{{PodUID: reportUID(3)}, {PodUID: reportUID(3), Restarts: 1}}
		}, "status.baselines[1]: Duplicate value"),
		Entry("a negative restart sum", func(s *v1alpha1.VerificationReportStatus) {
			s.Baselines = []v1alpha1.PodBaseline{{PodUID: reportUID(3), Restarts: -1}}
		}, "status.baselines[0].restarts"),
		Entry("more than 200 pods covered", func(s *v1alpha1.VerificationReportStatus) { s.PodsCovered = 201 }, "status.podsCovered"),
		Entry("a reset reason the verifier does not set", func(s *v1alpha1.VerificationReportStatus) {
			s.LastReset = &v1alpha1.WindowReset{Reason: v1alpha1.ResetStaleReport, Time: metav1.NewTime(reportTime)}
		}, "status.lastReset.reason"),
		Entry("a trouble reset that is not Restarted", func(s *v1alpha1.VerificationReportStatus) {
			s.TroubleResets = 1
			s.LastTroubleReset = &v1alpha1.WindowReset{Reason: v1alpha1.ResetPodNotReady, Time: metav1.NewTime(reportTime)}
		}, "lastTroubleReset.reason must be Restarted"),
		Entry("a negative trouble count", func(s *v1alpha1.VerificationReportStatus) { s.TroubleResets = -1 }, "status.troubleResets"),
	)

	It("accepts 200 baselines", func() {
		vr := newReport()
		expectAccepted(vr)
		for i := range v1alpha1.MaxReportBaselines {
			vr.Status.Baselines = append(vr.Status.Baselines, v1alpha1.PodBaseline{PodUID: reportUID(100 + i), Restarts: 3})
		}
		Expect(k8sClient.Status().Update(ctx, vr)).To(Succeed())
	})
})
