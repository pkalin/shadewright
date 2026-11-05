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

// Package ownership holds the bounds the analyzer applies when it follows the owner references and label
// selectors of workloads. The collector correlates an event to its owning workload with them, and the context
// builder (internal/incidentcontext) resolves a workload's pods with them, so both follow the same objects
// and refuse the same ones. Real workloads have a few labels and a short selector; anything larger is not
// followed, so a cache or a context cannot be filled with large objects.
package ownership

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	// MaxLabels bounds the labels of an object that is followed.
	MaxLabels = 64
	// MaxSelectorTerms bounds the terms of a selector that is followed: each matchLabels entry, and each
	// matchExpressions entry plus its values.
	MaxSelectorTerms = 64
)

// Selector converts a workload's selector. It refuses an empty one, which would select everything (the
// apps/v1 API refuses it too), one with more than [MaxSelectorTerms] terms, and one that does not convert.
func Selector(ls *metav1.LabelSelector) (labels.Selector, bool) {
	if ls == nil || len(ls.MatchLabels)+len(ls.MatchExpressions) == 0 {
		return nil, false
	}
	terms := len(ls.MatchLabels)
	for _, e := range ls.MatchExpressions {
		terms += 1 + len(e.Values)
	}
	if terms > MaxSelectorTerms {
		return nil, false
	}
	s, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return nil, false
	}
	return s, true
}

// Selects reports whether sel matches the object's labels, refusing an object with more than [MaxLabels].
func Selects(sel labels.Selector, obj metav1.Object) bool {
	l := obj.GetLabels()
	return len(l) <= MaxLabels && sel.Matches(labels.Set(l))
}
