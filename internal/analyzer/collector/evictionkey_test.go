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
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// The kubelet's eviction messages, copied from pkg/kubelet/eviction (helpers.go, eviction_manager.go) of
// Kubernetes v1.37.2, the release of k8s.io/api in go.mod. The kubelet writes the same text as the pod's
// status.message and as the note of its Evicted event.
const (
	kubeletNodeLowFmt              = "The node was low on resource: %v. "
	kubeletNodeConditionFmt        = "The node had condition: %v. "
	kubeletContainerFmt            = "Container %s was using %s, request is %s, has larger consumption of %v. "
	kubeletPodFmt                  = "Pod %s was using %s, request is %s, has larger consumption of %v. "
	kubeletContainerEphemeralFmt   = "Container %s exceeded its local ephemeral storage limit %q. "
	kubeletPodEphemeralFmt         = "Pod ephemeral local storage usage exceeds the total limit of containers %s. "
	kubeletEmptyDirFmt             = "Usage of EmptyDir volume %q exceeds the limit %q. "
	kubeletThresholdMetFmt         = "Threshold quantity: %v, available: %v. "
	corpusInstancesPerShape        = 12
	corpusOwner                    = "6c2d1f0a-1b2c-4d3e-8f4a-5b6c7d8e9f00"
	corpusK8sAlphabet              = "bcdfghjklmnpqrstvwxz2456789"
	corpusEvictedContainerExitCode = 137
)

// quantity returns a quantity in one of the forms resource.Quantity prints: binary SI (91236Ki, 1Gi, or a
// plain byte count), decimal SI (1500M), milli (250m), zero, and, when negative is set, a negative one (the
// kubelet's "available" is below zero once usage passes capacity).
func corpusQuantity(r *rand.Rand, negative bool) string {
	n := r.Int64N(64<<30) + 1
	if negative && r.IntN(3) == 0 {
		n = -n
	}
	var q *resource.Quantity
	switch r.IntN(6) {
	case 0:
		q = resource.NewQuantity(n, resource.BinarySI)
	case 1:
		q = resource.NewQuantity(n&^1023, resource.BinarySI) // a whole number of Ki, Mi or Gi
	case 2:
		q = resource.NewQuantity(n, resource.DecimalSI)
	case 3:
		q = resource.NewMilliQuantity(r.Int64N(4000)+1, resource.DecimalSI)
	case 4:
		q = resource.NewQuantity((r.Int64N(64)+1)<<30, resource.BinarySI)
	default:
		q = resource.NewQuantity(0, resource.BinarySI)
	}
	return q.String()
}

// corpusPodName is a generated pod name of the web ReplicaSet, as each re-created pod gets one.
func corpusPodName(r *rand.Rand) string {
	b := make([]byte, 5)
	for i := range b {
		b[i] = corpusK8sAlphabet[r.IntN(len(corpusK8sAlphabet))]
	}
	return "web-7d4b9c8f6-" + string(b)
}

// shape is one kind of eviction: the message the kubelet writes for one evicted pod, from the generator's
// quantities. Two messages of one shape differ only in the quantities and the pod's own name.
type shape struct {
	name string
	gen  func(r *rand.Rand, pod string) string
}

func nodeLow(res string, threshold bool, podMsg bool, containers ...string) func(*rand.Rand, string) string {
	return func(r *rand.Rand, pod string) string {
		m := fmt.Sprintf(kubeletNodeLowFmt, res)
		if threshold {
			m += fmt.Sprintf(kubeletThresholdMetFmt, corpusQuantity(r, false), corpusQuantity(r, true))
		}
		if podMsg {
			m += fmt.Sprintf(kubeletPodFmt, pod, corpusQuantity(r, false), corpusQuantity(r, false), res)
		}
		for _, c := range containers {
			m += fmt.Sprintf(kubeletContainerFmt, c, corpusQuantity(r, false), corpusQuantity(r, false), res)
		}
		return m
	}
}

