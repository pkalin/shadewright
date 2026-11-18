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
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// statusClock is how a time field of a request's status is classified (fourth decision of 2026-11-18 on #771):
// every status clock that bounds a wait on the Git host is an entry of status.gitWaits (entry names its kind); a
// clock bounded by a check of its own stays off that list with the test that proves it ends (test); any other time
// field bounds no wait (why says what it is).
type statusClock struct {
	entry api.GitWaitKind
	test  string
	why   string
}

// statusClocks classifies every time field of RemediationRequestStatus, by its JSON path.
var statusClocks = map[string]statusClock{
	"status.deadline": {entry: api.GitWaitRequestDeadline,
		why: "the request deadline; its waits are the RequestDeadline and HandoffGrace entries, which record their end"},
	"status.mergeCommitAwaitedSince": {entry: api.GitWaitMergeCheck,
		why: "the start of the merge wait, kept as recorded before the entry existed; the MergeCheck entry bounds the wait"},
	"status.gitWaits.since": {entry: api.GitWaitPaced, why: "the start of an entry"},
	"status.gitWaits.ends":  {entry: api.GitWaitPaced, why: "the end of an entry"},

	"status.pauseSeen.pausedSince": {test: "TestSequenceMergeCheckEndsUnderAPauseThatComesAndGoes",
		why: "a pause a reconcile of the request saw; it bounds no wait, it stretches the MergeCheck and MergeContent " +
			"entries and the expired cleanup's bound, and the first reconcile that sees the lift clears it"},

	"status.pullRequest.conflictSeen.at": {test: "TestConflictWaitIsBoundedWhenTheClockGoesBack",
		why: "the wait to confirm a merge conflict, bounded by its own check"},
	"status.rollout.waitStartedAt": {test: "TestRolloutLateReconcileEndsWithAVerdict",
		why: "the delivery window, ended by the rollout's verdict"},
	"status.rollout.pausedSince": {test: "TestRolloutPausedDoesNotEndTheWindow",
		why: "a paused rollout, which moves the delivery window by the pause"},
	"status.pullRequest.appliedObservedAt": {test: "TestEveryGitReadRetrySiteKeepsItsBound",
		why: "the start of the delivery window (paceAt(windowEnd) of deliveryStep and waitForDelivery)"},
	"status.verification.windowStart": {test: "TestCountedStartsNoWindowBeforeReconciling",
		why: "the stable window of the verification, ended by its verdict"},

	"status.pullRequest.incidentStopped.readAt": {why: "the read that set the IncidentStopped mark; the wait before " +
		"it is held in memory and starts again after a restart"},
	"status.pullRequest.reviewCheck.refusedAt":           {why: "when the check run was refused; the refusal's backoff (TestRefusalBackoff)"},
	"status.verification.lastReset.time":                 {why: "a recorded reset of the stable window"},
	"status.verification.incidentCount.atMerge.readAt":   {why: "a reading of the Incident's count"},
	"status.verification.incidentCount.atApplied.readAt": {why: "a reading of the Incident's count"},
	"status.hints.updatedAt":                             {why: "a reviewer hint's time on the code host"},
	"status.hints.acceptedAt":                            {why: "when a reviewer hint was accepted"},
	"status.refusedHints.refusedAt":                      {why: "when a reviewer hint was refused"},
	"status.commentCursor.since":                         {why: "the cursor of the comment listing"},
	"status.commentCursor.sweepStart":                    {why: "the start of a sweep of the comment listing"},
	"status.commentCursor.seen.updatedAt":                {why: "a comment's time on the code host"},
	"status.commentCursor.evictedAt":                     {why: "when the cursor dropped old comments"},
	"status.conditions.lastTransitionTime":               {why: "a condition's transition"},
}

// statusTimePaths lists the JSON paths of every metav1.Time field of RemediationRequestStatus.
func statusTimePaths() []string {
	var paths []string
	var walk func(tp reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(tp reflect.Type, path string, seen map[reflect.Type]bool) {
		for tp.Kind() == reflect.Pointer || tp.Kind() == reflect.Slice {
			tp = tp.Elem()
		}
		if tp == reflect.TypeFor[metav1.Time]() {
			paths = append(paths, path)
			return
		}
		if tp.Kind() != reflect.Struct || seen[tp] {
			return
		}
		seen[tp] = true
		defer delete(seen, tp)
		for f := range tp.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			walk(f.Type, path+"."+name, seen)
		}
	}
	walk(reflect.TypeFor[api.RemediationRequestStatus](), "status", map[reflect.Type]bool{})
	return paths
}

// TestEveryStatusClockIsClassified fails on a time field of the status that is neither the clock of a status.gitWaits
// entry nor on the list of clocks bounded by a check of their own (with the test that proves they end) or of fields
// that bound no wait, and on a listed field that is gone or names a test that does not exist.
func TestEveryStatusClockIsClassified(t *testing.T) {
	tests := map[string]bool{}
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				tests[fn.Name.Name] = true
			}
		}
	}
	paths := statusTimePaths()
	for _, p := range paths {
		c, ok := statusClocks[p]
		switch {
		case !ok:
			t.Errorf("%s: a status clock that is not classified; a clock that bounds a wait on the Git host is an "+
				"entry of status.gitWaits, any other is listed with the test that proves it ends or why it bounds none", p)
		case c.why == "":
			t.Errorf("%s: classified without a reason", p)
		case c.test != "" && !tests[c.test]:
			t.Errorf("%s: names %s, which is not a test of this package", p, c.test)
		case c.entry != "" && !slices.Contains(slices.Collect(maps.Values(gitWaitKinds)), c.entry):
			t.Errorf("%s: names the entry kind %s, which is not one", p, c.entry)
		}
	}
	for p := range statusClocks {
		if !slices.Contains(paths, p) {
			t.Errorf("%s: listed, and no such time field is in the status", p)
		}
	}
}
