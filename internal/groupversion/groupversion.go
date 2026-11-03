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

// Package groupversion checks Kubernetes API group versions ("apps/v1", "keda.sh/v1alpha1", or "v1" for the
// core group), in the form an object's apiVersion and the discovery API's groupVersion use.
package groupversion

import "regexp"

// Pattern is the apiVersion pattern of the Incident's ObjectRef schema (api/v1alpha1/common_types.go) and of
// an integration's detected version in RemediationSettings status: an optional DNS subdomain group and "/",
// then a Kubernetes version (v1, v1alpha1, v2beta3). A test keeps them equal.
const Pattern = `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?v[0-9]+((alpha|beta)[0-9]+)?$`

// MaxLength is the longest group version accepted, the ObjectRef schema's bound.
const MaxLength = 253

var pattern = regexp.MustCompile(Pattern)

// Valid reports whether s is a group version that fits the ObjectRef schema.
func Valid(s string) bool { return len(s) <= MaxLength && pattern.MatchString(s) }
