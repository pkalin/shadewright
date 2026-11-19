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
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// #878: a request with an open MergeCheck wait is deleted while the reads of its pull request fail, so the cleanup
// of the finalizer is retried (up to its bounds). A wait for a read does not apply to a request being deleted
// (waitKind.applies): the first reconcile of the deletion removes the entry, and the status no longer says the request
// waits for the merge commit while the cleanup is retried, before or past the wait's end, nor paces the cleanup's
// retry by the passed end of that entry.
func TestDeletionRemovesTheMergeWait(t *testing.T) {
	m := mergeCheckWithFailingReads(t, false)
	w := gitWait(m.live(), waitMergeCheck)
	ctx := context.Background()
	rr := m.live()
	if controllerutil.AddFinalizer(rr, FinalizerName) {
		if err := m.base.Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.base.Delete(ctx, rr); err != nil {
		t.Fatal(err)
	}
	// The write that removes the entry meets a conflicting write first: the reconcile returns the conflict for the
	// error backoff, and the next one reads the request again and removes it.
	m.conflictNext = true
	if _, _, err := m.once(); err == nil {
		t.Fatalf("setup: the conflicting write was not met")
	}
	if gitWait(m.live(), waitMergeCheck) == nil {
		t.Fatalf("setup: the entry was removed by a write that met a conflict")
	}
	for i, at := range []time.Time{m.clock.at, w.Ends.Add(time.Second), w.Ends.Add(10 * time.Minute)} {
		m.clock.at = at
		res, _, err := m.once()
		if err != nil {
			t.Fatalf("reconcile %d of the deletion: %v", i+1, err)
		}
		if res.RequeueAfter < time.Second {
			m.fatalf("reconcile %d of the deletion: the cleanup's retry runs again after %v, paced by a wait that "+
				"no longer applies", i+1, res.RequeueAfter)
		}
		live := m.live()
		if live.DeletionTimestamp.IsZero() {
			t.Fatalf("setup: the request is not being deleted")
		}
		retry := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry)
		switch {
		case gitWait(live, waitMergeCheck) != nil:
			m.fatalf("reconcile %d of the deletion: the MergeCheck wait stands while the cleanup is retried (waits %v)",
				i+1, live.Status.GitWaits)
		case retry != nil && retry.Reason == "GitReadFailedBeforeMergeWait":
			m.fatalf("reconcile %d of the deletion: GitReadRetry still says the request waits for the merge commit",
				i+1)
		}
	}
	m.report()
}
