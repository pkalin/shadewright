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

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

// clearOtelEnv empties every OTEL_* variable of the environment for the test, so the developer's or CI's
// own settings cannot change a case.
func clearOtelEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "OTEL_") {
			t.Setenv(k, "")
		}
	}
}

func TestTracingConfiguration(t *testing.T) {
	// Nothing listens there: the exporter must not wait for it, at the start or at the flush.
	const endpoint = "http://127.0.0.1:1"
	otlp := func(kv ...string) map[string]string {
		env := map[string]string{envTracesExporter: exporterOTLP}
		for i := 0; i+1 < len(kv); i += 2 {
			env[kv[i]] = kv[i+1]
		}
		return env
	}
	cases := []struct {
		name    string
		env     map[string]string
		on      bool
		wantErr string
	}{
		{name: "off by default"},
		{name: "none", env: map[string]string{envTracesExporter: "none"}},
		{name: "disabled wins", env: otlp(envSDKDisabled, "TRUE", envEndpoint, endpoint)},
		{name: "an endpoint alone stays off", env: map[string]string{envEndpoint: endpoint}},
		{name: "otlp", env: otlp(envEndpoint, endpoint), on: true},
		{name: "otlp grpc traces endpoint", env: otlp(envTracesProtocol, "grpc", envTracesEndpoint, endpoint), on: true},
		{name: "otlp without an endpoint", env: otlp(), wantErr: envTracesEndpoint},
		{name: "another exporter", env: map[string]string{envTracesExporter: "console", envEndpoint: endpoint},
			wantErr: "not supported"},
		{name: "malformed resource", env: otlp(envEndpoint, endpoint, "OTEL_RESOURCE_ATTRIBUTES", "no-equals-sign"),
			wantErr: "OTEL_RESOURCE_ATTRIBUTES"},
		{name: "http protocol", env: otlp(envProtocol, "http/protobuf", envEndpoint, endpoint), wantErr: "not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOtelEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			tp, err := newTracerProvider(logr.Discard())
			if (tp != nil) != tc.on || (err != nil) != (tc.wantErr != "") ||
				(err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("provider %v, err %v: want on %v, an error naming %q", tp != nil, err, tc.on, tc.wantErr)
			}
			if tp == nil {
				return
			}
			_, span := tp.Tracer("test").Start(context.Background(), "span")
			span.End()
			start := time.Now()
			shutdownTracing(tp, logr.Discard())
			if d := time.Since(start); d > tracingShutdownTimeout+time.Second {
				t.Errorf("shutdown took %s, want at most about %s", d, tracingShutdownTimeout)
			}
		})
	}
}

// The OpenTelemetry error log writes the first error, then at most one line per interval with the count of
// the errors it did not write.
func TestOtelErrorLogIsRateLimited(t *testing.T) {
	var lines []string
	log := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	now := time.Unix(0, 0)
	h := newOtelErrorLog(log, func() time.Time { return now })
	for range 5 {
		h.Handle(errors.New("export failed"))
		now = now.Add(10 * time.Second)
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d lines in 50s, want 1: %q", len(lines), lines)
	}
	now = now.Add(otelLogInterval)
	h.Handle(errors.New("export failed"))
	if len(lines) != 2 || !strings.Contains(lines[1], `"notLoggedSinceLastLine"=4`) {
		t.Errorf("second line %q, want one naming the 4 errors not logged", lines)
	}
}
