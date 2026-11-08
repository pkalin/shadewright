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
	"errors"
	"reflect"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// gitReads are the GitProvider methods that write nothing to the host. Every other method is a write, and a
// write method added to the interface is gated by this test's rule until it is listed here on purpose.
var gitReads = map[string]bool{"Capabilities": true, "ReadFile": true, "ListDir": true, "BranchHead": true,
	"CheckBranch": true, "CheckMerged": true, opFind: true}

// TestGatedGitGatesEveryWrite calls every GitProvider method of a gated provider whose gate refuses, and checks
// that each write asked the gate and never reached the host, and that no read asked it.
func TestGatedGitGatesEveryWrite(t *testing.T) {
	typ := reflect.TypeFor[providers.GitProvider]()
	writes := 0
	for m := range typ.Methods() {
		t.Run(m.Name, func(t *testing.T) {
			inner := fakegit.NewGit("main", map[string][]byte{"a.yaml": []byte("a: 1\n")})
			var reached []string
			inner.SetFail(func(op string) error {
				reached = append(reached, op)
				return nil
			})
			asked := 0
			gated := &gatedGit{git: inner, allow: func(context.Context) error {
				asked++
				return freshness.ErrPaused
			}}
			args := []reflect.Value{}
			for in := range m.Type.Ins() {
				if in == reflect.TypeFor[context.Context]() {
					args = append(args, reflect.ValueOf(context.Background()))
					continue
				}
				args = append(args, reflect.Zero(in))
			}
			out := reflect.ValueOf(gated).MethodByName(m.Name).Call(args)
			if gitReads[m.Name] {
				if asked != 0 {
					t.Fatalf("the read %s asked the gate", m.Name)
				}
				return
			}
			writes++
			if asked != 1 {
				t.Fatalf("the write %s asked the gate %d times, want once", m.Name, asked)
			}
			if len(reached) != 0 {
				t.Fatalf("the write %s reached the host while the gate refused: %v", m.Name, reached)
			}
			err, _ := reflect.TypeAssert[error](out[len(out)-1])
			if !errors.Is(err, freshness.ErrPaused) {
				t.Fatalf("the write %s returned %v, want the gate's error", m.Name, err)
			}
		})
	}
	if writes != typ.NumMethod()-len(gitReads) {
		t.Fatalf("checked %d writes, want %d", writes, typ.NumMethod()-len(gitReads))
	}
}

// checkpointProbe is a GitProvider whose every method records what the write checkpoint of its context
// answers right now, as an adapter does before each write request it sends.
type checkpointProbe struct {
	asked map[string]error
}

func (p *checkpointProbe) probe(ctx context.Context, name string) error {
	p.asked[name] = providers.CheckWrite(ctx)
	return nil
}

func (p *checkpointProbe) Capabilities() providers.GitCapabilities {
	p.asked["Capabilities"] = nil
	return providers.GitCapabilities{}
}

func (p *checkpointProbe) ReadFile(ctx context.Context, _, _ string, _ int) (providers.File, error) {
	return providers.File{}, p.probe(ctx, "ReadFile")
}

func (p *checkpointProbe) ListDir(ctx context.Context, _, _ string) ([]providers.DirEntry, error) {
	return nil, p.probe(ctx, "ListDir")
}

func (p *checkpointProbe) BranchHead(ctx context.Context, _ string) (string, error) {
	return "", p.probe(ctx, "BranchHead")
}

func (p *checkpointProbe) CheckBranch(ctx context.Context, _, _ string) (providers.BranchState, error) {
	return 0, p.probe(ctx, "CheckBranch")
}

func (p *checkpointProbe) CheckMerged(ctx context.Context, _, _, _ string) (providers.BranchState, string, error) {
	return 0, "", p.probe(ctx, "CheckMerged")
}

func (p *checkpointProbe) CommentChangeRequest(ctx context.Context, _ int64, _ string) error {
	return p.probe(ctx, "CommentChangeRequest")
}

func (p *checkpointProbe) FindChangeRequest(ctx context.Context, _ string) (providers.ChangeRequest, error) {
	return providers.ChangeRequest{}, p.probe(ctx, opFind)
}

func (p *checkpointProbe) CreateBranch(ctx context.Context, _, _ string) error {
	return p.probe(ctx, "CreateBranch")
}

func (p *checkpointProbe) CommitFile(ctx context.Context, _, _ string, _ providers.FileChange) (string, error) {
	return "", p.probe(ctx, "CommitFile")
}

func (p *checkpointProbe) OpenChangeRequest(ctx context.Context, _ providers.NewChangeRequest) (
	providers.ChangeRequest, error) {
	return providers.ChangeRequest{}, p.probe(ctx, "OpenChangeRequest")
}

func (p *checkpointProbe) EnableAutoMerge(ctx context.Context, _ int64, _ string) error {
	return p.probe(ctx, "EnableAutoMerge")
}

