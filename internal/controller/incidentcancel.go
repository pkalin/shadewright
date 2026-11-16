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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// Fixed messages of a request that ends Cancelled because of its Incident (ADR-0039 points 21 and 22). They name
// no Incident: the name in the spec may be anything of the right form, a secret included.
const (
	messageIncidentFolded = "The request's Incident was folded into the Incident of its cause (status.foldedInto) " +
		"before a pull request was opened; no pull request was opened and the branch was left as it is"
	messageIncidentGone = "The Incident the request names is gone (not found, or made again under the same name " +
		"with another UID) before a pull request was opened; no pull request was opened and the branch was left " +
		"as it is"
)

// errIncidentUnread means the live read of the request's Incident before its pull request did not give an
// answer. The request is held and the step retried: it never opens a pull request on a failed read.
var errIncidentUnread = errors.New("the Incident of the request could not be read before its pull request")

// incidentStanding is the last step before the pull request is created (ADR-0039 points 21 and 22): it reads the
// Incident the request names (spec.incidentRef.name in the request's namespace) live from the API server, never
// from the cache, and fails closed. It returns the reason and message to end the request Cancelled with:
//   - IncidentGone when the API server answers NotFound for that Incident, or the Incident of that name has
//     another UID than the one the request names (deleted and opened again under the same name);
//   - IncidentFolded when it has status.foldedInto.
//
// Both empty means the request may open its pull request. Any read that does not give one of these answers (no
// API server reader, a failed or timed-out read, a NotFound that does not name the Incident) is returned as an
// error wrapping errIncidentUnread, for a retry with backoff; the request's deadline (expire) bounds the retries.
// The error carries the API reason only: the API error repeats the name.
func (r *RemediationRequestReconciler) incidentStanding(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (reason, message string, err error) {
	if r.APIReader == nil {
		// The cache could still hold an Incident folded or deleted since; never decide on it.
		return "", "", fmt.Errorf("%w: no API server reader", errIncidentUnread)
	}
	ref := rr.Spec.IncidentRef
	readCtx, cancel := context.WithTimeout(ctx, incidentReadTimeout(ctx, r.IncidentReadTimeout))
	defer cancel()
	inc := &remediationv1alpha1.Incident{}
	err = r.APIReader.Get(readCtx, client.ObjectKey{Namespace: rr.Namespace, Name: ref.Name}, inc)
	switch {
	case err != nil && notFoundIncident(err, ref.Name):
		return remediationv1alpha1.ReasonIncidentGone, messageIncidentGone, nil
	case err != nil:
		apiReason := apierrors.ReasonForError(err)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			apiReason = metav1.StatusReasonTimeout
		}
		return "", "", fmt.Errorf("%w: %s", errIncidentUnread, apiReason)
	case inc.UID != types.UID(ref.UID):
		return remediationv1alpha1.ReasonIncidentGone, messageIncidentGone, nil
	case inc.Status.FoldedInto != nil:
		return remediationv1alpha1.ReasonIncidentFolded, messageIncidentFolded, nil
	}
	return "", "", nil
}

// notFoundIncident reports whether err is the API server's NotFound for the Incident named name: a definite
// answer that it does not exist. A NotFound whose details name another object or none is not taken for one.
func notFoundIncident(err error, name string) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || !apierrors.IsNotFound(err) {
		return false
	}
	d := status.Status().Details
	return d != nil && d.Name == name
}

// endForIncident ends a request whose Incident was folded or is gone (incidentStanding) Cancelled with reason and
// message, as endHandoff ends a replacement: the kill switch's gate first, then one status write that sets the
// phase and releases the target lock. Cancelled is a final phase, so the same write takes the request out of
// the count under maxConcurrentRemediations (active). The request has no pull request: incidentStanding runs
// only right before one is opened. A conflict or a failed write is returned for a retry, which reads the
// request again and decides again; a write that went through but whose answer was lost is found Cancelled.
func (r *RemediationRequestReconciler) endForIncident(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason, message string) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	release(rr)
	return r.stop(ctx, rr, remediationv1alpha1.PhaseCancelled, reason, message)
}
