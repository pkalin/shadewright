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

package incidents

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"testing"
	"time"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/analyzer/collector"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
	"github.com/shadewright-labs/shadewright/internal/testutil/crdschema"
)

// The tests of #1066 at the Incident controller (ADR-0039 point 12, amended 2026-11-21):
//   - kubelet image-pull Failed events (ClassConsequence, Reason "Failed") count on their container's image-pull
//     cause Incident (ImagePullBackOff, ErrImagePull or InvalidImageName) and open no Incident of their own;
//   - a pull Failed event is never absorbed by a non-pull cause (OOMKilled, non-zero exit, CreateContainerConfigError,
//     CreateContainerError), and a non-pull Failed event (ClassNone) is never absorbed by anything;
//   - a pull Failed Incident opened before its container's pull cause Incident is folded into it within RecurWithin
//     (15 minutes), while a non-pull Failed Incident on the same container is never folded.

const (
	reasonFailed      = "Failed"
	reasonImagePull   = "ImagePullBackOff"
	reasonErrImage    = "ErrImagePull"
	reasonInvalidImg  = "InvalidImageName"
	reasonCreateError = "CreateContainerError"
)

func pullCause(reason string, at time.Time) collector.Observation {
	o := symptomOf(1, reason, waitingID(1), 1, fingerprint.SourcePodStatus, foldContainer, collector.ClassCause, at)
	o.Began = at.Add(-RecurAfter)
	return o
}

// TestIssue1066ImagePullOpensOneIncident reproduces the rc.3 bug at the Incident controller: one image-pull cause
// Incident absorbs the three kubelet Failed pull events and BackOff of its container, opening 1 Incident in all.
func TestIssue1066ImagePullOpensOneIncident(t *testing.T) {
	for _, waitReason := range []string{reasonImagePull, reasonErrImage, reasonInvalidImg} {
		t.Run(waitReason, func(t *testing.T) {
			c, clock := newAt(t)
			at := t0.Add(5 * time.Minute)
			clock.set(at)
			c.consume(t.Context(), pullCause(waitReason, at))
			if got := opened(c); !slices.Equal(got, []string{fp(1)}) {
				t.Fatalf("after %s cause, opened %v, want [%s]", waitReason, got, fp(1))
			}

			// Three distinct Failed pull event fingerprints (different notes) + BackOff.
			c.consume(t.Context(), consequenceOf(2, 1, "web", reasonFailed, 2, 3, at.Add(time.Second)))
			c.consume(t.Context(), consequenceOf(3, 1, "web", reasonFailed, 3, 2, at.Add(2*time.Second)))
			c.consume(t.Context(), consequenceOf(4, 1, "web", reasonFailed, 4, 4, at.Add(3*time.Second)))
			c.consume(t.Context(), consequenceOf(5, 1, "web", reasonBackOff, 5, 5, at.Add(4*time.Second)))

			if got := opened(c); !slices.Equal(got, []string{fp(1)}) {
				t.Errorf("%s with pull Failed and BackOff opened %d Incidents %v, want 1 (%s)",
					waitReason, len(got), got, fp(1))
			}
			c.mu.Lock()
			b := c.pending[fp(1)]
			failedCount := b.absorbed[remediationv1alpha1.AbsorbedReason(reasonFailed)].count
			backOffCount := b.absorbed[remediationv1alpha1.AbsorbedBackOff].count
			c.mu.Unlock()
			if failedCount != 9 {
				t.Errorf("absorbed Failed count %d, want 9", failedCount)
			}
			if backOffCount != 5 {
				t.Errorf("absorbed BackOff count %d, want 5", backOffCount)
			}
		})
	}
}