func (p *checkpointProbe) CloseChangeRequest(ctx context.Context, _ int64) error {
	return p.probe(ctx, "CloseChangeRequest")
}

// TestGatedGitPassesTheGateAsCheckpoint checks that every write of a gated provider gives the wrapped provider
// the gate as its write checkpoint, so a pause set after the method's own check stops the adapter's next write
// request (#338), and that reads carry none.
func TestGatedGitPassesTheGateAsCheckpoint(t *testing.T) {
	typ := reflect.TypeFor[providers.GitProvider]()
	for m := range typ.Methods() {
		t.Run(m.Name, func(t *testing.T) {
			inner := &checkpointProbe{asked: map[string]error{}}
			asked := 0
			// The first ask (the method's own gate) allows; the pause is set right after it.
			gated := &gatedGit{git: inner, allow: func(context.Context) error {
				asked++
				if asked > 1 {
					return freshness.ErrPaused
				}
				return nil
			}}
			args := []reflect.Value{}
			for in := range m.Type.Ins() {
				if in == reflect.TypeFor[context.Context]() {
					args = append(args, reflect.ValueOf(context.Background()))
					continue
				}
				args = append(args, reflect.Zero(in))
			}
			reflect.ValueOf(gated).MethodByName(m.Name).Call(args)
			got, called := inner.asked[m.Name]
			if !called {
				t.Fatalf("%s did not reach the wrapped provider", m.Name)
			}
			if gitReads[m.Name] {
				if asked != 0 || got != nil {
					t.Fatalf("the read %s carried a checkpoint (asked %d, answered %v)", m.Name, asked, got)
				}
				return
			}
			if !errors.Is(got, freshness.ErrPaused) || asked != 2 {
				t.Fatalf("the write %s: checkpoint answered %v after %d asks, want the pause on the second",
					m.Name, got, asked)
			}
		})
	}
}

// TestGatedGitWritesWhenAllowed checks that an allowed write reaches the host.
func TestGatedGitWritesWhenAllowed(t *testing.T) {
	inner := fakegit.NewGit("main", map[string][]byte{"a.yaml": []byte("a: 1\n")})
	gated := &gatedGit{git: inner, allow: func(context.Context) error { return nil }}
	ctx := context.Background()
	head, err := inner.BranchHead(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := gated.CreateBranch(ctx, "shadewright/x", head); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if got, err := inner.BranchHead(ctx, "shadewright/x"); err != nil || got != head {
		t.Fatalf("the branch was not created: %q, %v", got, err)
	}
}

// TestProviderErrorKeepsTheKillSwitch checks that wrapping a gate refusal as a provider error keeps it, so the
// caller still tells a pause from a failure of the host.
func TestProviderErrorKeepsTheKillSwitch(t *testing.T) {
	for _, want := range []error{freshness.ErrPaused, freshness.ErrWriteBlocked} {
		if err := freshness.WrapProvider("create the branch", want); !errors.Is(err, want) {
			t.Fatalf("WrapProvider lost %v: %v", want, err)
		}
	}
}

// TestWriteAllowedNeedsTheAPIReader checks that the gate never falls back to the client (and its cache): with
// no APIReader it blocks the write, even when the client would read settings that allow it.
func TestWriteAllowedNeedsTheAPIReader(t *testing.T) {
	r := &RemediationRequestReconciler{Client: allowingClient{}}
	if err := r.writeAllowed(context.Background()); !errors.Is(err, freshness.ErrWriteBlocked) {
		t.Fatalf("writeAllowed without an APIReader returned %v, want ErrWriteBlocked", err)
	}
	r.APIReader = allowingClient{}
	if err := r.writeAllowed(context.Background()); err != nil {
		t.Fatalf("writeAllowed with an APIReader that allows returned %v", err)
	}
}

// TestGatedRepositoryIsGated checks that the opener cmd/main.go gives the source mapping is gated: reads pass,
// and a write is blocked while the kill switch cannot be read (here: no APIReader).
func TestGatedRepositoryIsGated(t *testing.T) {
	inner := fakegit.NewGit("main", nil)
	r := &RemediationRequestReconciler{OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (
		providers.GitProvider, error) {
		return inner, nil
	}}
	ctx := context.Background()
	git, err := r.GatedRepository(ctx, remediationv1alpha1.GitRepository{})
	if err != nil {
		t.Fatal(err)
	}
	head, err := git.BranchHead(ctx, "main")
	if err != nil {
		t.Fatalf("a read through the gated opener failed: %v", err)
	}
	if err := git.CreateBranch(ctx, "shadewright/x", head); !errors.Is(err, freshness.ErrWriteBlocked) {
		t.Fatalf("CreateBranch through the gated opener returned %v, want ErrWriteBlocked", err)
	}
	if _, err := inner.BranchHead(ctx, "shadewright/x"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("the blocked write reached the host: %v", err)
	}
}

// allowingClient answers every Get with an object left as it is: settings that do not pause.
type allowingClient struct{ client.Client }

func (allowingClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return nil
}
