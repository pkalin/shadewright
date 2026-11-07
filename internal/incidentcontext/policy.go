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

package incidentcontext

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy"
)

// policyBlock is the policy bounds section (#493): for each catalog action that can target the subject, the
// bounds of the RemediationPolicy configured for it (Config.Policies), as the executor's policy step reads
// them ([policy.BoundsFor], the same functions as the decision), or why a proposal would be refused whatever
// its values. It is operator configuration, not evidence: it has no quotable values, and it is written from
// typed fields only (names, quantities and numbers), never from a free-text field of the policy. Bounds are
// shown only for a subject the builder resolved, or one of a kind it does not read (said in the block); for a
// subject that is gone, unread or unwatched the block says so instead. A read of a policy or of the settings
// that fails for any reason but NotFound is logged (names and the API reason only), labelled in the block and
// reported as failed, which makes it an escalation signal like any other configured source.
func (b *Builder) policyBlock(ctx context.Context, inc *remediationv1alpha1.Incident, t *target) (Block, bool) {
	blk := Block{Section: SectionPolicy, Provenance: ProvenanceConfiguration, Availability: Available}
	if b.policyReader == nil {
		blk.Availability = NotAvailable
		blk.Text = "not available: no action policies are configured\n"
		return blk, false
	}
	s := inc.Spec.Subject
	var actions []remediationv1alpha1.ActionType
	for _, a := range remediationv1alpha1.ActionTypes() {
		if policy.ActionMayTarget(a, s.APIVersion, s.Kind) {
			actions = append(actions, a)
		}
	}
	var l lines
	done := func(failed bool) (Block, bool) {
		b.finish(&blk, &l, b.limits.Policy)
		// Configuration, not evidence: nothing in this block may be quoted.
		blk.Values = nil
		return blk, failed
	}
	if len(actions) == 0 {
		l.add("No catalog action can change a %s: there is nothing to propose for this subject.", b.value(s.Kind))
		return done(false)
	}
	if why := subjectUnread(t); why != "" {
		l.add("No policy bounds are shown: %s.", why)
		if t.state == targetFailed {
			blk.addLabel(LabelIncomplete)
		}
		return done(false)
	}
	l.add("The RemediationPolicy configured for each action that can change this subject, as the policy check " +
		"reads it. The policy decides every request: a proposal outside these bounds is Denied.")
	log := logf.FromContext(ctx)
	failed := false
	var settings *remediationv1alpha1.RemediationSettings
	var settingsErr error
	settingsRead := false
	for _, a := range actions {
		name, ok := b.policies[a]
		if !ok || name == "" {
			l.add("%s: no policy is configured for this action, so it is never proposed.", a)
			continue
		}
		p := &remediationv1alpha1.RemediationPolicy{}
		err := b.call(ctx, func(ctx context.Context) error {
			return b.policyReader.Get(ctx, client.ObjectKey{Name: string(name)}, p)
		})
		switch {
		case apierrors.IsNotFound(err):
			l.add("%s: policy %s does not exist, so a proposal would be Denied (PolicyNotFound).", a, b.value(string(name)))
			continue
		case err != nil:
			log.Info("Failed to read RemediationPolicy for the policy bounds section", "remediationPolicy", string(name),
				"reason", readFailure(err))
			l.add("%s: policy %s could not be read.", a, b.value(string(name)))
			blk.addLabel(LabelIncomplete)
			failed = true
			continue
		}
		if !settingsRead {
			settingsRead = true
			st := &remediationv1alpha1.RemediationSettings{}
			settingsErr = b.call(ctx, func(ctx context.Context) error {
				return b.policyReader.Get(ctx, client.ObjectKey{Name: remediationv1alpha1.SettingsName}, st)
			})
			if settingsErr == nil {
				settings = st
			} else if !apierrors.IsNotFound(settingsErr) {
				log.Info("Failed to read RemediationSettings for the policy bounds section",
					"remediationSettings", remediationv1alpha1.SettingsName, "reason", readFailure(settingsErr))
			}
		}
		switch {
		case apierrors.IsNotFound(settingsErr):
			// The executor denies every request without the settings object (its reason SettingsNotFound).
			l.add("%s: policy %s; the RemediationSettings %s does not exist, so a proposal would be Denied (SettingsNotFound).",
				a, b.value(string(name)), remediationv1alpha1.SettingsName)
			continue
		case settingsErr != nil:
			l.add("%s: policy %s; the RemediationSettings could not be read, so its bounds are not shown.", a,
				b.value(string(name)))
			blk.addLabel(LabelIncomplete)
			failed = true
			continue
		}
		b.boundsLines(&l, a, p, settings, t)
	}
	return done(failed)
}

