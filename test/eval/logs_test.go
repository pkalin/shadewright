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

package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/fake"
)

// sourceSynthetic is the source of a recording written by hand.
const sourceSynthetic = "synthetic"

// logsFile is the optional file of a scenario that records what its pod logs hold. A scenario without it has
// no log source, as an analyzer without one: the context says "no log source is configured" and get_pod_logs
// is not offered.
const logsFile = "logs.yaml"

// logsRecording is what the log source of a scenario answers: one excerpt per pod, container and instance. It
// is replayed through the providers.LogSource interface, the one the context builder and the get_pod_logs tool
// use, by [replayLogs]. The replay is for the harness only (#504); the analyzer reads logs through the
// Kubernetes adapter.
type logsRecording struct {
	// Source says how the lines were made: "synthetic" (written by hand) or the run that recorded them.
	Source string `json:"source"`
	// Namespace is the namespace of the pods; a query about any other finds no pod.
	Namespace string `json:"namespace"`
	// Excerpts are the logs of each container instance, each once.
	Excerpts []recordedLog `json:"excerpts"`
}

// recordedLog is the whole log of one container instance, oldest line first. Every line lies in the window of
// any query, so the replay bounds an answer by bytes only.
type recordedLog struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Previous is the terminated instance before the running one.
	Previous bool `json:"previous,omitempty"`
	// PodUID, ContainerID and RestartCount name the instance, as the source reads them from the pod status.
	PodUID       string `json:"podUID"`
	ContainerID  string `json:"containerID"`
	RestartCount int32  `json:"restartCount"`
	Text         string `json:"text"`
}

// check validates a recording: a source, the Incident's namespace, and each instance once with its lines.
func (r *logsRecording) check(namespace string) error {
	if r.Source == "" {
		return errors.New("logs.yaml: source is required")
	}
	if r.Namespace != namespace {
		return fmt.Errorf("logs.yaml: namespace %q is not the Incident subject's %q", r.Namespace, namespace)
	}
	if len(r.Excerpts) == 0 {
		return errors.New("logs.yaml: no excerpts")
	}
	for i, e := range r.Excerpts {
		if len(validation.IsDNS1123Subdomain(e.Pod)) > 0 || len(validation.IsDNS1123Label(e.Container)) > 0 {
			return fmt.Errorf("logs.yaml: excerpt %d: pod or container is not a valid name", i)
		}
		if e.Text == "" || !strings.HasSuffix(e.Text, "\n") {
			return fmt.Errorf("logs.yaml: excerpt %d: the text must be whole lines", i)
		}
		if slices.ContainsFunc(r.Excerpts[:i], func(o recordedLog) bool {
			return o.Pod == e.Pod && o.Container == e.Container && o.Previous == e.Previous
		}) {
			return fmt.Errorf("logs.yaml: excerpt %d: %s/%s (previous %v) is listed twice", i, e.Pod, e.Container,
				e.Previous)
		}
	}
	return nil
}

