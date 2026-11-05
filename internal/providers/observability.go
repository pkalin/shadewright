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
	// bound them, and a template that does not select pods refuses them. They are pods in the namespace the
	// query names: the caller resolves them in that same namespace from the target's kind and UID through
	// owner references (Deployment to ReplicaSet to Pod, StatefulSet to Pod), never from a name prefix or model
	// output, so that only the target's own pods supply its evidence. An adapter cannot check that the pods
	// belong to the namespace; it matches namespace and pod together, so a pod of the same name in another
	// namespace is never selected.
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
	// Previous selects the logs of the previous, terminated container instance. It is relative: it names the
	// instance before the one running when the source reads, so a restart during a query moves it.
	Previous bool
	// Since limits how far back to read; zero means the adapter's default, which for the previous instance
	// may be no time limit.
	Since time.Duration
	// MaxBytes bounds the excerpt; zero means DefaultLogBytes.
	MaxBytes int64
}

// MaxLogInstanceIDLength bounds the PodUID and ContainerID of a [LogInstance]; a source records a longer
// value, or one with other characters than letters, digits and ._:/+-, as empty.
const MaxLogInstanceIDLength = 128

// LogInstance names the pod and container instance an excerpt was read from, as the pod's status gave them
// when the source checked it before reading. The values come from the cluster: they identify the evidence and
// are never instructions. They belong with the excerpt only, never in Incident status.
type LogInstance struct {
	// PodUID is the UID of the pod; a pod recreated under the same name has another.
	PodUID string
	// ContainerID is the runtime's ID of the instance read: the running (or last started) container, or for
	// a Previous query the terminated one before it. Empty when the status named none.
	ContainerID string
	// RestartCount is the container's restart count at the check. A Previous query read the instance before
	// the one this count belongs to.
	RestartCount int32
}

// LogExcerpt is a bounded piece of logs. The text is untrusted. The Kubernetes adapter
// (internal/providers/logs/kube) redacts it before returning it; the fake does not.
//
// A log request names a pod and a container by name only: it cannot ask for a pod UID or a container ID, so
// the instance that answered is inferred from the pod's status read before and after. Instance and
// InstanceUncertain say how far that holds. An uncertain excerpt is a label on the evidence, not a reason to
// escalate (design section 3.3): pod restarts and recreation are under the control of whoever runs workloads
// in a watched namespace.
type LogExcerpt struct {
	Text string
	// Truncated means older lines were cut from the front of the excerpt to fit MaxBytes.
	Truncated bool
	// EndMissing means the newest lines could not be read within the adapter's read bound, so the excerpt
	// ends before the end of the log.
	EndMissing bool
	// Instance is the pod and container instance the excerpt is tied to (the check before reading).
	Instance LogInstance
	// CollectedAt is when the source finished reading the logs.
	CollectedAt time.Time
	// InstanceUncertain means the source cannot tie the text to Instance: the pod's UID, the container's ID
	// or its restart count differed between the checks before and after reading, the pod was gone after
	// reading, or the status named no instance or an ID out of bounds. The text may then hold lines of
	// another instance, or of two.
	InstanceUncertain bool
}

// LogSource reads logs.
type LogSource interface {
	// Logs returns the most recent lines within the query's bounds. ErrNotFound when the pod does not
	// exist; ErrRejected when the source refuses the request itself, for example for a container the pod
	// does not have, a previous instance that does not exist or a container that has not started (the
	// Kubernetes API server answers all three with 400).
	Logs(ctx context.Context, q LogQuery) (LogExcerpt, error)
}
