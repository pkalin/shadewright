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

package analysis

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Results of an analysis record write, the values of the label "result" of shadewright_analysis_records_total.
const (
	// recordWritten: the store created the record.
	recordWritten = "written"
	// recordExists: the store answered that the record's ID exists. The ID is the run's own (recordID), so
	// an earlier try of the same write stored it, and the ID is kept in the status.
	recordExists = "exists"
	// recordFailed: the write, or the live read of the claim before it, failed or was canceled.
	recordFailed = "failed"
	// recordSkipped: not tried, because no time was left on the claim or the claim was lost.
	recordSkipped = "skipped"
	// recordRefused: the record could not be built, or the store refused it for good before or when sending it
	// (data the redactor still changes, a bad ID, too large, rejected by Blob Storage).
	recordRefused = "refused"
)

// recordResults are every value of the label "result".
var recordResults = []string{recordWritten, recordExists, recordFailed, recordSkipped, recordRefused}

// analysisRecords counts the analysis record writes, by result, when a store is configured.
var analysisRecords = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analysis_records_total",
	Help: "Analysis record writes to the analysis store, by result (written, exists, failed, skipped, refused).",
}, []string{"result"})

func init() {
	metrics.Registry.MustRegister(analysisRecords)
	for _, r := range recordResults {
		analysisRecords.WithLabelValues(r)
	}
}
