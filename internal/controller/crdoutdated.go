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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// A CustomResourceDefinition of RemediationRequest older than the executor does not list the status fields added
// since: the API server prunes them from every status write and the write still succeeds (#866). The executor's
// waits and marks in those fields are recorded state that later reconciles decide from, so a write that loses one
// must not pass for a write that stored it. Every status write of the executor goes through the reconciler's
// Status() (a test over the package source checks it), which compares the object the API server returned with
// what was sent. When a field the executor sent is missing or shorter in the answer, the write is an error for the
// error backoff (a Kubernetes error, never a Git read failure), a Warning Event with reason
// CustomResourceDefinitionOutdated is recorded on the request, and every later status write of that request in the
// same reconcile is refused with the same error, so nothing that depends on the lost field is decided. It needs no
// read of the CustomResourceDefinition and no new RBAC: the answer to the write is the evidence.
//
// An enum value added since v0.5.2 is not pruned but refused: the API server answers the whole write with 422
// Invalid, a cause of type FieldValueNotSupported on the field. Such a refusal of a value in storedValues, the
// value this write sent, is the same case (#895): the same error for the error backoff, the same Warning Event,
// and the same refusal of later status writes in the reconcile. Nothing is written without the value to get past
// it; the request waits for the CustomResourceDefinitions as the upgrade order says. Any other Invalid answer is
// returned as it is.

// EventCustomResourceDefinitionOutdated is the reason of the Warning Event recorded on a request when the API
// server dropped a status field the executor wrote.
const EventCustomResourceDefinitionOutdated = "CustomResourceDefinitionOutdated"

// upgradeDocsURL is the section of the upgrade page that says to apply the CustomResourceDefinitions first.
const upgradeDocsURL = "https://shadewright.io/upgrade-uninstall/#upgrade"

// storedField is a status field of RemediationRequest added after v0.5.2 that the executor writes and decides from:
// size counts what an object holds of it (entries of a list, 1 for a set pointer).
type storedField struct {
	path string
	size func(*api.RemediationRequest) int
}

// storedFields are the fields the check covers. status.gitWaits bounds every wait on a failed Git read (#771);
// status.unfinishedRenders counts render checks that took the executor down (#714); status.pullRequest.incidentStopped
// makes condition IncidentStopped mark once over restarts (#715).
var storedFields = []storedField{
	{path: "status.gitWaits", size: func(rr *api.RemediationRequest) int { return len(rr.Status.GitWaits) }},
	{path: "status.unfinishedRenders", size: func(rr *api.RemediationRequest) int {
		return one(rr.Status.UnfinishedRenders != nil)
	}},
	{path: "status.pullRequest.incidentStopped", size: func(rr *api.RemediationRequest) int {
		return one(rr.Status.PullRequest != nil && rr.Status.PullRequest.IncidentStopped != nil)
	}},
}

// storedValue is an enum value of a status field added after v0.5.2 that the executor writes: sent returns the
// value a request holds in that field, empty when it holds none.
type storedValue struct {
	path  string
	value string
	sent  func(*api.RemediationRequest) string
}

// storedValues are the values the check covers. ExecutorRestarted records a restart of the executor in the
// executor's verification window (#895).
var storedValues = []storedValue{
	{path: "status.verification.lastReset.reason", value: string(api.ResetExecutorRestarted),
		sent: func(rr *api.RemediationRequest) string {
			if v := rr.Status.Verification; v != nil && v.LastReset != nil {
				return string(v.LastReset.Reason)
			}
			return ""
		}},
}

// refusedValues returns, for a status write of rr that err answered, the stored values the API server refused as
// not supported: the paths, each with the value. Empty for any other error.
func refusedValues(rr *api.RemediationRequest, err error) []string {
	if !apierrors.IsInvalid(err) {
		return nil
	}
	status, ok := errors.AsType[interface {
		error
		apierrors.APIStatus
	}](err)
	if !ok || status.Status().Details == nil {
		return nil
	}
	var out []string
	for _, v := range storedValues {
		if v.sent(rr) != v.value {
			continue
		}
		for _, c := range status.Status().Details.Causes {
			if c.Type == metav1.CauseTypeFieldValueNotSupported && c.Field == v.path {
				out = append(out, fmt.Sprintf("%s (value %s)", v.path, v.value))
				break
			}
		}
	}
	return out
}