// subjectUnread says why the bounds of the subject of t are not shown, or returns "" when they are: the
// subject was resolved, or it is of a kind the builder does not read (boundsLines says so).
func subjectUnread(t *target) string {
	switch t.state {
	case targetResolved:
		return ""
	case targetGone:
		return "the subject no longer exists, or exists under another UID, so a proposal for it would be refused"
	case targetFailed:
		return "the subject could not be read"
	case targetUnwatched:
		return "the subject is in a namespace the analyzer does not watch"
	}
	if t.ref.UID == "" || t.ref.Namespace == "" || t.ref.Name == "" {
		return "the subject reference is incomplete"
	}
	return ""
}

// readFailure is the API reason of a failed read, for the log: never the error text, which may quote the
// server's answer.
func readFailure(err error) string {
	if r := apierrors.ReasonForError(err); r != metav1.StatusReasonUnknown {
		return string(r)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "DeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "Canceled"
	}
	return "Unknown"
}

// boundsLines writes the bounds of policy p for action a on the subject of t, or the reason a proposal would
// be refused.
func (b *Builder) boundsLines(l *lines, a remediationv1alpha1.ActionType, p *remediationv1alpha1.RemediationPolicy,
	settings *remediationv1alpha1.RemediationSettings, t *target) {
	ref := t.ref
	scope := policy.Scope{RequestNamespace: b.requestNamespace, Action: a, Target: policy.Target{
		APIVersion: ref.APIVersion, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name, UID: ref.UID,
	}}
	if t.workload != nil {
		scope.Target.Labels, scope.LabelsKnown = t.workload.GetLabels(), true
	}
	name := b.value(p.Name)
	bounds, reason, _ := policy.BoundsFor(p, settings, scope)
	if reason != "" {
		l.add("%s: policy %s does not allow this action on this subject, so a proposal would be Denied (%s).", a, name, reason)
		return
	}
	l.add("%s: policy %s", a, name)
	switch {
	case bounds.Memory != nil:
		m := bounds.Memory
		l.add("  desired memory limit from %s to %s", m.Min.String(), m.Max.String())
		percentLines(l, m.MaxIncreasePercent, m.MaxDecreasePercent)
	case bounds.Replicas != nil:
		r := bounds.Replicas
		l.add("  desired maxReplicaCount from %d to %d", max(r.Min, 1), r.Max)
		percentLines(l, r.MaxIncreasePercent, r.MaxDecreasePercent)
	}
	if bounds.ApprovalRequired {
		l.add("  approval: a person approves every request")
	}
	if t.state != targetResolved {
		l.add("  this subject's kind is not read by the context builder, so whether it still exists is not checked here: the policy check reads it")
	}
	if bounds.SelectorUnchecked {
		l.add("  the policy limits targets by label, and this subject's labels were not read: the policy check decides whether it applies")
	}
	if bounds.Memory != nil {
		b.memoryRanges(l, bounds.Memory, t)
	}
}

// percentLines writes the step limits of a change.
func percentLines(l *lines, increase int32, decrease *int32) {
	if increase > 0 {
		l.add("  an increase may add at most %d%% of the previous value", increase)
	} else {
		l.add("  increases are refused")
	}
	if decrease != nil && *decrease > 0 {
		l.add("  a decrease may remove at most %d%% of the previous value", *decrease)
	} else {
		l.add("  decreases are refused")
	}
}

// memoryRanges writes, for each container of the pod template with a memory limit, the desired values the
// policy allows from that limit ([policy.AllowedMemory]), and for one without a limit that it has none.
func (b *Builder) memoryRanges(l *lines, m *remediationv1alpha1.MemoryLimitBounds, t *target) {
	for _, c := range templateOf(t) {
		if !dnsLabel.MatchString(c.Name) {
			// The API server allows only DNS labels; anything else is not rendered in a configuration block.
			continue
		}
		if c.MemoryLimit == nil {
			l.add("  container %s: the pod template sets no memory limit, so no proposal can replace it", b.value(c.Name))
			continue
		}
		prev := c.MemoryLimit.String()
		ranges := policy.AllowedMemory(m, *c.MemoryLimit)
		if len(ranges) == 0 {
			l.add("  container %s (pod template limit %s): no desired value is within the policy", b.value(c.Name), prev)
			continue
		}
		for _, r := range ranges {
			l.add("  container %s (pod template limit %s): desired %s", b.value(c.Name), prev, rangeText(r, *c.MemoryLimit))
		}
	}
}

// dnsLabel is a container name as the API server accepts it: the only text from the workload in this block.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// rangeText writes a range of desired values; an end next to previous is written as "above" or "below" it.
func rangeText(r policy.MemoryRange, previous resource.Quantity) string {
	p := previous.DeepCopy()
	lo, hi := r.Min.DeepCopy(), r.Max.DeepCopy()
	switch {
	case lo.Value() == p.Value()+1:
		return fmt.Sprintf("above %s, up to %s", previous.String(), r.Max.String())
	case hi.Value() == p.Value()-1:
		return fmt.Sprintf("from %s, below %s", r.Min.String(), previous.String())
	default:
		return fmt.Sprintf("from %s to %s", r.Min.String(), r.Max.String())
	}
}
