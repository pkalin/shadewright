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
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// serviceName is the service.name of the analyzer's traces unless OTEL_SERVICE_NAME or
// OTEL_RESOURCE_ATTRIBUTES name another.
const serviceName = "shadewright-analyzer"

// tracingShutdownTimeout bounds the flush of the spans still queued when the analyzer stops; whatever is not
// sent by then is dropped.
const tracingShutdownTimeout = 3 * time.Second

// The OpenTelemetry environment variables newTracerProvider reads itself, and the one exporter it supports.
const (
	envSDKDisabled    = "OTEL_SDK_DISABLED"
	envTracesExporter = "OTEL_TRACES_EXPORTER"
	envProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	envEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	exporterOTLP      = "otlp"
)

// newTracerProvider returns the tracer provider of the analysis traces (#90), or nil when tracing is off. It is
// off by default: on only with OTEL_TRACES_EXPORTER=otlp (unlike the OpenTelemetry default, an unset variable
// means off) and an OTLP endpoint (OTEL_EXPORTER_OTLP_TRACES_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT), and
// never with OTEL_SDK_DISABLED=true. The exporter is OTLP over gRPC (OTEL_EXPORTER_OTLP_PROTOCOL grpc or unset);
// the exporter reads its other standard variables itself (insecure, headers, certificate, timeout, compression),
// the batch processor OTEL_BSP_* and the sampler OTEL_TRACES_SAMPLER.
//
// The exporter never blocks or fails an analysis: spans go to a bounded queue that drops new spans when it is
// full, a background goroutine exports them with a timeout, and an export error is only logged. An unknown
// exporter or protocol, or a missing endpoint, is an error, so a misconfiguration stops the start instead of
// passing silently.
func newTracerProvider(log logr.Logger) (*sdktrace.TracerProvider, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(envSDKDisabled)), "true") {
		return nil, nil
	}
	switch exp := strings.TrimSpace(os.Getenv(envTracesExporter)); exp {
	case "", "none":
		return nil, nil
	case exporterOTLP:
	default:
		return nil, fmt.Errorf("OTEL_TRACES_EXPORTER %q is not supported: use otlp, or none (the default) for no traces", exp)
	}
	protocol := strings.TrimSpace(os.Getenv(envTracesProtocol))
	if protocol == "" {
		protocol = strings.TrimSpace(os.Getenv(envProtocol))
	}
	if protocol != "" && protocol != "grpc" {
		return nil, fmt.Errorf("OTLP protocol %q is not supported: the analyzer exports traces over grpc only", protocol)
	}
	if strings.TrimSpace(os.Getenv(envTracesEndpoint)) == "" &&
		strings.TrimSpace(os.Getenv(envEndpoint)) == "" {
		return nil, errors.New("OTEL_TRACES_EXPORTER=otlp needs OTEL_EXPORTER_OTLP_TRACES_ENDPOINT or " +
			"OTEL_EXPORTER_OTLP_ENDPOINT, such as http://otel-collector.observability.svc:4317")
	}
	// Neither call waits for anything: the gRPC connection is made lazily, and the resource reads the
	// environment only.
	ctx := context.Background()
	// Later options win: OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the default name.
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithTelemetrySDK(), resource.WithFromEnv())
	if err != nil {
		return nil, fmt.Errorf("the trace resource: %w", err)
	}
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("the OTLP trace exporter: %w", err)
	}
	// An export failure is reported to the global error handler: log it, never fail anything for it.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Error(err, "Failed to export analysis traces")
	}))
	return sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res)), nil
}

// shutdownTracing flushes the queued spans and stops the exporter within tracingShutdownTimeout, on a context
// of its own: the manager's is already done when the analyzer stops.
func shutdownTracing(tp *sdktrace.TracerProvider, log logr.Logger) {
	if tp == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
	defer cancel()
	if err := tp.Shutdown(ctx); err != nil {
		log.Error(err, "Failed to flush the analysis traces on shutdown")
	}
}
