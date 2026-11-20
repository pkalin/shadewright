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
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// The replacement takes the pull request over; a person puts the predecessor's head back and closes the pull request;
// the predecessor stores its stop and the abort of its intent and crashes before it settles its lock; the pull request
// is reopened and merged at that head, and the replacement stores that it waits for the predecessor's record
// (MergeRecordPending). The cache then serves the replacement as it was before that write: its next reconcile must
// still write nothing to Git, not even the rewrite of the pull request after the takeover (P4). The reconcile starts
// from the API server's copy, and the gate before the write reads both requests there.
func TestHandoffModelCachedCopyBeforeTheRecordWaitWritesNothing(t *testing.T) {
	hmDirected(t, hmStartIntent, []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop}, {Kind: hClose},
		{Kind: hCrash, N: 1}, stRecPred, {Kind: hReopen}, stMerge, stRecRepl, {Kind: hStale, Obj: hmRepl}, stRecRepl,
		stRecRepl})
}

// After a refused handoff the replacement records the merge of its commit without a receipt (the host reports no
// merge commit ID), and the predecessor ends Superseded with MergedForReplacement; the replacement is deleted. The
// cache serves the predecessor as it was before that end, and its reconcile meets a failed read of a file: no wait
// for the merge may be added to the predecessor, which is settled, and it never records the same merge as a foreign
// change (P1).
func TestHandoffModelCachedCopyAddsNoWaitToASettledPredecessor(t *testing.T) {
	once := func(code int, op string) hmStep { return hmStep{Kind: hFault, Code: code, Op: op} }
	hmDirected(t, hmStartPushed, []hmStep{once(403, opBranchHead), stRecRepl, stAnswer, {Kind: hMerge, NoID: true},
		stRecRepl, {Kind: hClock, Clock: clockSmall, Mins: 6}, stRecRepl, stRecRepl, {Kind: hDelete, Obj: hmRepl},
		stRecRepl, {Kind: hStale, Obj: hmPred}, once(503, opReadFile), stRecPred, {Kind: hClock, Clock: clockSmall,
			Mins: 6}, stRecPred, stRecPred})
}

// requestReads counts the reads of RemediationRequests a reader serves.
type requestReads struct {
	client.Reader
	n *int
}

func (c requestReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*api.RemediationRequest); ok {
		*c.n++
	}
	return c.Reader.Get(ctx, key, obj, opts...)
}

// requestReadsClient is requestReads for the reconciler's client (the cache).
type requestReadsClient struct {
	client.Client
	n *int
}

func (c requestReadsClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*api.RemediationRequest); ok {
		*c.n++
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// The cost of deciding on live copies (decision of 2026-11-19 on #786): the reads of requests from the API server and
// from the cache in the reconciles of a handoff, from the reservation to the rewrite of the pull request after the
// takeover, and the Git writes they make. The numbers are logged (go test -v) and bounded: one read of the request at
// the start of each reconcile, and the gate's reads (the request and the other request of the handoff) at each write.
func TestHandoffReadsPerReconcileAndPerGitWrite(t *testing.T) {
	m := newHMWorld(t, requestStatusSchema(t), hmStartIntent)
	var live, cached int
	m.r.APIReader = requestReads{Reader: m.r.APIReader, n: &live}
	m.r.Client = requestReadsClient{Client: m.r.Client, n: &cached}
	writes := 0
	for range 3 {
		before := len(m.trace)
		m.reconcile(hmRepl)
		for _, l := range m.trace[before:] {
			if strings.Contains(l, "git write") {
				writes++
			}
		}
	}
	t.Logf("3 reconciles of the replacement: %d reads of requests from the API server, %d from the cache, %d Git writes",
		live, cached, writes)
	if len(m.violations) > 0 {
		t.Fatalf("violations: %v", m.violations)
	}
	// Measured on 2026-11-19: before, 11 reads from the API server and 5 from the cache; after, 23 and 2. The fake host
	// asks the gate twice per write (the gate and the adapter's checkpoint), and each check reads the request and the
	// predecessor: 3 starts + 4 writes x 2 checks x 2 reads + 4 other reads. A change here is a change of the cost.
	if live != 23 || cached != 2 || writes != 4 {
		t.Fatalf("reads %d from the API server, %d from the cache, %d writes; want 23, 2, 4", live, cached, writes)
	}
}
