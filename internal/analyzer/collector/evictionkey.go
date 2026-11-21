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

package collector

import (
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// quantityPlaceholder stands for a quantity of a kubelet eviction sentence in the fingerprint input.
const quantityPlaceholder = "<quantity>"

// quantityRe is one quantity as resource.Quantity prints it: an optional sign, a number, and an exponent or a
// binary or decimal suffix ("91236Ki", "1Gi", "1500M", "250m", "-12345", "1e3").
const quantityRe = `[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+|[KMGTPE]i|[numkMGTPE])?`

// evictionSentence is one sentence the kubelet's eviction manager writes, with the submatches that are
// quantities.
type evictionSentence struct {
	re         *regexp.Regexp
	quantities []int
}

// evictionSentences are the sentences of pkg/kubelet/eviction (Kubernetes v1.37) that carry a quantity, each
// matched as one whole sentence: a name is matched as a name (a container "500m" stays), and a sentence that
// differs in any word is no match and stays as it is. Every eviction message is a run of such sentences, each
// ending in ". ".
var evictionSentences = []evictionSentence{
	// thresholdMetMessageFmt
	{regexp.MustCompile(`^Threshold quantity: (` + quantityRe + `), available: (` + quantityRe + `)\. ?$`),
		[]int{1, 2}},
	// containerMessageFmt and podMessageFmt
	{regexp.MustCompile(`^(?:Container|Pod) \S+ was using (` + quantityRe + `), request is (` + quantityRe +
		`), has larger consumption of \S+\. ?$`), []int{1, 2}},
	// containerEphemeralStorageMessageFmt
	{regexp.MustCompile(`^Container \S+ exceeded its local ephemeral storage limit "(` + quantityRe + `)"\. ?$`),
		[]int{1}},
	// podEphemeralStorageMessageFmt
	{regexp.MustCompile(`^Pod ephemeral local storage usage exceeds the total limit of containers (` + quantityRe +
		`)\. ?$`), []int{1}},
	// emptyDirMessageFmt
	{regexp.MustCompile(`^Usage of EmptyDir volume "[^"]*" exceeds the limit "(` + quantityRe + `)"\. ?$`),
		[]int{1}},
}

// evictionFingerprintNote returns the fingerprint input of an eviction message (ADR-0039 point 3, decided
// 2026-11-21): message with every quantity of a kubelet eviction sentence replaced by quantityPlaceholder, so
// the evictions of one workload for one resource share a fingerprint whatever the usage, request, threshold or
// limit. Only the quantity is replaced; every other byte, and any text that is no such sentence, is kept. The
// shown symptom is built from the whole message, never from this.
func evictionFingerprintNote(message string) string {
	var b strings.Builder
	for _, s := range strings.SplitAfter(message, ". ") {
		b.WriteString(keySentence(s))
	}
	return b.String()
}

// keySentence replaces the quantities of s when it is one of evictionSentences, and returns it as it is
// otherwise.
func keySentence(s string) string {
	for _, es := range evictionSentences {
		m := es.re.FindStringSubmatchIndex(s)
		if m == nil {
			continue
		}
		var b strings.Builder
		pos := 0
		for _, g := range es.quantities {
			b.WriteString(s[pos:m[2*g]])
			b.WriteString(quantityPlaceholder)
			pos = m[2*g+1]
		}
		b.WriteString(s[pos:])
		return b.String()
	}
	return s
}

// evictionInput is the fingerprint input of a pod's eviction with note as its note.
func (c *Collector) evictionInput(namespace, owner, podName string, podUID types.UID,
	note string) fingerprint.Observation {
	return fingerprint.Observation{
		ClusterID:           c.cfg.ClusterID,
		Namespace:           namespace,
		OwnerUID:            owner,
		ResourceUID:         string(podUID),
		Reason:              ReasonEvicted,
		Type:                corev1.EventTypeWarning,
		ReportingController: reportingKubelet,
		Note:                note,
		RegardingKind:       kindPod,
		RegardingName:       podName,
		Source:              fingerprint.SourcePodStatus,
	}
}

// evictionFingerprint is the fingerprint of a pod's eviction with the kubelet's message.
func (c *Collector) evictionFingerprint(namespace, owner, podName string, podUID types.UID, message string) string {
	return fingerprint.Fingerprint(c.evictionInput(namespace, owner, podName, podUID,
		evictionFingerprintNote(evictionNote(message))))
}