// TestIssue1066FailedAbsorptionBoundaries checks that:
//   - a pull Failed event is NOT absorbed by OOMKilled, a non-zero exit (Error), CreateContainerConfigError or
//     CreateContainerError, and CauseSeen answers false for each;
//   - a non-pull Failed event (ClassNone) is NOT absorbed by ImagePullBackOff and opens its own Incident.
func TestIssue1066FailedAbsorptionBoundaries(t *testing.T) {
	t.Run("non-pull causes never absorb pull Failed", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			cause func(at time.Time) collector.Observation
		}{
			{"OOMKilled", func(at time.Time) collector.Observation { return oomCause(1, at) }},
			{"Error", func(at time.Time) collector.Observation {
				o := symptomOf(1, "Error", terminationID(1), 1, fingerprint.SourcePodStatus, "web", collector.ClassCause, at)
				o.Began = at.Add(-RecurAfter)
				return o
			}},
			{"CreateContainerConfigError", func(at time.Time) collector.Observation {
				return waitingCause(1, "web", 1, at)
			}},
			{"CreateContainerError", func(at time.Time) collector.Observation {
				return pullCause(reasonCreateError, at)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c, clock := newAt(t)
				at := t0.Add(5 * time.Minute)
				clock.set(at)
				c.consume(t.Context(), tc.cause(at))
				pullFailed := consequenceOf(2, 1, "web", reasonFailed, 2, 2, at.Add(time.Second))
				c.mu.Lock()
				seen := c.causeSeen(pullFailed)
				c.mu.Unlock()
				if seen {
					t.Errorf("CauseSeen(%s, pull Failed) = true, want false", tc.name)
				}
				c.consume(t.Context(), pullFailed)
				pullFP := fingerprint.Scoped(fp(2), pullFailed.Container, pullFailed.CauseFingerprint)
				if got := opened(c); len(got) != 2 || !slices.Contains(got, fp(1)) || !slices.Contains(got, pullFP) {
					t.Errorf("open Incidents %v, want both cause %s and pull Failed %s", got, fp(1), pullFP)
				}
			})
		}
	})

	t.Run("non-pull Failed opens its own Incident alongside ImagePullBackOff", func(t *testing.T) {
		c, clock := newAt(t)
		at := t0.Add(5 * time.Minute)
		clock.set(at)
		c.consume(t.Context(), pullCause(reasonImagePull, at))
		nonPull := symptomOf(2, reasonFailed, eventID(2), 2, fingerprint.SourceEvent, "web", collector.ClassNone, at.Add(time.Second))
		nonPull.Began = at.Add(-RecurAfter)
		nonPull.CauseFingerprint = fp(1) // even if somehow set, ClassNone is never absorbed
		c.consume(t.Context(), nonPull)
		if got := opened(c); !slices.Equal(got, []string{fp(1), fp(2)}) {
			t.Errorf("open Incidents %v, want both %s and non-pull Failed %s", got, fp(1), fp(2))
		}
	})
}

