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

package fingerprint

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenV2 holds the fingerprints of corpus(), computed by the fingerprint code before the source of an
// observation was hashed (ADR-0039 point 2). Regenerate it only for a deliberate change of every
// fingerprint, which needs a new version string: SHADEWRIGHT_UPDATE_GOLDEN=1 go test -run TestCorpusGolden.
var goldenV2 = filepath.Join("testdata", "corpus-v2.golden")

// corpus generates observations from a fixed seed, so it is the same list on every run: every field drawn
// from realistic values and from hostile ones (notes that imitate a pod-status termination, control bytes,
// long text, secrets, the words podStatus and event).
func corpus() []Observation {
	r := rand.New(rand.NewPCG(789, 432)) // #nosec G404 -- a fixed seed, so the corpus is the same list on every run
	pick := func(s []string) string { return s[r.IntN(len(s))] }
	clusters := []string{"lab-1", "lab-weu-1", ""}
	namespaces := []string{"shop", "demo", "kube-system", ""}
	uids := []string{deploymentUID, podAUID, podBUID, ""}
	reasons := []string{"OOMKilled", "BackOff", "Unhealthy", "Terminated", "Error", "ImagePullBackOff",
		"ErrImagePull", "CreateContainerConfigError", "FailedScheduling", string(SourcePodStatus), string(SourceEvent), ""}
	actions := []string{"", "Restarting", "Pulling", string(SourcePodStatus)}
	types := []string{"Warning", "Normal", ""}
	controllers := []string{"kubelet", "default-scheduler", string(SourcePodStatus), string(SourceEvent), ""}
	notes := []string{
		"Container app terminated with exit code 137",
		"Container app terminated with exit code 1",
		"Container app is waiting: ImagePullBackOff",
		"Back-off 10s restarting failed container app in pod web-7d4b9c8f6-q8wzt",
		"Readiness probe failed: HTTP probe failed with statuscode: 503",
		`MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`,
		"password=hunter2-synthetic token=abc.def.ghi",
		"podStatus\x00event\x01",
		strings.Repeat("x", 2048),
		"",
	}
	kinds := []string{kindPod, "ReplicaSet", ""}
	names := []string{"web-7d4b9c8f6-q8wzt", "web-7d4b9c8f6", ""}
	out := make([]Observation, 0, 600)
	for range 600 {
		out = append(out, Observation{
			ClusterID: pick(clusters), Namespace: pick(namespaces), OwnerUID: pick(uids),
			ResourceUID: pick(uids), Reason: pick(reasons), Action: pick(actions), Type: pick(types),
			ReportingController: pick(controllers), Note: pick(notes), RegardingKind: pick(kinds),
			RegardingName: pick(names),
		})
	}
	return out
}

func readGolden(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(goldenV2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		out = append(out, s.Text())
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestCorpusGolden checks that an observation without a source, or with the source event, keeps the
// fingerprint the code computed before sources were hashed, byte for byte.
func TestCorpusGolden(t *testing.T) {
	obs := corpus()
	if os.Getenv("SHADEWRIGHT_UPDATE_GOLDEN") == "1" {
		var b strings.Builder
		for _, o := range obs {
			fmt.Fprintln(&b, Fingerprint(o))
		}
		if err := os.WriteFile(goldenV2, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := readGolden(t)
	if len(want) != len(obs) {
		t.Fatalf("golden has %d fingerprints, corpus %d", len(want), len(obs))
	}
	for i, o := range obs {
		if got := Fingerprint(o); got != want[i] {
			t.Errorf("observation %d without a source: %s, want %s", i, got, want[i])
		}
		o.Source = SourceEvent
		if got := Fingerprint(o); got != want[i] {
			t.Errorf("observation %d from an event: %s, want %s", i, got, want[i])
		}
	}
}

// TestCorpusPodStatusApart checks that a pod-status observation never shares a fingerprint with any
// observation of the corpus that is not one, whatever note, controller and reason the latter carries.
func TestCorpusPodStatusApart(t *testing.T) {
	obs := corpus()
	other := map[string]int{}
	for i, o := range obs {
		o.Source = SourceEvent
		other[Fingerprint(o)] = i
	}
	for i, o := range obs {
		o.Source = SourcePodStatus
		fp := Fingerprint(o)
		if j, ok := other[fp]; ok {
			t.Errorf("pod-status observation %d has the fingerprint of event observation %d", i, j)
		}
		if !crdPattern.MatchString(fp) {
			t.Errorf("observation %d: %q does not match the CRD pattern", i, fp)
		}
	}
}
