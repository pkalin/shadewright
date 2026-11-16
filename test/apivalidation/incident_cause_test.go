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
	"maps"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The Incident status fields of the cause rule (ADR-0039 points 8, 14 and 16, #790): their bounds, and that an
// Incident written before them stays valid.
var _ = Describe("Incident status of the cause rule", func() {
	seen := metav1.NewTime(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))

	// stored creates an Incident with a phase, so each spec's status write runs the rules of the status object.
	stored := func() *v1alpha1.Incident {
		inc := fx.Incident("default", unique("inc"))
		Expect(k8sClient.Create(ctx, inc)).To(Succeed())
		deleteAfterSpec(inc)
		inc.Status.Phase = v1alpha1.IncidentOpen
		return inc
	}

	// expectStatusRefused asserts the API server refuses inc's status as invalid, with msg in the error.
	expectStatusRefused := func(inc *v1alpha1.Incident, msg string) {
		err := k8sClient.Status().Update(ctx, inc)
		ExpectWithOffset(1, err).To(HaveOccurred(), "the API server accepted an invalid status")
		ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		ExpectWithOffset(1, err.Error()).To(ContainSubstring(msg))
	}

	containers := func(n int) []v1alpha1.Name {
		names := make([]v1alpha1.Name, n)
		for i := range names {
			names[i] = v1alpha1.Name(fmt.Sprintf("c%d", i))
		}
		return names
	}

	occurrences := func(n int) []v1alpha1.OccurrenceRecord {
		records := make([]v1alpha1.OccurrenceRecord, n)
		for i := range records {
			began := metav1.NewTime(seen.Add(-time.Duration(i) * time.Minute))
			records[i] = v1alpha1.OccurrenceRecord{ID: fmt.Sprintf("event:%08d-0000-4000-8000-000000000000", i),
				Total: 1, Began: &began}
		}
		return records
	}

	It("accepts every new field at its bound", func() {
		inc := stored()
		inc.Status.Containers = containers(v1alpha1.MaxIncidentContainers)
		inc.Status.Containers[0] = v1alpha1.Name(strings.Repeat("a", 63))
		inc.Status.Cause = true
		inc.Status.Absorbed = []v1alpha1.AbsorbedTally{
			{Reason: v1alpha1.AbsorbedBackOff, Count: v1alpha1.MaxIncidentCount, LastSeen: seen},
			{Reason: v1alpha1.AbsorbedUnhealthy, Count: 0, LastSeen: seen},
		}
		inc.Status.AbsorbedOccurrences = occurrences(v1alpha1.MaxOccurrenceRecords)
		inc.Status.AbsorbedOccurrencesDroppedThrough = &seen
		inc.Status.FoldedInto = &v1alpha1.IncidentRef{Name: v1alpha1.IncidentName(
			"sha256:"+strings.Repeat("b", 64), 0), UID: testRootUID}
		// foldedInto is allowed only in phase Resolved (#793).
		inc.Status.Phase, inc.Status.ResolvedTime = v1alpha1.IncidentResolved, &seen
		Expect(k8sClient.Status().Update(ctx, inc)).To(Succeed())

		got := &v1alpha1.Incident{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inc), got)).To(Succeed())
		Expect(got.Status.Containers).To(Equal(inc.Status.Containers))
		Expect(got.Status.Cause).To(BeTrue())
		Expect(got.Status.Absorbed).To(HaveLen(2))
		Expect(got.Status.AbsorbedOccurrences).To(HaveLen(v1alpha1.MaxOccurrenceRecords))
		Expect(got.Status.AbsorbedOccurrencesDroppedThrough).NotTo(BeNil())
		Expect(got.Status.FoldedInto).To(Equal(inc.Status.FoldedInto))
	})

	DescribeTable("refuses status.containers",
		func(edit func(inc *v1alpha1.Incident), msg string) {
			inc := stored()
			edit(inc)
			expectStatusRefused(inc, msg)
		},
		Entry("with a ninth name", func(inc *v1alpha1.Incident) {
			inc.Status.Containers = containers(v1alpha1.MaxIncidentContainers + 1)
		}, "must have at most 8 items"),
		Entry("with an upper-case name", func(inc *v1alpha1.Incident) {
			inc.Status.Containers = []v1alpha1.Name{"Web"}
		}, "status.containers[0]"),
		Entry("with an underscore", func(inc *v1alpha1.Incident) {
			inc.Status.Containers = []v1alpha1.Name{"web_1"}
		}, "status.containers[0]"),
		Entry("with a dot, a DNS subdomain and not a label", func(inc *v1alpha1.Incident) {
			inc.Status.Containers = []v1alpha1.Name{"web.side"}
		}, "status.containers[0]"),
		Entry("with a leading hyphen", func(inc *v1alpha1.Incident) {
			inc.Status.Containers = []v1alpha1.Name{"-web"}
		}, "status.containers[0]"),
		Entry("with an empty name", func(inc *v1alpha1.Incident) {
			inc.Status.Containers = []v1alpha1.Name{""}
		}, "status.containers[0]"),
		Entry("with a name of 64 characters", func(inc *v1alpha1.Incident) {
			inc.Status.Containers = []v1alpha1.Name{v1alpha1.Name(strings.Repeat("a", 64))}
		}, "status.containers[0]"),
		Entry("with a name twice", func(inc *v1alpha1.Incident) {
			inc.Status.Containers = []v1alpha1.Name{"side", "side"}
		}, "Duplicate value"),
	)

	DescribeTable("refuses status.absorbed",
		func(edit func(inc *v1alpha1.Incident), msg string) {
			inc := stored()
			edit(inc)
			expectStatusRefused(inc, msg)
		},
		Entry("with a ninth entry", func(inc *v1alpha1.Incident) {
			for i := range v1alpha1.MaxAbsorbedReasons + 1 {
				inc.Status.Absorbed = append(inc.Status.Absorbed, v1alpha1.AbsorbedTally{
					Reason: []v1alpha1.AbsorbedReason{v1alpha1.AbsorbedBackOff, v1alpha1.AbsorbedUnhealthy}[i%2],
					Count:  1, LastSeen: seen})
			}
		}, "must have at most 8 items"),
		Entry("with a reason outside the fixed table", func(inc *v1alpha1.Incident) {
			inc.Status.Absorbed = []v1alpha1.AbsorbedTally{{Reason: "OOMKilled", Count: 1, LastSeen: seen}}
		}, `Unsupported value: "OOMKilled"`),
		Entry("with a reason twice", func(inc *v1alpha1.Incident) {
			inc.Status.Absorbed = []v1alpha1.AbsorbedTally{
				{Reason: v1alpha1.AbsorbedBackOff, Count: 1, LastSeen: seen},
				{Reason: v1alpha1.AbsorbedBackOff, Count: 2, LastSeen: seen},
			}
		}, "Duplicate value"),
		Entry("with a count over the saturation", func(inc *v1alpha1.Incident) {
			inc.Status.Absorbed = []v1alpha1.AbsorbedTally{
				{Reason: v1alpha1.AbsorbedBackOff, Count: v1alpha1.MaxIncidentCount + 1, LastSeen: seen}}
		}, "status.absorbed[0].count"),
		Entry("with a negative count", func(inc *v1alpha1.Incident) {
			inc.Status.Absorbed = []v1alpha1.AbsorbedTally{{Reason: v1alpha1.AbsorbedBackOff, Count: -1, LastSeen: seen}}
		}, "status.absorbed[0].count"),
		Entry("without lastSeen", func(inc *v1alpha1.Incident) {
			inc.Status.Absorbed = []v1alpha1.AbsorbedTally{{Reason: v1alpha1.AbsorbedBackOff, Count: 1}}
		}, "status.absorbed[0].lastSeen"),
	)

	DescribeTable("refuses status.absorbedOccurrences",
		func(edit func(inc *v1alpha1.Incident), msg string) {
			inc := stored()
			edit(inc)
			expectStatusRefused(inc, msg)
		},
		Entry("with a 33rd entry", func(inc *v1alpha1.Incident) {
			inc.Status.AbsorbedOccurrences = occurrences(v1alpha1.MaxOccurrenceRecords + 1)
		}, "must have at most 32 items"),
		Entry("with an ID of another kind", func(inc *v1alpha1.Incident) {
			inc.Status.AbsorbedOccurrences = []v1alpha1.OccurrenceRecord{{ID: "note:abc", Total: 1}}
		}, "status.absorbedOccurrences[0].id"),
		Entry("with a negative total", func(inc *v1alpha1.Incident) {
			inc.Status.AbsorbedOccurrences = []v1alpha1.OccurrenceRecord{{ID: "event:abc", Total: -1}}
		}, "status.absorbedOccurrences[0].total"),
	)

	DescribeTable("refuses status.foldedInto",
		func(ref v1alpha1.IncidentRef, msg string) {
			inc := stored()
			// In phase Resolved, where the field is allowed, so only its own schema refuses it.
			resolved := seen
			inc.Status.Phase, inc.Status.ResolvedTime, inc.Status.FoldedInto = v1alpha1.IncidentResolved, &resolved, &ref
			expectStatusRefused(inc, msg)
		},
		Entry("with a UID that is not a UID", v1alpha1.IncidentRef{Name: "incident-a", UID: "not-a-uid"},
			"status.foldedInto.uid"),
		Entry("with an upper-case name", v1alpha1.IncidentRef{Name: "Incident-a",
			UID: testRootUID}, "status.foldedInto.name"),
		Entry("with a name over 253 characters", v1alpha1.IncidentRef{Name: strings.Repeat("a", 254),
			UID: testRootUID}, "status.foldedInto.name"),
		Entry("without a name", v1alpha1.IncidentRef{UID: testRootUID},
			"status.foldedInto.name"),
	)

	// The schema of the Incident status only grows: each field of the release before stays, the new ones are
	// added, and an Incident written before them, with none of them, is still valid.
	It("keeps an Incident written before the new fields valid, and only adds them to the schema", func() {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "incidents.remediation.shadewright.io"}, crd)).To(Succeed())
		status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
		before := []string{"analysisRecords", "conditions", "count", "firstSeen", "handledHint", "lastAnalysisTime",
			"lastSeen", "occurrences", "occurrencesDroppedThrough", "phase", "remediationRequests", "resolvedTime",
			"revertsProposed"}
		added := []string{"absorbed", "absorbedOccurrences", "absorbedOccurrencesDroppedThrough", "cause",
			"containers", "folded", "foldedInto"}
		Expect(slices.Sorted(maps.Keys(status.Properties))).To(Equal(slices.Sorted(slices.Values(
			append(slices.Clone(before), added...)))))
		Expect(status.Required).To(BeEmpty(), "a new field must not be required")
		for _, name := range added {
			Expect(status.Properties[name].Default).To(BeNil(), "%s must have no default", name)
		}

		// Every field of the release before, as the analyzer writes them, with none of the new ones.
		inc := stored()
		first := metav1.NewTime(seen.Add(-time.Hour))
		inc.Status = v1alpha1.IncidentStatus{
			Phase: v1alpha1.IncidentProposed, FirstSeen: &first, LastSeen: &seen, Count: 3,
			Occurrences:               occurrences(2),
			OccurrencesDroppedThrough: &first, LastAnalysisTime: &seen,
			AnalysisRecords:     []v1alpha1.RecordID{"record-1"},
			RemediationRequests: []v1alpha1.Name{"remediation-z"},
			Conditions: []metav1.Condition{{Type: "Analyzed", Status: metav1.ConditionTrue, Reason: "Proposed",
				Message: "proposed", LastTransitionTime: seen}},
		}
		Expect(k8sClient.Status().Update(ctx, inc)).To(Succeed())
		inc.Status.Count = 4
		Expect(k8sClient.Status().Update(ctx, inc)).To(Succeed(), "a later write without the new fields is valid")

		got := &v1alpha1.Incident{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inc), got)).To(Succeed())
		Expect(got.Status.Containers).To(BeNil())
		Expect(got.Status.Cause).To(BeFalse())
		Expect(got.Status.Absorbed).To(BeNil())
		Expect(got.Status.AbsorbedOccurrences).To(BeNil())
		Expect(got.Status.FoldedInto).To(BeNil())
		Expect(got.Status.Folded).To(BeFalse())
	})
})