// TestIssue1066PullFailedFoldsIntoPullCause checks Decision 2 row 4 and row 6: when a pull Failed event opens an
// Incident before its container's pull cause Incident (ImagePullBackOff, ErrImagePull or InvalidImageName), the
// pull cause folds it within RecurWithin, while a non-pull Failed Incident on the same container, a pull Failed
// Incident on another container, one older than RecurWithin, one with a pull request, or any pull Failed Incident
// against a non-pull cause stays untouched.
func TestIssue1066PullFailedFoldsIntoPullCause(t *testing.T) {
	at := triggerNow
	pullFailedInc := storedPullObservation(t, foldConsequenceUID,
		consequenceOf(2, 1, foldContainer, reasonFailed, 2, 2, at.Add(-2*time.Minute)),
		at.Add(-2*time.Minute))
	nonPullFailed := symptomOf(3, reasonFailed, eventID(3), 2, fingerprint.SourceEvent, foldContainer, collector.ClassNone, at.Add(-2*time.Minute))
	nonPullFailed.Began = at.Add(-5 * time.Minute)
	nonPullFailedInc := storedPullObservation(t, types.UID("63636363-6363-4363-8363-636363636363"),
		nonPullFailed, at.Add(-2*time.Minute))
	otherContainerInc := storedPullObservation(t, types.UID("64646464-6464-4464-8464-646464646464"),
		consequenceOf(4, 1, "proxy", reasonFailed, 4, 2, at.Add(-2*time.Minute)),
		at.Add(-2*time.Minute))
	tooOldInc := storedPullObservation(t, types.UID("65656565-6565-4565-8565-656565656565"),
		consequenceOf(5, 1, foldContainer, reasonFailed, 5, 2, at.Add(-RecurWithin-time.Second)),
		at.Add(-RecurWithin-time.Second))

	for _, waitReason := range []string{reasonImagePull, reasonErrImage, reasonInvalidImg} {
		cause := storedPullObservation(t, foldCauseUID, pullCause(waitReason, at), at)
		if !foldable(pullFailedInc, cause, nil) {
			t.Errorf("pull Failed Incident did not fold into %s cause", waitReason)
		}
		if foldable(nonPullFailedInc, cause, nil) {
			t.Errorf("non-pull Failed Incident folded into %s cause, want untouched", waitReason)
		}
		if foldable(otherContainerInc, cause, nil) {
			t.Errorf("pull Failed Incident of another container folded into %s cause, want untouched", waitReason)
		}
		if foldable(tooOldInc, cause, nil) {
			t.Errorf("pull Failed Incident older than RecurWithin folded into %s cause, want untouched", waitReason)
		}
		withPR := []remediationv1alpha1.RemediationRequest{foldRequest("rr-1", remediationv1alpha1.PhasePRCreated, true)}
		withPR[0].Spec.IncidentRef.Name = pullFailedInc.Name
		if foldable(pullFailedInc, cause, withPR) {
			t.Errorf("pull Failed Incident with a PR folded into %s cause, want untouched", waitReason)
		}
	}

	// Non-pull causes never fold a pull Failed Incident.
	for _, otherReason := range []string{reasonOOM, reasonExitError, reasonConfig, reasonCreateError} {
		cause := foldCause()
		cause.Spec.Reason = otherReason
		if foldable(pullFailedInc, cause, nil) {
			t.Errorf("pull Failed Incident folded into non-pull cause %s, want untouched", otherReason)
		}
	}
}

// TestIssue1066ConsequenceStatusFlagSafety checks that status.consequence is set only when an Incident starts from
// consequence observations alone, stays true across further consequence batches, clears if a non-consequence
// observation ever lands on the same fingerprint, and is never set on an Incident that already counted a
// non-consequence observation or is a cause.
func TestIssue1066ConsequenceStatusFlagSafety(t *testing.T) {
	// Consequence-only start -> Consequence true, stays true on further consequence batches.
	var st remediationv1alpha1.IncidentStatus
	merge(&st, &batch{count: 1, first: t0, last: t0, consequence: true})
	if !st.Consequence {
		t.Fatalf("consequence-only first write: Consequence = false, want true")
	}
	merge(&st, &batch{count: 2, first: t0, last: t0.Add(time.Minute), consequence: true})
	if !st.Consequence {
		t.Fatalf("second consequence-only write: Consequence = false, want true")
	}
	// A non-consequence observation colliding on the same fingerprint clears Consequence so it can never be folded.
	merge(&st, &batch{count: 1, first: t0, last: t0.Add(2 * time.Minute), nonConsequence: true})
	if st.Consequence {
		t.Errorf("after non-consequence observation: Consequence = true, want false")
	}

	// An Incident started from a non-consequence observation never becomes Consequence true.
	var nonCons remediationv1alpha1.IncidentStatus
	merge(&nonCons, &batch{count: 1, first: t0, last: t0, nonConsequence: true})
	if nonCons.Consequence {
		t.Fatalf("non-consequence first write: Consequence = true, want false")
	}
	merge(&nonCons, &batch{count: 1, first: t0, last: t0.Add(time.Minute), consequence: true})
	if nonCons.Consequence {
		t.Errorf("consequence batch on existing non-consequence Incident: Consequence = true, want false")
	}

	// A mixed first batch (consequence and non-consequence together) leaves Consequence false.
	var mixed remediationv1alpha1.IncidentStatus
	merge(&mixed, &batch{count: 2, first: t0, last: t0, consequence: true, nonConsequence: true})
	if mixed.Consequence {
		t.Errorf("mixed first batch: Consequence = true, want false")
	}
}

