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

package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// The supersede handoff of ADR-0028 section 4: a branch that carries a chain of commits, the commit appended
// to it, the rewrite of the pull request and the check run on its new head.

const (
	opAppendCommit = "GitHub append commit"
	opUpdateCR     = "GitHub update pull request"
	opCheckRun     = "GitHub create check run"
	opReadCheckRun = "GitHub read check run"
)

// checkRunsPerPage and maxCheckRunPages bound the read of the check runs of one name on one commit. The executor
// creates one run per change it makes on the host, so a head carries a handful; more than it reads is ErrTooLarge.
const (
	checkRunsPerPage = 100
	maxCheckRunPages = 3
	checkRunsLimit   = 1 << 20
	// checkRunCompleted is GitHub's status of a completed check run.
	checkRunCompleted = "completed"
)

// CheckChain reports what commit carries over the target branch when the branch may carry up to commits
// commits of its own (providers.GitProvider.CheckChain). One commit is CheckBranch. Longer chains are walked
// from commit down, one parent at a time: each commit must have one parent and differ from it only in the
// file at path (the trees compared as CheckBranch compares them), until a parent is on the target branch.
func (p *Provider) CheckChain(ctx context.Context, commit, path string, commits int) (providers.BranchState, error) {
	switch {
	case commits < 1 || commits > providers.MaxChainCommits:
		return 0, errors.New(opCheckBranch + ": chain length is out of range")
	case commits == 1:
		return p.CheckBranch(ctx, commit, path)
	case !validSHA(commit):
		return 0, errors.New(opCheckBranch + ": commit is not a full SHA")
	case !validPath(path):
		return 0, errors.New(opCheckBranch + ": path is invalid")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	switch on, err := p.isOnTarget(ctx, opCheckBranch, commit); {
	case err != nil:
		return 0, err
	case on:
		return providers.BranchAtTarget, nil
	}
	segs := strings.Split(path, "/")
	at := commit
	for range commits {
		parents, err := p.parents(ctx, at)
		if err != nil {
			return 0, err
		}
		if len(parents) != 1 {
			return providers.BranchForeign, nil
		}
		parent := parents[0]
		switch state, err := p.changedOnly(ctx, at, parent, segs); {
		case err != nil:
			return 0, err
		case state != providers.BranchOneChange:
			return state, nil
		}
		switch on, err := p.isOnTarget(ctx, opCheckBranch, parent); {
		case err != nil:
			return 0, err
		case on:
			return providers.BranchOneChange, nil
		}
		at = parent
	}
	return providers.BranchForeign, nil
}

// Parents returns the parents of commit, each a full SHA.
func (p *Provider) Parents(ctx context.Context, commit string) ([]string, error) {
	if !validSHA(commit) {
		return nil, errors.New(opCheckBranch + ": commit is not a full SHA")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	return p.parents(ctx, commit)
}

// AppendCommit commits one change to an existing regular file on a shadewright/ branch whose head must be
// expectedHead, a commit that is not on the target branch, and returns the new commit
// (providers.GitProvider.AppendCommit). The branch moves with the same compare-and-swap as CommitFile.
func (p *Provider) AppendCommit(
	ctx context.Context, branch, expectedHead string, change providers.FileChange,
) (string, error) {
	switch {
	case !isHeadBranch(branch):
		return "", errors.New(opAppendCommit + ": branch is not a Shadewright branch")
	case !validSHA(expectedHead):
		return "", errors.New(opAppendCommit + ": expected head is not a full SHA")
	case !validPath(change.Path):
		return "", errors.New(opAppendCommit + ": path is invalid")
	case len(change.Content) > providers.MaxFileBytes:
		return "", fmt.Errorf("%s: %w", opAppendCommit, providers.ErrTooLarge)
	case strings.TrimSpace(change.Message) == "" || len(change.Message) > maxMessageBytes ||
		!utf8.ValidString(change.Message):
		return "", errors.New(opAppendCommit + ": commit message is empty, too long or not UTF-8")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	head, err := p.ref(ctx, opAppendCommit, branch)
	if err != nil {
		return "", err
	}
	if head != expectedHead {
		return "", fmt.Errorf("%s: branch head moved: %w", opAppendCommit, providers.ErrConflict)
	}
	// A head on the target branch carries nothing of its own: the first commit is CommitFile's.
	switch on, err := p.isOnTarget(ctx, opAppendCommit, expectedHead); {
	case err != nil:
		return "", err
	case on:
		return "", errors.New(opAppendCommit + ": the branch carries no commit of its own")
	}
	return p.commitOnto(ctx, branch, expectedHead, change)
}

// UpdateChangeRequest replaces the title and body of an open pull request from a shadewright/ branch to the
// target branch (providers.GitProvider.UpdateChangeRequest). The same title and body are not sent again. Input
// GitHub would refuse (an empty or too long title, a body over its limit, text that is not UTF-8) is refused
// with providers.ErrRejected before any write, so the caller records it as final instead of asking again on
// every look. The pull request is read first: one closed or merged meanwhile is ErrConflict whatever the input.
func (p *Provider) UpdateChangeRequest(ctx context.Context, number int64, title, body string) error {
	if number < 1 {
		return fmt.Errorf("%s: number must be positive: %w", opUpdateCR, providers.ErrRejected)
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	pr, err := p.pullFor(ctx, opUpdateCR, number)
	if err != nil {
		return err
	}
	open := func(pr pullJSON) error {
		if pr.MergedAt != nil || pr.State != stateOpen {
			return fmt.Errorf("%s: pull request is not open: %w", opUpdateCR, providers.ErrConflict)
		}
		return nil
	}
	if err := open(pr); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(title) == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > maxTitleRunes:
		return fmt.Errorf("%s: title is empty, too long or not UTF-8: %w", opUpdateCR, providers.ErrRejected)
	case !utf8.ValidString(body) || utf8.RuneCountInString(body) > maxBodyRunes:
		return fmt.Errorf("%s: body is too long or not UTF-8: %w", opUpdateCR, providers.ErrRejected)
	}
	if pr.Title == title && pr.Body != nil && *pr.Body == body {
		return nil
	}
	r, err := p.do(ctx, call{
		op: opUpdateCR, method: http.MethodPatch, path: "/pulls/" + strconv.FormatInt(number, 10),
		body: map[string]string{"title": title, "body": body},
	})
	if err != nil {
		return err
	}
	switch r.status {
	case http.StatusOK:
		var updated pullJSON
		if err := decode(opUpdateCR, r, &updated); err != nil {
			return err
		}
		if updated.Number != number {
			return errors.New(opUpdateCR + ": GitHub returned an unexpected pull request")
		}
		return nil
	case http.StatusUnprocessableEntity:
		// It may have been merged or closed in the meantime: read it again.
		if pr, err = p.pullFor(ctx, opUpdateCR, number); err != nil {
			return err
		}
		if err := open(pr); err != nil {
			return err
		}
	}
	return statusError(opUpdateCR, r.status)
}

// checkRunStatus maps a check run status to GitHub's status; checkRunConclusion gives the conclusion of a
// completed one.
var (
	checkRunStatus = map[providers.CheckRunStatus]string{
		providers.CheckRunInProgress: "in_progress", providers.CheckRunSuccess: checkRunCompleted,
		providers.CheckRunFailure: checkRunCompleted}
	checkRunConclusion = map[providers.CheckRunStatus]string{providers.CheckRunSuccess: "success",
		providers.CheckRunFailure: "failure"}
)

// CreateCheckRun creates a check run on head (providers.GitProvider.CreateCheckRun). It needs the App
// permission checks: write.
func (p *Provider) CreateCheckRun(ctx context.Context, head string, run providers.CheckRun) error {
	status, ok := checkRunStatus[run.Status]
	switch {
	case !validSHA(head):
		return errors.New(opCheckRun + ": commit is not a full SHA")
	case run.Name != providers.CheckRunName:
		return errors.New(opCheckRun + ": not the Shadewright check")
	case !ok:
		return errors.New(opCheckRun + ": status is not supported")
	case !checkRunText(run.Title) || !checkRunText(run.Summary):
		return errors.New(opCheckRun + ": title or summary is empty, too long or not UTF-8")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	body := map[string]any{
		"name": run.Name, "head_sha": head, "status": status,
		"output": map[string]string{"title": run.Title, "summary": run.Summary},
	}
	if c, ok := checkRunConclusion[run.Status]; ok {
		body["conclusion"] = c
	}
	r, err := p.do(ctx, call{op: opCheckRun, method: http.MethodPost, path: "/check-runs", body: body})
	if err != nil {
		return err
	}
	if r.status != http.StatusCreated {
		return statusError(opCheckRun, r.status)
	}
	return nil
}

// checkRunJSON is the part of a GitHub check run that ReviewCheckRun reads.
type checkRunJSON struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        *struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

// ReviewCheckRun returns the status of the latest check run shadewright/review on head that the provider's own
// App created (providers.GitProvider.ReviewCheckRun). It asks GitHub for the runs of that name and App on the
// commit (filter all) and checks each one it gets: a run of another name, commit or App is never counted, so a
// run another App posts under the same name cannot stand in for the App's own. The latest is the one with the
// highest ID, as GitHub gives each new run a higher one. It needs the App permission checks: read, which the
// checks: write of every token request includes.
func (p *Provider) ReviewCheckRun(ctx context.Context, head string) (providers.CheckRunStatus, bool, error) {
	if !validSHA(head) {
		return "", false, errors.New(opReadCheckRun + ": commit is not a full SHA")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	app, err := p.ownApp(ctx, opReadCheckRun)
	if err != nil {
		return "", false, err
	}
	var latest *checkRunJSON
	read := 0
	for page := 1; ; page++ {
		if page > maxCheckRunPages {
			return "", false, fmt.Errorf("%s: more check runs than are read: %w", opReadCheckRun, providers.ErrTooLarge)
		}
		q := url.Values{"check_name": {providers.CheckRunName}, "app_id": {strconv.FormatInt(app, 10)},
			"filter": {"all"}, paramPerPage: {strconv.Itoa(checkRunsPerPage)}, paramPage: {strconv.Itoa(page)}}
		r, err := p.do(ctx, call{op: opReadCheckRun, method: http.MethodGet, path: "/commits/" + head + "/check-runs",
			query: q, limit: checkRunsLimit})
		if err != nil {
			return "", false, err
		}
		if !r.ok() {
			return "", false, statusError(opReadCheckRun, r.status)
		}
		var body struct {
			TotalCount *int           `json:"total_count"`
			CheckRuns  []checkRunJSON `json:"check_runs"`
		}
		if err := decode(opReadCheckRun, r, &body); err != nil {
			return "", false, err
		}
		if body.TotalCount == nil {
			// An empty or null body decodes to no count: a broken answer, retried like any unclassified failure.
			return "", false, errors.New(opReadCheckRun + ": GitHub answered without a count of check runs")
		}
		for i := range body.CheckRuns {
			run := &body.CheckRuns[i]
			if run.Name != providers.CheckRunName || run.HeadSHA != head || run.App == nil || run.App.ID != app {
				continue
			}
			if latest == nil || run.ID > latest.ID {
				latest = run
			}
		}
		read += len(body.CheckRuns)
		if len(body.CheckRuns) == 0 || read >= *body.TotalCount {
			break
		}
	}
	if latest == nil {
		return "", false, nil
	}
	if latest.Status != checkRunCompleted {
		return providers.CheckRunInProgress, true, nil
	}
	switch latest.Conclusion {
	case "success", "neutral", "skipped":
		return providers.CheckRunSuccess, true, nil
	}
	return providers.CheckRunFailure, true, nil
}

func checkRunText(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= providers.MaxCheckRunTextBytes && utf8.ValidString(s)
}
