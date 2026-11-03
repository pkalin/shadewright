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

package keda

import "github.com/pkalin/shadewright/internal/kedaready"

// KedaReady is the KedaReady verification check, for verification (planned, #19): it holds when obj is a
// keda.sh/v1alpha1 ScaledObject whose Ready condition is True, which is not paused (no pause annotation, and not
// Paused=True, which KEDA also sets for a scale-in or scale-out pause), not being deleted, whose triggers are
// not falling back (Fallback=True) and whose HPA can read its metrics (HPAActive is not False). A ScaledObject
// on fallback, or whose HPA cannot read its metrics, can report Ready=True while it does not scale on its
// metrics, so it does not count as ready. A missing HPAActive (no HPA observed yet, or a KEDA release before
// 2.21) does not fail the check.
//
// The rule is [kedaready.IsReady], the same function the policy gate of AdjustKedaMaxReplicas runs at the
// decision and before every write (design sections 5.1 and 6), so the check and the gate cannot drift apart.
//
// KEDA's conditions carry no observedGeneration, so the check cannot tell whether Ready describes the
// delivered spec: verification is to run it after DeliveryRevisionApplied and require it to hold for
// stableFor (Verification in the API). It only reads obj, never changes it, and does no I/O.
func KedaReady(obj map[string]any) bool { return kedaready.IsReady(obj) }
