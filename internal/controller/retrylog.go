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

package controller

import (
	"errors"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// controllerName is the name of the RemediationRequest controller, in its metrics and its log.
const controllerName = "remediationrequest"

// expectedRetry reports whether err is a retry the controller expects in normal operation (#902): a target that
// changed under the policy check, or a conflict on the write that moves a request to Verifying.
func expectedRetry(err error) bool {
	return errors.Is(err, errTargetChanged) || errors.Is(err, errVerifyingConflict)
}

// expectedRetryLogConstructor is the log of the RemediationRequest controller: the fields controller-runtime's
// builder gives it by default, on a sink that writes the error log of an expected retry at V(1). Reconcile still
// returns that error, so it keeps everything the error path gives (the rate limited backoff, both error counters
// and the delivery window and final phases of the steps), and only the "Reconciler error" line changes level.
func expectedRetryLogConstructor(base logr.Logger) func(*reconcile.Request) logr.Logger {
	gvk := remediationv1alpha1.GroupVersion.WithKind("RemediationRequest")
	log := base.WithValues("controller", controllerName, "controllerGroup", gvk.Group, "controllerKind", gvk.Kind)
	if sink := log.WithCallDepth(1).GetSink(); sink != nil {
		log = log.WithSink(expectedRetrySink{sink: sink})
	}
	return func(req *reconcile.Request) logr.Logger {
		if req == nil {
			return log
		}
		return log.WithValues("RemediationRequest", req.NamespacedName, "namespace", req.Namespace, "name", req.Name)
	}
}

// expectedRetrySink passes everything to sink, but for the error log of an expected retry, which it writes at V(1)
// with the error as a field. sink is one call deeper than the logger's, for the frame of this wrapper.
type expectedRetrySink struct {
	sink logr.LogSink
}

// Init does nothing: sink was initialized by the logger it came from.
func (s expectedRetrySink) Init(logr.RuntimeInfo) {}

// Enabled reports whether sink writes at level.
func (s expectedRetrySink) Enabled(level int) bool { return s.sink.Enabled(level) }

// Info passes the log to sink.
func (s expectedRetrySink) Info(level int, msg string, keysAndValues ...any) {
	s.sink.Info(level, msg, keysAndValues...)
}

// Error writes an expected retry at V(1) and anything else at ERROR.
func (s expectedRetrySink) Error(err error, msg string, keysAndValues ...any) {
	if err == nil || !expectedRetry(err) {
		s.sink.Error(err, msg, keysAndValues...)
		return
	}
	if s.sink.Enabled(1) {
		kv := make([]any, len(keysAndValues)+2)
		copy(kv, keysAndValues)
		kv[len(keysAndValues)] = "error"
		kv[len(keysAndValues)+1] = err.Error()
		s.sink.Info(1, msg, kv...)
	}
}

// WithValues keeps the wrapper on the sink with the values.
func (s expectedRetrySink) WithValues(keysAndValues ...any) logr.LogSink {
	return expectedRetrySink{sink: s.sink.WithValues(keysAndValues...)}
}

// WithName keeps the wrapper on the named sink.
func (s expectedRetrySink) WithName(name string) logr.LogSink {
	return expectedRetrySink{sink: s.sink.WithName(name)}
}

// WithCallDepth keeps the wrapper on the sink with its call depth adjusted.
func (s expectedRetrySink) WithCallDepth(depth int) logr.LogSink {
	if cd, ok := s.sink.(logr.CallDepthLogSink); ok {
		return expectedRetrySink{sink: cd.WithCallDepth(depth)}
	}
	return s
}