// storedPullObservation persists one observation through the controller's gate and batch merge.
func storedPullObservation(t *testing.T, uid types.UID, o collector.Observation, created time.Time) *remediationv1alpha1.Incident {
	t.Helper()
	c, clock := newAt(t)
	clock.set(created)
	c.consume(t.Context(), o)
	keys := opened(c)
	if len(keys) != 1 {
		t.Fatalf("expected one Incident for %s, got %v", o.Reason, keys)
	}
	b := c.pending[keys[0]]
	inc := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{
			Name: Name(keys[0]), Namespace: fakeNamespace, UID: uid,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: b.spec,
	}
	merge(&inc.Status, b)
	addContainers(&inc.Status, b)
	return inc
}

// storedPullPair starts a fresh controller with the Failed Incident persisted before the pull cause.
func storedPullPair(t *testing.T, linkedCause int) (*Controller, client.Client, *remediationv1alpha1.Incident) {
	t.Helper()
	at := triggerNow.Add(-time.Minute)
	inc := storedPullObservation(t, foldConsequenceUID, consequenceOf(2, linkedCause, foldContainer, reasonFailed, 2, 2, at), at)
	cause := storedPullObservation(t, foldCauseUID, pullCause(reasonImagePull, triggerNow), triggerNow)
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(inc).WithObjects(inc, cause).Build()
	return loaded(t, cl), cl, inc
}

func TestPullFoldAfterRollout(t *testing.T) {
	c, cl, inc := storedPullPair(t, 3) // The Failed event saw image A. Image B has cause fp(1).
	if err := c.foldRecent(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getNamed(t, cl, inc.Name); got.Status.Phase != remediationv1alpha1.IncidentOpen || got.Status.Folded {
		t.Fatalf("rollout closed the earlier pull: %+v", got.Status.FoldedInto)
	}
}

func TestPullFoldWithUnverifiedEventBuffered(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    int
		total int64
	}{
		{"distinct event", 3, 2},
		{"later occurrences of the same series", 2, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cl, _ := storedPullPair(t, 1)
			// Same note and workload, but the second event names a nonexistent container.
			o := consequenceOf(2, 0, "", reasonFailed, tc.id, tc.total, triggerNow.Add(-time.Second))
			o.Class = collector.ClassNone
			c.consume(t.Context(), o)
			if err := c.foldRecent(t.Context()); err != nil {
				t.Fatal(err)
			}
			c.flush(t.Context(), o.Fingerprint)
			got := getNamed(t, cl, Name(o.Fingerprint))
			if got.Status.Count != 2 || got.Status.Folded || got.Status.Phase != remediationv1alpha1.IncidentOpen {
				t.Fatalf("unverified event ended on folded Incident: %+v", got.Status.FoldedInto)
			}
		})
	}
}

func TestPullFoldAfterOldStatusUpdate(t *testing.T) {
	_, cl, inc := storedPullPair(t, 1)
	got := getNamed(t, cl, inc.Name)
	// Decode and re-encode the status fields present on this Incident in the older API.
	var old struct {
		Phase       remediationv1alpha1.IncidentPhase      `json:"phase"`
		FirstSeen   *metav1.Time                           `json:"firstSeen"`
		LastSeen    *metav1.Time                           `json:"lastSeen"`
		Count       int64                                  `json:"count"`
		Occurrences []remediationv1alpha1.OccurrenceRecord `json:"occurrences"`
		Containers  []remediationv1alpha1.Name             `json:"containers"`
		Conditions  []metav1.Condition                     `json:"conditions"`
	}
	wire, err := json.Marshal(got.Status)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire, &old); err != nil {
		t.Fatal(err)
	}
	old.Conditions = append(old.Conditions, metav1.Condition{Type: "AnalysisDue", Status: metav1.ConditionTrue, Reason: "Due", LastTransitionTime: metav1.NewTime(triggerNow)})
	wire, err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	got.Status = remediationv1alpha1.IncidentStatus{}
	if err := json.Unmarshal(wire, &got.Status); err != nil {
		t.Fatal(err)
	}
	if err := cl.Status().Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	c := loaded(t, cl)
	if err := c.foldRecent(t.Context()); err != nil {
		t.Fatal(err)
	}
	got = getNamed(t, cl, inc.Name)
	if !got.Status.Folded || got.Status.FoldedInto == nil || got.Status.FoldedInto.Name != Name(fp(1)) ||
		!meta.IsStatusConditionTrue(got.Status.Conditions, "AnalysisDue") {
		t.Fatalf("owed fold or due condition lost: %+v", got.Status)
	}
}

