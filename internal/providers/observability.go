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

package providers

import (
	"context"
	"time"
)

// MetricsQuery runs one approved, named query template with parameters, never free-form query text.
type MetricsQuery struct {
	// Template is the name of an approved query template.
	Template string
	// Params fill the template; adapters validate them.
	Params map[string]string
	// Pods are the exact names of the target's pods, for a template that selects pods; adapters validate and
	// bound them, and a template that does not select pods refuses them. The caller resolves them from the
	// target's kind and UID through owner references (Deployment to ReplicaSet to Pod), never from a name
	// prefix or model output, so that only the target's own pods supply its evidence.
	Pods  []string
	Start time.Time
	End   time.Time
	// Step is the resolution of a range query; zero asks for an instant query at End.
	Step time.Duration
}

// Sample is one value at one time.
type Sample struct {
	Time  time.Time
	Value float64
}

// Series is one labelled time series.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

// MetricsResult is the answer to one query.
type MetricsResult struct {
	Series []Series
	// Partial means the source answered but warned that the answer may be incomplete, for example because a
	// store behind it did not respond. A series missing from a partial result may exist; a value in it may
	// lack samples or contributions. The warning text is not kept: it is untrusted and may quote the query.
	// A caller that draws a conclusion from absence or from a total treats a partial result as not knowing.
	Partial bool
}

// MetricsSource answers metric queries.
type MetricsSource interface {
	// Query runs an approved template. ErrNotFound when the template is unknown.
	Query(ctx context.Context, q MetricsQuery) (MetricsResult, error)
}

// DefaultLogBytes is the excerpt size an adapter and the fake use when a query sets no MaxBytes.
const DefaultLogBytes = 64 << 10

// LogQuery selects a bounded excerpt of one container's logs.
type LogQuery struct {
	Namespace string
	Pod       string
	Container string
	// Previous selects the logs of the previous, terminated container instance.
	Previous bool
	// Since limits how far back to read; zero means the adapter's default, which for the previous instance
	// may be no time limit.
	Since time.Duration
	// MaxBytes bounds the excerpt; zero means DefaultLogBytes.
	MaxBytes int64
}

// LogExcerpt is a bounded piece of logs. The text is untrusted. The Kubernetes adapter
// (internal/providers/logs/kube) redacts it before returning it; the fake does not.
type LogExcerpt struct {
	Text string
	// Truncated means older lines were cut from the front of the excerpt to fit MaxBytes.
	Truncated bool
	// EndMissing means the newest lines could not be read within the adapter's read bound, so the excerpt
	// ends before the end of the log.
	EndMissing bool
}

// LogSource reads logs.
type LogSource interface {
	// Logs returns the most recent lines within the query's bounds. ErrNotFound when the pod does not
	// exist; ErrRejected when the source refuses the request itself, for example for a container the pod
	// does not have, a previous instance that does not exist or a container that has not started (the
	// Kubernetes API server answers all three with 400).
	Logs(ctx context.Context, q LogQuery) (LogExcerpt, error)
}
