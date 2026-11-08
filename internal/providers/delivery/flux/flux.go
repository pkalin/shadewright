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

// Package flux is the Flux adapter of [providers.DeliveryVerifier] (ADR-0013, design 7.3 and 8.2 step 8). It
// reads one Kustomization or HelmRelease with get, as an unstructured object, and reports its status through
// internal/fluxstatus, the same reading the source mapping uses before a write: whether it is Ready for its
// current generation, the generation it observed, the commit its applied and attempted revisions name, and
// whether it reports a failure. [providers.DeliveryStatus.Outcome] then compares that with the merge commit.
//
// Only a Kustomization whose source is a GitRepository names a commit: Flux writes its revisions as
// <branch>@sha1:<commit>. A HelmRelease's revisions are chart versions, which name no commit, so its
// AppliedCommit stays empty and its outcome is never Applied; the delivery of a commit to a HelmRelease is
// shown by the Kustomization that applies it.
//
// It reads only: the caller needs get on the kind in the unit's namespace, nothing else. It does not retry; a
// failed read is classified (providers.ErrNotFound, providers.ErrForbidden, a [providers.RetryableError] or the
// context's error) and never reported as a status.
package flux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/fluxstatus"
	"github.com/pkalin/shadewright/internal/providers"
)

// MaxMessageBytes bounds the message copied from the unit's Ready condition.
const MaxMessageBytes = 1024

// ErrInvalidUnit means a unit is not one this adapter can read: another kind, or a namespace or name that is
// not a valid object reference. It is permanent for that unit.
var ErrInvalidUnit = errors.New("invalid delivery unit")

// kinds are the units read, by kind.
var kinds = map[string]schema.GroupVersionKind{
	fluxstatus.KindKustomization: {
		Group: fluxstatus.KustomizeGroup, Version: fluxstatus.KustomizeVersion, Kind: fluxstatus.KindKustomization},
	fluxstatus.KindHelmRelease: {
		Group: fluxstatus.HelmGroup, Version: fluxstatus.HelmVersion, Kind: fluxstatus.KindHelmRelease},
}

// Verifier reads Flux delivery units.
type Verifier struct {
	reader client.Reader
}

var _ providers.DeliveryVerifier = (*Verifier)(nil)

// New returns a Verifier that reads with reader, for example the executor's API reader.
func New(reader client.Reader) (*Verifier, error) {
	if reader == nil {
		return nil, fmt.Errorf("reader is required: %w", providers.ErrInvalidConfig)
	}
	return &Verifier{reader: reader}, nil
}

// Status reads the unit and reports its status.
func (v *Verifier) Status(ctx context.Context, unit providers.DeliveryUnit) (providers.DeliveryStatus, error) {
	gvk, ok := kinds[unit.Kind]
	if !ok {
		return providers.DeliveryStatus{}, fmt.Errorf("kind is not Kustomization or HelmRelease: %w", ErrInvalidUnit)
	}
	if len(validation.IsDNS1123Label(unit.Namespace)) > 0 || len(validation.IsDNS1123Subdomain(unit.Name)) > 0 {
		return providers.DeliveryStatus{}, fmt.Errorf("namespace or name is not a valid object reference: %w", ErrInvalidUnit)
	}
	if err := ctx.Err(); err != nil {
		return providers.DeliveryStatus{}, fmt.Errorf("read %s: %w", unit.Kind, err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := v.reader.Get(ctx, client.ObjectKey{Namespace: unit.Namespace, Name: unit.Name}, obj); err != nil {
		return providers.DeliveryStatus{}, readError(ctx, unit.Kind, err)
	}
	if obj.GetNamespace() != unit.Namespace || obj.GetName() != unit.Name || obj.GroupVersionKind() != gvk {
		return providers.DeliveryStatus{}, &providers.RetryableError{
			Err: fmt.Errorf("read %s: the reader returned another object", unit.Kind)}
	}
	return statusOf(unit.Kind, obj.Object), nil
}

// statusOf reads the status of a unit of kind from its object.
func statusOf(kind string, obj map[string]any) providers.DeliveryStatus {
	status := fluxstatus.Status(obj)
	generation, hasGeneration := fluxstatus.Generation(obj)
	observed, _ := fluxstatus.ObservedGeneration(obj)
	st := providers.DeliveryStatus{
		AppliedRevision:    fluxstatus.AppliedRevision(status),
		Ready:              fluxstatus.Ready(obj),
		Generation:         max(generation, 0),
		ObservedGeneration: max(observed, 0),
		Message:            bounded(fluxstatus.ReadyMessage(obj)),
	}
	cond, typ := fluxstatus.Failing(status, generation, hasGeneration)
	reason, _ := cond["reason"].(string)
	st.Failed = cond != nil && (typ != fluxstatus.ConditionReady || reason != fluxstatus.ReasonHealthCheckCanceled)
	if kind == fluxstatus.KindKustomization && fluxstatus.SourceKind(obj) == fluxstatus.KindGitRepository {
		st.AppliedCommit = commitOf(st.AppliedRevision)
		st.AttemptedCommit = commitOf(fluxstatus.AttemptedRevision(status))
	}
	return st
}

// commitOf returns the commit ID a Git source revision names, or "".
func commitOf(revision string) string {
	_, commit, ok := fluxstatus.ParseRevision(revision)
	if !ok {
		return ""
	}
	return commit
}

// bounded returns s as valid UTF-8 and at most MaxMessageBytes long, cut at a rune boundary.
func bounded(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= MaxMessageBytes {
		return s
	}
	cut := MaxMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// readError classifies a failed read. The API server's text is dropped: it can repeat names from the request.
func readError(ctx context.Context, kind string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("read %s: %w", kind, cerr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("read %s: %w", kind, err)
	}
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return fmt.Errorf("read %s: %w", kind, providers.ErrNotFound)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return fmt.Errorf("read %s: the API server refused the identity: %w", kind, providers.ErrForbidden)
	}
	return &providers.RetryableError{Err: fmt.Errorf("read %s failed", kind)}
}