func TestOldSchemaKeepsMixedPullBatch(t *testing.T) {
	c, cl, _ := storedPullPair(t, 1)
	w := &countingClient{Client: cl}
	c.client = w
	schema := crdschema.Field(t, "remediation.shadewright.io_incidents.yaml", "status.absorbed")
	wire, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var listSchema extensionsv1.JSONSchemaProps
	if err := json.Unmarshal(wire, &listSchema); err != nil {
		t.Fatal(err)
	}
	reason := listSchema.Items.Schema.Properties["reason"]
	reason.Enum = []extensionsv1.JSON{{Raw: []byte(`"BackOff"`)}, {Raw: []byte(`"Unhealthy"`)}}
	listSchema.Items.Schema.Properties["reason"] = reason
	var old apiextensions.JSONSchemaProps
	if err := extensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&listSchema, &old, nil); err != nil {
		t.Fatal(err)
	}
	// Validate the enclosing object so the API server constructs the field path itself.
	validator, _, err := crdvalidation.NewSchemaValidator(&apiextensions.JSONSchemaProps{Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{
		"status": {Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{"absorbed": old}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var refusal error
	w.before = func(_ context.Context, inc *remediationv1alpha1.Incident) {
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(inc)
		if err != nil {
			t.Fatal(err)
		}
		refusal = nil
		if errs := crdvalidation.ValidateCustomResource(nil, obj, validator); len(errs) > 0 {
			refusal = apierrors.NewInvalid(remediationv1alpha1.GroupVersion.WithKind("Incident").GroupKind(), inc.Name, errs)
		}
	}
	w.instead = func(context.Context) error { return refusal }
	w.after = func(err error) error {
		w.after = nil
		if err == nil {
			return apierrors.NewTimeoutError("response lost", 1)
		}
		return err
	}
	count := getNamed(t, cl, Name(fp(1))).Status.Count
	cause := pullCause(reasonErrImage, triggerNow)
	cause.Occurrence = waitingID(2)
	c.consume(t.Context(), cause)
	c.consume(t.Context(), consequenceOf(3, 1, foldContainer, reasonBackOff, 3, 2, triggerNow))
	c.consume(t.Context(), consequenceOf(4, 1, foldContainer, reasonFailed, 4, 3, triggerNow))
	c.flush(t.Context(), fp(1))
	c.flush(t.Context(), fp(1))
	got := getNamed(t, cl, Name(fp(1))).Status
	if got.Count != count+1 || len(got.Absorbed) != 1 || got.Absorbed[0].Reason != remediationv1alpha1.AbsorbedBackOff ||
		got.Absorbed[0].Count != 2 || len(got.AbsorbedOccurrences) != 2 || c.unwritten() != 0 {
		t.Fatalf("mixed batch after schema refusal: count %d (was %d), tallies %+v, records %+v", got.Count, count, got.Absorbed, got.AbsorbedOccurrences)
	}
}

// The CRD admits only a cause fingerprint in the immutable fold link.
func TestPullCauseFingerprintSchema(t *testing.T) {
	for _, field := range []string{"spec.pullCauseFingerprint", "status.continuedPullFingerprint"} {
		t.Run(field, func(t *testing.T) {
			rule := regexp.MustCompile(crdschema.Pattern(t, "remediation.shadewright.io_incidents.yaml", field))
			if !rule.MatchString(fp(1)) {
				t.Fatal("valid pull cause fingerprint refused")
			}
			for _, invalid := range []string{"", "web:v2", fp(1) + "0", fp(1)[1:]} {
				if rule.MatchString(invalid) {
					t.Errorf("schema accepted %q as a pull cause fingerprint", invalid)
				}
			}
		})
	}
}