// loadLogs reads the scenario's logs.yaml, if it has one, and checks it against the Incident's namespace.
func (s *scenario) loadLogs(dir string) error {
	path := filepath.Join(dir, logsFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	rec := &logsRecording{}
	if err := readStrict(path, rec); err != nil {
		return err
	}
	if err := rec.check(s.incident.Spec.Subject.Namespace); err != nil {
		return err
	}
	s.logs = rec
	return nil
}

// replayLogs answers log queries from a recording as the Kubernetes API server does: a pod the recording does
// not name is not found, a container or instance it does not hold is a refused request, and an answer keeps
// the newest whole lines within MaxBytes (providers.DefaultLogBytes when zero), marked truncated when older
// lines were left out. The excerpt names the recorded instance, read at fixtureNow.
type replayLogs struct {
	rec *logsRecording
}

var _ providers.LogSource = replayLogs{}

func (l replayLogs) Logs(ctx context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	if err := ctx.Err(); err != nil {
		return providers.LogExcerpt{}, err
	}
	known := false
	for _, e := range l.rec.Excerpts {
		if q.Namespace != l.rec.Namespace || e.Pod != q.Pod {
			continue
		}
		known = true
		if e.Container != q.Container || e.Previous != q.Previous {
			continue
		}
		limit := q.MaxBytes
		if limit <= 0 {
			limit = providers.DefaultLogBytes
		}
		ex, err := fake.CutLog(e.Text, int(limit))
		if err != nil {
			return providers.LogExcerpt{}, err
		}
		ex.CollectedAt = fixtureNow
		ex.Instance = providers.LogInstance{PodUID: e.PodUID, ContainerID: e.ContainerID, RestartCount: e.RestartCount}
		return ex, nil
	}
	if known {
		return providers.LogExcerpt{}, fmt.Errorf("logs of %s/%s/%s: %w", q.Namespace, q.Pod, q.Container,
			providers.ErrRejected)
	}
	return providers.LogExcerpt{}, fmt.Errorf("logs of pod %s/%s: %w", q.Namespace, q.Pod, providers.ErrNotFound)
}

// newestLines keeps the newest whole lines of text within n bytes, and reports whether any were left out: the
// whole-line cut that the log source and the context builder made before #1055, kept to show what it dropped.
func newestLines(text string, n int) (string, bool) {
	if len(text) <= n {
		return text, false
	}
	start := len(text) - n
	if text[start-1] != '\n' {
		i := strings.IndexByte(text[start:], '\n')
		if i < 0 {
			return "", true
		}
		start += i + 1
	}
	return text[start:], true
}

// logSource is the log source of a run: the scenario's recording replayed, or none (a nil interface) when the
// scenario has no logs.yaml.
func (s *scenario) logSource() providers.LogSource {
	if s.logs == nil {
		return nil
	}
	return replayLogs{rec: s.logs}
}

// The replayed log source answers as the production log source cuts what the API server sent (internal/logcut):
// newest whole lines within the bound, or the tail of a longer last line, truncated when older ones are left
// out, a refused request for an instance the pod does not have, not found for another pod.
func TestReplayLogs(t *testing.T) {
	rec := &logsRecording{Source: sourceSynthetic, Namespace: nsDemo, Excerpts: []recordedLog{
		{Pod: "p", Container: "c", Previous: true, PodUID: "u", ContainerID: "id", RestartCount: 2,
			Text: "one\ntwo\nthree\n"},
	}}
	if err := rec.check(nsDemo); err != nil {
		t.Fatal(err)
	}
	l := replayLogs{rec: rec}
	ex, err := l.Logs(t.Context(), providers.LogQuery{Namespace: nsDemo, Pod: "p", Container: "c", Previous: true,
		MaxBytes: 11})
	if err != nil || ex.Text != "two\nthree\n" || !ex.Truncated || ex.Instance.RestartCount != 2 ||
		!ex.CollectedAt.Equal(fixtureNow) {
		t.Errorf("got %+v, %v", ex, err)
	}
	ex, err = l.Logs(t.Context(), providers.LogQuery{Namespace: nsDemo, Pod: "p", Container: "c", Previous: true})
	if err != nil || ex.Text != rec.Excerpts[0].Text || ex.Truncated {
		t.Errorf("unbounded: got %+v, %v", ex, err)
	}
	if _, err := l.Logs(t.Context(), providers.LogQuery{Namespace: nsDemo, Pod: "p", Container: "c"}); !errors.Is(err,
		providers.ErrRejected) {
		t.Errorf("current instance: got %v, want rejected", err)
	}
	for _, q := range []providers.LogQuery{{Namespace: nsDemo, Pod: "q", Container: "c", Previous: true},
		{Namespace: "other", Pod: "p", Container: "c", Previous: true}} {
		if _, err := l.Logs(t.Context(), q); !errors.Is(err, providers.ErrNotFound) {
			t.Errorf("%+v: got %v, want not found", q, err)
		}
	}
}

// A recording is refused without a source, in another namespace, with an instance listed twice, or with text
// that is not whole lines.
func TestLogsRecordingRefused(t *testing.T) {
	good := func() *logsRecording {
		return &logsRecording{Source: sourceSynthetic, Namespace: nsDemo, Excerpts: []recordedLog{
			{Pod: "p", Container: "c", Text: "a\n"}}}
	}
	if err := good().check(nsDemo); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, want string
		change     func(*logsRecording)
	}{
		{"no source", "source is required", func(r *logsRecording) { r.Source = "" }},
		{"other namespace", "is not the Incident subject's", func(r *logsRecording) { r.Namespace = "other" }},
		{"no excerpts", "no excerpts", func(r *logsRecording) { r.Excerpts = nil }},
		{"an instance listed twice", "is listed twice", func(r *logsRecording) {
			r.Excerpts = append(r.Excerpts, r.Excerpts[0])
		}},
		{"partial line", "whole lines", func(r *logsRecording) { r.Excerpts[0].Text = "a" }},
		{"bad name", "not a valid name", func(r *logsRecording) { r.Excerpts[0].Container = "C/1" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := good()
			c.change(r)
			if err := r.check(nsDemo); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

// downMetrics is a configured metrics source that fails every query, as a Prometheus that is down: the
// builder marks the metrics section failed (an escalation signal, design 3.3) and query_metrics answers
// "could not fetch".
type downMetrics struct{}

var _ providers.MetricsSource = downMetrics{}

func (downMetrics) Query(ctx context.Context, _ providers.MetricsQuery) (providers.MetricsResult, error) {
	if err := ctx.Err(); err != nil {
		return providers.MetricsResult{}, err
	}
	return providers.MetricsResult{}, errors.New("prometheus: connection refused (evaluation: the source is down)")
}
