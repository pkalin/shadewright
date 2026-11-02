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
	Start  time.Time
	End    time.Time
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

// MetricsSource answers metric queries.
type MetricsSource interface {
	// Query runs an approved template. ErrNotFound when the template is unknown.
	Query(ctx context.Context, q MetricsQuery) ([]Series, error)
}

// LogQuery selects a bounded excerpt of one container's logs.
type LogQuery struct {
	Namespace string
	Pod       string
	Container string
	// Previous selects the logs of the previous, terminated container instance.
	Previous bool
	// Since limits how far back to read; zero means the adapter's default.
	Since time.Duration
	// MaxBytes bounds the excerpt; zero means the adapter's default.
	MaxBytes int64
}

// LogExcerpt is a bounded piece of logs. The text is untrusted and not yet redacted.
type LogExcerpt struct {
	Text      string
	Truncated bool
}

// LogSource reads logs.
type LogSource interface {
	// Logs returns the most recent lines within the query's bounds. ErrNotFound when the pod or container
	// does not exist.
	Logs(ctx context.Context, q LogQuery) (LogExcerpt, error)
}
