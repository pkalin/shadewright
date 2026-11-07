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
package semdiff

// Reason says why a change was refused. Each is stable and safe to show in a condition.
type Reason string

const (
	// ReasonInvalidInput means the target, action, paths or file set given to the check are not usable.
	ReasonInvalidInput Reason = "InvalidInput"
	// ReasonUnsupportedKustomization means a kustomization file uses a field the renderer does not allow:
	// generators, Helm charts, plugins, OpenAPI schemas, replacements or anything else outside the allow-list.
	ReasonUnsupportedKustomization Reason = "UnsupportedKustomization"
	// ReasonInvalidReference means a kustomization file refers to something that is not a plain relative path
	// inside the repository: a URL, a remote base, an absolute path or a path that leaves the repository.
	ReasonInvalidReference Reason = "InvalidReference"
	// ReasonFileMissing means the build needs a file that is not in the given set or not in the repository,
	// or a directory has no kustomization file or more than one.
	ReasonFileMissing Reason = "FileMissing"
	// ReasonTooLarge means the build is over a limit: file size, number of files or reads, nesting depth, the
	// number of directory visits, the build bytes ([MaxBuildBytes]), the YAML documents read before the render
	// ([MaxBuildDocuments]) or the rendered objects ([MaxObjects]).
	ReasonTooLarge Reason = "TooLarge"
	// ReasonInvalidYAML means a kustomization file does not parse or has a duplicate key, or a file of the
	// build is not UTF-8, does not parse, or uses YAML anchors, aliases or merge keys, which kustomize would
	// expand before the document count could bound them ([documents]).
	ReasonInvalidYAML Reason = "InvalidYAML"
	// ReasonRenderFailed means kustomize could not build the path, before or after the change.
	ReasonRenderFailed Reason = "RenderFailed"
	// ReasonFileNotInBuild means the changed file is not a resource of the build.
	ReasonFileNotInBuild Reason = "FileNotInBuild"
	// ReasonTargetNotFound means the rendered output does not hold the target object.
	ReasonTargetNotFound Reason = "TargetNotFound"
	// ReasonAmbiguousTarget means the rendered output may hold the target more than once, also with another
	// version or without a namespace, or holds two objects with the same identity.
	ReasonAmbiguousTarget Reason = "AmbiguousTarget"
	// ReasonObjectsAddedOrRemoved means the change adds or removes a rendered object.
	ReasonObjectsAddedOrRemoved Reason = "ObjectsAddedOrRemoved"
	// ReasonOtherObjectChanged means the change alters a rendered object other than the target, for example
	// through a base that another object is also built from.
	ReasonOtherObjectChanged Reason = "OtherObjectChanged"
	// ReasonOtherFieldChanged means the change alters a field of the target other than the one the action
	// changes.
	ReasonOtherFieldChanged Reason = "OtherFieldChanged"
	// ReasonPreviousValueDiffers means the rendered target before the change does not hold the previous value
	// of the plan, so something else in the build sets the field.
	ReasonPreviousValueDiffers Reason = "PreviousValueDiffers"
	// ReasonDesiredValueMissing means the rendered target after the change does not hold the desired value,
	// for example because a patch in an overlay overrides it.
	ReasonDesiredValueMissing Reason = "DesiredValueMissing"
	// ReasonFieldNotFound means the rendered target before the change does not set the field the action
	// changes.
	ReasonFieldNotFound Reason = "FieldNotFound"
	// ReasonSourceIgnored means a .sourceignore file applies to a directory of the build. Flux would filter
	// the files by it, so the render could differ from what Flux builds; the rules are not interpreted.
	ReasonSourceIgnored Reason = "SourceIgnored"
	// ReasonSchemaInvalid means the changed object fails the basic schema checks.
	ReasonSchemaInvalid Reason = "SchemaInvalid"
)

// Error is a typed refusal. Detail names only validated identifiers and paths, never text from a file or
// from kustomize, which may be untrusted.
type Error struct {
	Reason Reason
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "semdiff: " + string(e.Reason)
	}
	return "semdiff: " + string(e.Reason) + ": " + e.Detail
}

// Is reports whether target is an *Error with the same Reason and no detail, so errors.Is(err, ErrX) works.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Detail == "" && t.Reason == e.Reason
}

// Sentinels for errors.Is, one per reason.
var (
	ErrInvalidInput             = &Error{Reason: ReasonInvalidInput}
	ErrUnsupportedKustomization = &Error{Reason: ReasonUnsupportedKustomization}
	ErrInvalidReference         = &Error{Reason: ReasonInvalidReference}
	ErrFileMissing              = &Error{Reason: ReasonFileMissing}
	ErrTooLarge                 = &Error{Reason: ReasonTooLarge}
	ErrInvalidYAML              = &Error{Reason: ReasonInvalidYAML}
	ErrRenderFailed             = &Error{Reason: ReasonRenderFailed}
	ErrFileNotInBuild           = &Error{Reason: ReasonFileNotInBuild}
	ErrTargetNotFound           = &Error{Reason: ReasonTargetNotFound}
	ErrAmbiguousTarget          = &Error{Reason: ReasonAmbiguousTarget}
	ErrObjectsAddedOrRemoved    = &Error{Reason: ReasonObjectsAddedOrRemoved}
	ErrOtherObjectChanged       = &Error{Reason: ReasonOtherObjectChanged}
	ErrOtherFieldChanged        = &Error{Reason: ReasonOtherFieldChanged}
	ErrPreviousValueDiffers     = &Error{Reason: ReasonPreviousValueDiffers}
	ErrDesiredValueMissing      = &Error{Reason: ReasonDesiredValueMissing}
	ErrFieldNotFound            = &Error{Reason: ReasonFieldNotFound}
	ErrSourceIgnored            = &Error{Reason: ReasonSourceIgnored}
	ErrSchemaInvalid            = &Error{Reason: ReasonSchemaInvalid}
)

func refuse(r Reason, detail string) error { return &Error{Reason: r, Detail: detail} }
