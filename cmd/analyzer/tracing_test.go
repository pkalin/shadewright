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
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// otelVars are the variables newTracerProvider reads; each case starts with all of them empty.
var otelVars = []string{envSDKDisabled, envTracesExporter, envProtocol, envTracesProtocol, envEndpoint,
	envTracesEndpoint}

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
		wantErr bool
	}{
		{name: "off by default"},
		{name: "none", env: map[string]string{envTracesExporter: "none"}},
		{name: "disabled wins", env: otlp(envSDKDisabled, "TRUE", envEndpoint, endpoint)},
		{name: "an endpoint alone stays off", env: map[string]string{envEndpoint: endpoint}},
		{name: "otlp", env: otlp(envEndpoint, endpoint), on: true},
		{name: "otlp grpc traces endpoint", env: otlp(envTracesProtocol, "grpc", envTracesEndpoint, endpoint), on: true},
		{name: "otlp without an endpoint", env: otlp(), wantErr: true},
		{name: "another exporter", env: map[string]string{envTracesExporter: "console", envEndpoint: endpoint},
			wantErr: true},
		{name: "http protocol", env: otlp(envProtocol, "http/protobuf", envEndpoint, endpoint), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range otelVars {
				t.Setenv(k, tc.env[k])
			}
			tp, err := newTracerProvider(logr.Discard())
			if (err != nil) != tc.wantErr || (tp != nil) != tc.on {
				t.Fatalf("provider %v, err %v: want on %v, error %v", tp != nil, err, tc.on, tc.wantErr)
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