func corpusShapes() []shape {
	return []shape{
		{"memory, one container", nodeLow("memory", true, false, "app")},
		{"ephemeral-storage, one container", nodeLow("ephemeral-storage", true, false, "app")},
		{"inodes, no container", nodeLow("inodes", true, false)},
		{"memory, other container", nodeLow("memory", true, false, "proxy")},
		// A name like "500m" would read as a duration to fingerprint.Normalize, which runs after this rule for every
		// note; "500k" is a quantity to this rule only.
		{"memory, container named like a quantity", nodeLow("memory", true, false, "500k")},
		{"memory, container named like another quantity", nodeLow("memory", true, false, "600k")},
		{"memory, container with a digit", nodeLow("memory", true, false, "web-1")},
		{"memory, container with another digit", nodeLow("memory", true, false, "web-2")},
		{"memory, two containers", nodeLow("memory", true, false, "app", "proxy")},
		{"memory, no threshold", nodeLow("memory", false, false, "app")},
		{"memory, pod level", nodeLow("memory", true, true, "app")},
		{"container ephemeral limit app", func(r *rand.Rand, _ string) string {
			return fmt.Sprintf(kubeletContainerEphemeralFmt, "app", corpusQuantity(r, false))
		}},
		{"container ephemeral limit proxy", func(r *rand.Rand, _ string) string {
			return fmt.Sprintf(kubeletContainerEphemeralFmt, "proxy", corpusQuantity(r, false))
		}},
		{"pod ephemeral limit", func(r *rand.Rand, _ string) string {
			return fmt.Sprintf(kubeletPodEphemeralFmt, corpusQuantity(r, false))
		}},
		{"emptyDir cache", func(r *rand.Rand, _ string) string {
			return fmt.Sprintf(kubeletEmptyDirFmt, "cache", corpusQuantity(r, false))
		}},
		{"emptyDir data-1", func(r *rand.Rand, _ string) string {
			return fmt.Sprintf(kubeletEmptyDirFmt, "data-1", corpusQuantity(r, false))
		}},
		{"emptyDir named like a quantity", func(r *rand.Rand, _ string) string {
			return fmt.Sprintf(kubeletEmptyDirFmt, "100m", corpusQuantity(r, false))
		}},
		{"emptyDir named like another quantity", func(r *rand.Rand, _ string) string {
			return fmt.Sprintf(kubeletEmptyDirFmt, "200m", corpusQuantity(r, false))
		}},
		{"node condition memory", func(*rand.Rand, string) string {
			return fmt.Sprintf(kubeletNodeConditionFmt, "[MemoryPressure]")
		}},
		{"node condition disk", func(*rand.Rand, string) string {
			return fmt.Sprintf(kubeletNodeConditionFmt, "[DiskPressure]")
		}},
		{"node condition both", func(*rand.Rand, string) string {
			return fmt.Sprintf(kubeletNodeConditionFmt, "[MemoryPressure DiskPressure]")
		}},
	}
}

// corpusFingerprints returns, for corpusInstancesPerShape evicted pods of one workload with messages of shape
// s, the eviction fingerprints the collector gives and the fingerprints main gave the same pods: their
// container's termination with exit code 137, which main read in place of the eviction.
func corpusFingerprints(c *Collector, s shape, seed uint64) (evictions, mainTerminations map[string]string) {
	r := rand.New(rand.NewPCG(seed, 432)) // #nosec G404 -- a seeded generator, so the corpus is reproducible
	evictions, mainTerminations = map[string]string{}, map[string]string{}
	for i := range corpusInstancesPerShape {
		pod := specPod()
		pod.Name = corpusPodName(r)
		pod.UID = types.UID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		msg := s.gen(r, pod.Name)
		evictions[c.evictionFingerprint(testNS, corpusOwner, pod.Name, pod.UID, msg)] = msg
		main := fingerprint.Fingerprint(c.podStatusInput(pod, corpusOwner, reasonError,
			terminationNote(containerApp, corpusEvictedContainerExitCode)))
		mainTerminations[main] = msg
	}
	return evictions, mainTerminations
}

