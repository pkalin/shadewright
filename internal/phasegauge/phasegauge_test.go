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
package phasegauge

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

type phase string

// Phases of the tests.
const (
	open   phase = "Open"
	closed phase = "Closed"
)

func TestLabel(t *testing.T) {
	known := []phase{open, closed}
	cases := map[phase]string{open: string(open), closed: string(closed), "": None, "open": Other,
		"a name from the cluster": Other}
	for in, want := range cases {
		if got := Label(in, known); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
		if !slices.Contains(Labels(known), Label(in, known)) {
			t.Errorf("Label(%q) = %q is not in Labels", in, Label(in, known))
		}
	}
}

// informers is an Informers whose GetInformer waits for release, as a cache waits for its informer to sync.
type informers struct {
	client.Reader
	release chan struct{}
	err     error
}

func (i *informers) GetInformer(ctx context.Context, _ client.Object, _ ...cache.InformerGetOption) (cache.Informer, error) {
	select {
	case <-i.release:
		return nil, i.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// eventually waits until cond holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The source is empty until the informer has synced, set while the runnable runs, and empty again after.
func TestSourceIsSetOnlyWhileTheInformerIsSynced(t *testing.T) {
	var s Source
	inf := &informers{release: make(chan struct{})}
	r := s.Runnable(inf, &remediationv1alpha1.Incident{})
	if lr, ok := r.(interface{ NeedLeaderElection() bool }); !ok || !lr.NeedLeaderElection() {
		t.Fatal("the runnable must run only on the leader")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)
	if s.Reader() != nil {
		t.Fatal("the source was set before the informer synced")
	}
	close(inf.release)
	eventually(t, "the source set", func() bool { return s.Reader() != nil })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.Reader() != nil {
		t.Fatal("the source is still set after the runnable stopped")
	}
}

// An informer that cannot be had leaves the source empty and does not stop the manager.
func TestSourceStaysEmptyWhenTheInformerFails(t *testing.T) {
	var s Source
	inf := &informers{release: make(chan struct{}), err: errors.New("no kind")}
	close(inf.release)
	if err := s.Runnable(inf, &remediationv1alpha1.Incident{}).Start(t.Context()); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	if s.Reader() != nil {
		t.Fatal("the source was set although the informer failed")
	}
}
