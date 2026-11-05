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

package prometheus

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

const (
	testCluster = "lab-1"
	// queryPath is the instant query path under the test server's base URL.
	queryPath  = "/prom/api/v1/query"
	emptyProbe = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	// oneSample is the probe's answer when the series exist: one sample at now (1793793600), without labels.
	oneSample = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1793793600,"1"]}]}}`
)

// The check sends the one fixed probe, scoped to the configured cluster, as an instant query.
func TestCheckClusterSendsTheFixedProbe(t *testing.T) {
	var query string
	s, _, n := testSourceWith(t, Config{Namespaces: []string{testNS}, Cluster: testCluster},
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != queryPath {
				t.Errorf("path %s", r.URL.Path)
			}
			query = r.URL.Query().Get("query")
			writeJSON(w, oneSample)
		})
	if err := s.CheckCluster(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := `group(kube_pod_container_status_restarts_total{cluster="` + testCluster + `"}) and ` +
		`group(container_memory_working_set_bytes{cluster="` + testCluster + `"})`
	if query != want || n.Load() != 1 {
		t.Errorf("query %q (%d requests), want %q once", query, n.Load(), want)
	}
}

// Acceptance (#371): a configured cluster that no series carries is told apart from an answer.
func TestCheckClusterNotFound(t *testing.T) {
	s, _, _ := testSourceWith(t, Config{Cluster: "lab-typo"}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, emptyProbe)
	})
	if err := s.CheckCluster(context.Background()); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("got %v, want ErrClusterNotFound", err)
	}
}

// An empty answer with a warning is not knowing, and a failing source is its error: neither is "not found",
// which would switch metrics off.
func TestCheckClusterDoesNotGuess(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"partial": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"status":"success","data":{"resultType":"vector","result":[]},"warnings":["store down"]}`)
		},
		"unavailable": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
	} {
		s, _, _ := testSourceWith(t, Config{Cluster: testCluster}, h)
		err := s.CheckCluster(context.Background())
		var retry *providers.RetryableError
		if errors.Is(err, ErrClusterNotFound) || !errors.As(err, &retry) {
			t.Errorf("%s: got %v, want a retryable error", name, err)
		}
	}
}

// Without a cluster setting there is nothing to check, and nothing is sent.
func TestCheckClusterWithoutClusterSendsNothing(t *testing.T) {
	s, _, n := testSourceWith(t, Config{}, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, emptyProbe) })
	if err := s.CheckCluster(context.Background()); err != nil || n.Load() != 0 {
		t.Fatalf("got %v after %d requests", err, n.Load())
	}
}

// A probe answer with a label is not the answer the probe asks for.
func TestCheckClusterRefusesLabels(t *testing.T) {
	s, _, _ := testSourceWith(t, Config{Cluster: testCluster}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"x"},"value":[1793793600,"1"]}]}}`)
	})
	if err := s.CheckCluster(context.Background()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("got %v, want ErrInvalidResponse", err)
	}
}