func one(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CRDOutdatedError is a status write of a request whose answer from the API server lacks a field the executor
// sent: the installed CustomResourceDefinition of RemediationRequest is older than the executor.
type CRDOutdatedError struct {
	Request types.NamespacedName
	// Fields are the paths of the fields the API server did not store; for a refused value, each path with
	// the value.
	Fields []string
	// Refused is set when the API server refused the write (422 Invalid, a value it does not support) rather
	// than pruning a field from a write that succeeded. Err is that answer.
	Refused bool
	Err     error
}

func (e *CRDOutdatedError) Error() string {
	return fmt.Sprintf("the API server %s %s of RemediationRequest %s: the installed "+
		"CustomResourceDefinition of RemediationRequest is older than the executor; apply the "+
		"CustomResourceDefinitions of this release before the executor (%s)", e.verb(),
		strings.Join(e.Fields, ", "), e.Request, upgradeDocsURL) + e.answer()
}

// answer is the API server's answer of a refused write, for the log: one 422 can name other causes besides the
// value this executor's CRD does not know, and those must not hide behind the advice to apply the CRDs. The
// Event keeps to fixed text (eventMessage).
func (e *CRDOutdatedError) answer() string {
	if !e.Refused || e.Err == nil {
		return ""
	}
	return "; the API server answered: " + e.Err.Error()
}

// Unwrap returns the API server's answer of a refused write, nil for a pruned field.
func (e *CRDOutdatedError) Unwrap() error { return e.Err }

func (e *CRDOutdatedError) verb() string {
	if e.Refused {
		return "refused the status write, as it does not support"
	}
	return "did not store"
}

// eventMessage is the text of the Warning Event: fixed text, field paths and the executor's own enum values only.
func (e *CRDOutdatedError) eventMessage() string {
	return fmt.Sprintf("The API server %s %s: the installed CustomResourceDefinition of "+
		"RemediationRequest is older than the executor. Apply the CustomResourceDefinitions of this release (%s).",
		e.verb(), strings.Join(e.Fields, ", "), upgradeDocsURL)
}

// sizes returns what obj holds of each stored field, nil for an object that is not a request.
func sizes(obj client.Object) []int {
	rr, ok := obj.(*api.RemediationRequest)
	if !ok {
		return nil
	}
	out := make([]int, len(storedFields))
	for i, f := range storedFields {
		out[i] = f.size(rr)
	}
	return out
}

// Status returns the writer of the request status that every status write of the executor uses. It shadows the
// embedded client's Status().
func (r *RemediationRequestReconciler) Status() client.SubResourceWriter {
	return &storedStatusWriter{SubResourceWriter: r.Client.Status(), r: r}
}

// storedStatusWriter checks the answer to each status write of a request (see the comment at the top of the file).
type storedStatusWriter struct {
	client.SubResourceWriter
	r *RemediationRequestReconciler
}

func (w *storedStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return w.r.checkedWrite(obj, func() error { return w.SubResourceWriter.Update(ctx, obj, opts...) })
}

func (w *storedStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch,
	opts ...client.SubResourcePatchOption) error {
	return w.r.checkedWrite(obj, func() error { return w.SubResourceWriter.Patch(ctx, obj, patch, opts...) })
}

// checkedWrite runs write, a status write of obj, unless an earlier write of the same request in this reconcile lost
// a field, and compares what the answer holds with what was sent.
func (r *RemediationRequestReconciler) checkedWrite(obj client.Object, write func() error) error {
	if _, ok := obj.(*api.RemediationRequest); !ok {
		return write()
	}
	key := client.ObjectKeyFromObject(obj)
	if prev, ok := r.outdatedCRD.Load(key); ok {
		return prev.(error)
	}
	sent := sizes(obj)
	var e *CRDOutdatedError
	if err := write(); err != nil {
		refused := refusedValues(obj.(*api.RemediationRequest), err)
		if len(refused) == 0 {
			return err
		}
		e = &CRDOutdatedError{Request: key, Fields: refused, Refused: true, Err: err}
	} else {
		got := sizes(obj)
		var lost []string
		for i, f := range storedFields {
			if got[i] < sent[i] {
				lost = append(lost, f.path)
			}
		}
		if len(lost) == 0 {
			return nil
		}
		e = &CRDOutdatedError{Request: key, Fields: lost}
	}
	err := &clusterError{err: e}
	r.outdatedCRD.Store(key, err)
	if r.Recorder != nil {
		r.Recorder.Event(obj, corev1.EventTypeWarning, EventCustomResourceDefinitionOutdated, e.eventMessage())
	}
	return err
}

// takeOutdatedCRD returns the error of a status write of key that lost a field since the last call, and forgets it.
func (r *RemediationRequestReconciler) takeOutdatedCRD(key types.NamespacedName) error {
	if v, ok := r.outdatedCRD.LoadAndDelete(key); ok {
		return v.(error)
	}
	return nil
}

// IsCRDOutdated reports whether err comes from a status write the installed CustomResourceDefinition pruned.
func IsCRDOutdated(err error) bool {
	_, ok := errors.AsType[*CRDOutdatedError](err)
	return ok
}