func corpusCollector(t *testing.T) *Collector {
	t.Helper()
	c, err := New(Config{ClusterID: testCluster})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestEvictionFingerprintCorpusSameShape: the evictions of one workload whose messages differ only in their
// quantities and the pod's name share one fingerprint, as their exit-137 terminations did on main.
func TestEvictionFingerprintCorpusSameShape(t *testing.T) {
	t.Parallel()
	c := corpusCollector(t)
	for i, s := range corpusShapes() {
		evictions, main := corpusFingerprints(c, s, uint64(i))
		if len(evictions) > len(main) {
			t.Errorf("%s: %d fingerprints for one workload's evictions, main had %d; messages: %q", s.name,
				len(evictions), len(main), values(evictions, 3))
		}
		if len(evictions) != 1 {
			t.Errorf("%s: %d fingerprints, want 1", s.name, len(evictions))
		}
	}
}

// TestEvictionFingerprintCorpusDistinctShapes: evictions for another resource, another container or volume,
// another node condition or another message format never share a fingerprint, whatever the quantities. A
// name that reads as a quantity (a container "500k") is a name and is kept.
func TestEvictionFingerprintCorpusDistinctShapes(t *testing.T) {
	t.Parallel()
	c := corpusCollector(t)
	owner := map[string]string{}
	for i, s := range corpusShapes() {
		evictions, _ := corpusFingerprints(c, s, uint64(i))
		for fp := range evictions {
			if prev, ok := owner[fp]; ok && prev != s.name {
				t.Errorf("shapes %q and %q share fingerprint %s", prev, s.name, fp)
			}
			owner[fp] = s.name
		}
	}
}

// TestEvictionFingerprintCorpusKeepsText: only the quantities of a kubelet sentence are replaced. The key of
// a message is the message with each quantity replaced by one placeholder, and a text that is no kubelet
// sentence, or a sentence around one, is kept byte for byte.
func TestEvictionFingerprintCorpusKeepsText(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- a seeded generator, so the corpus is reproducible
	for range 200 {
		used, avail := corpusQuantity(r, false), corpusQuantity(r, true)
		limit := corpusQuantity(r, false)
		msg := fmt.Sprintf(kubeletThresholdMetFmt, used, avail) + fmt.Sprintf(kubeletEmptyDirFmt, "cache", limit)
		want := fmt.Sprintf(kubeletThresholdMetFmt, quantityPlaceholder, quantityPlaceholder) +
			fmt.Sprintf(kubeletEmptyDirFmt, "cache", quantityPlaceholder)
		if got := evictionFingerprintNote(msg); got != want {
			t.Fatalf("key of %q = %q, want %q", msg, got, want)
		}
		around := "Pod was rejected 3 times. " + msg + "Retried 5Mi later."
		wantAround := "Pod was rejected 3 times. " + want + "Retried 5Mi later."
		if got := evictionFingerprintNote(around); got != wantAround {
			t.Fatalf("key of %q = %q, want %q", around, got, wantAround)
		}
	}
	// Not a kubelet sentence with a quantity: kept as it is.
	for _, s := range []string{
		"", "Pod was evicted", "The node was low on resource: memory.",
		"Threshold quantity: 1Gi available: 2Gi. ",                                          // no comma
		"Usage of EmptyDir volume \"a\" exceeds the limit \"x1Gi\". ",                       // not a quantity
		"Threshold quantity: 1Gi, available: 2Gi.Threshold quantity: 1Gi, available: 2Gi. ", // glued
		"Container app was using 1Gi, request is 2Gi, has larger consumption of memory and more. ",
		"Pod ephemeral local storage usage exceeds the total limit of containers 1Gi 2Gi. ",
	} {
		if got := evictionFingerprintNote(s); got != s {
			t.Errorf("key of %q = %q, want it unchanged", s, got)
		}
	}
}

// TestEvictionSymptomKeepsTheMessage: the fingerprint drops the quantities, the symptom shown for the eviction
// does not.
func TestEvictionSymptomKeepsTheMessage(t *testing.T) {
	d, rs, _ := workload()
	h := newHarness(t, Config{}, d, rs)
	r := rand.New(rand.NewPCG(7, 7)) // #nosec G404 -- a seeded generator, so the corpus is reproducible
	fps := make([]string, 0, 3)
	for i := range 3 {
		name := corpusPodName(r)
		used := resource.NewQuantity(int64(91236+i)<<10, resource.BinarySI).String()
		pod := evictedPod(name, types.UID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i)), reasonError,
			"containerd://"+name, t0)
		pod.Status.Message = fmt.Sprintf(kubeletNodeLowFmt, "memory") +
			fmt.Sprintf(kubeletThresholdMetFmt, "100Mi", "-"+used) +
			fmt.Sprintf(kubeletContainerFmt, containerApp, used, "64Mi", "memory")
		h.clock.set(t0)
		h.create(pod)
		h.reconcilePod(name)
		o := h.one()
		if !strings.Contains(o.Symptom, used) || !strings.Contains(o.Symptom, "64Mi") {
			t.Errorf("symptom %q lost a quantity of the message (%s)", o.Symptom, used)
		}
		fps = append(fps, o.Fingerprint)
	}
	if fps[0] != fps[1] || fps[1] != fps[2] {
		t.Errorf("three node-pressure evictions of one workload on %v, want one fingerprint", fps)
	}
}

func values(m map[string]string, n int) []string {
	var out []string
	for _, v := range m {
		if len(out) == n {
			break
		}
		out = append(out, v)
	}
	return out
}

// FuzzEvictionFingerprintNote: the rule never fails, gives the same key for its own key, and changes no text
// without a digit (a quantity has one).
func FuzzEvictionFingerprintNote(f *testing.F) {
	r := rand.New(rand.NewPCG(3, 3)) // #nosec G404 -- a seeded generator, so the corpus is reproducible
	for i, s := range corpusShapes() {
		f.Add(s.gen(r, fmt.Sprintf("web-%d", i)))
	}
	f.Add("Threshold quantity: 1Gi, available: 2Gi.Threshold quantity: 1Gi, available: 2Gi. ")
	f.Fuzz(func(t *testing.T, s string) {
		k := evictionFingerprintNote(s)
		if again := evictionFingerprintNote(k); again != k {
			t.Errorf("key of key %q = %q, want it unchanged", k, again)
		}
		if !strings.ContainsAny(s, "0123456789") && k != s {
			t.Errorf("key of %q = %q, a text without a quantity changed", s, k)
		}
	})
}
