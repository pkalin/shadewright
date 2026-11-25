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

package redactadapter

import (
	"github.com/prometheus/client_golang/prometheus"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

var withheldTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_redaction_withheld_total",
	Help: "Entries redaction withheld whole, by reason (work: the work budget ran out; size: longer than the bound; " +
		"heap: more pieces or strings than its bytes allow; wait: the wait for the heap limit of redactions at once ended).",
}, []string{"reason"})

var heapWaitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "shadewright_redaction_heap_waits_total",
	Help: "Redactions that waited for others to finish before running, so that the heap of the redactions running " +
		"at once stays within its limit.",
})

var hooks = redact.Hooks{
	Withheld: func(reason string) { withheldTotal.WithLabelValues(reason).Inc() },
	HeapWait: heapWaitsTotal.Inc,
}

func init() {
	for _, reason := range []string{redact.ReasonWork, redact.ReasonSize, redact.ReasonHeap, redact.ReasonWait} {
		withheldTotal.WithLabelValues(reason)
	}
	metrics.Registry.MustRegister(withheldTotal, heapWaitsTotal)
	redact.SetDefaultHooks(hooks, func(limitBytes int64) {
		logf.Log.WithName("redact").Info("no Go memory limit (GOMEMLIMIT) is set: the redactions running at once share a fixed heap limit",
			"limitBytes", limitBytes)
	})
}

// New creates a redactor with the built-in rules and the operator's decision counters.
func New() (*redact.Redactor, error) { return redact.New(hooks) }
