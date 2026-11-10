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
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

const (
	linkKind    = "Deployment"
	linkAPI     = "apps/v1"
	linkPred    = "old-uid"
	linkWaiting = "does not wait"
)

// TestChainLinkRefusals checks each refusal of the chain link at admission (ADR-0028 section 4, threat model
// H5) against a predecessor that is valid but for the one field each case changes.
func TestChainLinkRefusals(t *testing.T) {
	hint := remediationv1alpha1.HintRef{Source: remediationv1alpha1.HintSourceGitHub, CommentID: 7,
		Digest: "sha256:" + strings.Repeat("a", 64)}
	target := remediationv1alpha1.ObjectRef{APIVersion: linkAPI, Kind: linkKind, Name: appName}
	incident := remediationv1alpha1.IncidentRef{Name: "incident-1", UID: "incident-uid"}
	pred := func() *remediationv1alpha1.RemediationRequest {
		p := &remediationv1alpha1.RemediationRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "ns", UID: linkPred},
			Spec:       remediationv1alpha1.RemediationRequestSpec{IncidentRef: incident, TargetRef: target},
		}
		p.Status.Phase = remediationv1alpha1.PhaseAwaitingApproval
		p.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 12}
		p.Status.Conditions = []metav1.Condition{{Type: remediationv1alpha1.ConditionPROpened,
			Status: metav1.ConditionTrue, Reason: ReasonPullRequestOpened}}
		p.Status.Hints = []remediationv1alpha1.Hint{{Source: hint.Source, CommentID: hint.CommentID,
			Digest: hint.Digest, State: remediationv1alpha1.HintPending}}
		return p
	}
	repl := &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "ns", UID: "new-uid"},
		Spec: remediationv1alpha1.RemediationRequestSpec{IncidentRef: incident, TargetRef: target,
			Replaces:  &remediationv1alpha1.RequestReplacement{Name: "old", UID: linkPred, Hint: hint},
			ChainRoot: linkPred},
	}
	for name, c := range map[string]struct {
		change func(*remediationv1alpha1.RemediationRequest)
		want   string
	}{
		"valid":            {func(*remediationv1alpha1.RemediationRequest) {}, ""},
		"uid":              {func(p *remediationv1alpha1.RemediationRequest) { p.UID = "other" }, "another UID"},
		"another Incident": {func(p *remediationv1alpha1.RemediationRequest) { p.Spec.IncidentRef.UID = "x" }, "another Incident"},
		"target":           {func(p *remediationv1alpha1.RemediationRequest) { p.Spec.TargetRef.Name = "api" }, "another target"},
		"another chain":    {func(p *remediationv1alpha1.RemediationRequest) { p.Spec.ChainRoot = "root" }, "another chain"},
		"not waiting": {func(p *remediationv1alpha1.RemediationRequest) {
			p.Status.Phase = remediationv1alpha1.PhaseMerged
		}, linkWaiting},
		"no pull request": {func(p *remediationv1alpha1.RemediationRequest) { p.Status.PullRequest = nil }, linkWaiting},
		"closed pull request": {func(p *remediationv1alpha1.RemediationRequest) {
			p.Status.Conditions[0].Reason = "ChangeRequestClosed"
		}, linkWaiting},
		"already handed over": {func(p *remediationv1alpha1.RemediationRequest) {
			p.Status.Handoff = &remediationv1alpha1.Handoff{To: &remediationv1alpha1.OutgoingHandoff{}}
		}, "already handed over"},
		"an earlier handoff aborted": {func(p *remediationv1alpha1.RemediationRequest) {
			p.Status.Handoff = &remediationv1alpha1.Handoff{To: &remediationv1alpha1.OutgoingHandoff{
				Phase: remediationv1alpha1.HandoffAborted}}
		}, ""},
		"hint not pending": {func(p *remediationv1alpha1.RemediationRequest) {
			p.Status.Hints[0].State = remediationv1alpha1.HintDeclined
		}, "named hint"},
		"hint missing": {func(p *remediationv1alpha1.RemediationRequest) { p.Status.Hints = nil }, "named hint"},
	} {
		t.Run(name, func(t *testing.T) {
			p := pred()
			c.change(p)
			reader := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithObjects(p).Build()
			r := &RemediationRequestReconciler{Client: reader, APIReader: reader, ReviewerHints: true}
			_, why, err := r.chainLink(context.Background(), repl)
			if err != nil || (c.want == "") != (why == "") || !strings.Contains(why, c.want) {
				t.Fatalf("chainLink() = %q, %v, want a refusal with %q", why, err, c.want)
			}
		})
	}
	// Hints off refuses a replacement whose link holds (ADR-0028 amendment of 2026-11-10).
	valid := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithObjects(pred()).Build()
	off := &RemediationRequestReconciler{Client: valid, APIReader: valid}
	if reason, _, err := off.chainLink(context.Background(), repl); err != nil ||
		reason != remediationv1alpha1.ReasonHintsTurnedOff {
		t.Fatalf("chainLink() with reviewer hints off = %q, %v, want %q", reason, err,
			remediationv1alpha1.ReasonHintsTurnedOff)
	}
	reader := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).Build()
	r := &RemediationRequestReconciler{Client: reader, APIReader: reader, ReviewerHints: true}
	if _, why, err := r.chainLink(context.Background(), repl); err != nil || !strings.Contains(why, "does not exist") {
		t.Fatalf("chainLink() without the predecessor = %q, %v", why, err)
	}
}
