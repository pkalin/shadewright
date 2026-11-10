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
package patch

// Reason says why a patch was refused. Each is stable and safe to show in a condition.
type Reason string

const (
	// ReasonPreviousValueDiffers means the file no longer holds the value the plan was made against.
	ReasonPreviousValueDiffers Reason = "PreviousValueDiffers"
	// ReasonFieldMissing means the object, or a field or container on the path to the value, is absent.
	ReasonFieldMissing Reason = "FieldMissing"
	// ReasonAmbiguousContainer means more than one container has the requested name.
	ReasonAmbiguousContainer Reason = "AmbiguousContainer"
	// ReasonTargetNotFound means no document declares the target object.
	ReasonTargetNotFound Reason = "TargetNotFound"
	// ReasonAmbiguousTarget means more than one document may declare the target object, also with another
	// version or without a namespace.
	ReasonAmbiguousTarget Reason = "AmbiguousTarget"
	// ReasonUnsupported means the YAML uses a construct the in-place splice does not handle (ADR-0020).
	ReasonUnsupported Reason = "Unsupported"
	// ReasonInvalidInput means the target, action parameters or value are not usable.
	ReasonInvalidInput Reason = "InvalidInput"
	// ReasonInvalidYAML means the file does not parse, has a duplicate key, is over MaxBytes, MaxNodes or
	// gityaml.MaxMappingKeys keys in one mapping, or fails the check of internal/gitops/gityaml (anchors,
	// aliases, merge keys and the forms that hide them).
	ReasonInvalidYAML Reason = "InvalidYAML"
	// ReasonVerificationFailed means the patched file differs from the original in more than the target value.
	ReasonVerificationFailed Reason = "VerificationFailed"
)

// Error is a typed refusal. Detail names only validated identifiers, never text from the document.
type Error struct {
	Reason Reason
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "patch: " + string(e.Reason)
	}
	return "patch: " + string(e.Reason) + ": " + e.Detail
}

// Is reports whether target is an *Error with the same Reason and no detail, so errors.Is(err, ErrX) works.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Detail == "" && t.Reason == e.Reason
}

// Sentinels for errors.Is, one per reason.
var (
	ErrPreviousValueDiffers = &Error{Reason: ReasonPreviousValueDiffers}
	ErrFieldMissing         = &Error{Reason: ReasonFieldMissing}
	ErrAmbiguousContainer   = &Error{Reason: ReasonAmbiguousContainer}
	ErrTargetNotFound       = &Error{Reason: ReasonTargetNotFound}
	ErrAmbiguousTarget      = &Error{Reason: ReasonAmbiguousTarget}
	ErrUnsupported          = &Error{Reason: ReasonUnsupported}
	ErrInvalidInput         = &Error{Reason: ReasonInvalidInput}
	ErrInvalidYAML          = &Error{Reason: ReasonInvalidYAML}
	ErrVerificationFailed   = &Error{Reason: ReasonVerificationFailed}
)

func refuse(r Reason, detail string) error { return &Error{Reason: r, Detail: detail} }
