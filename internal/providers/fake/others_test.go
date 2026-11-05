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

package fake_test

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

const (
	web    = "web"
	memory = "memory"
)

func TestDelivery(t *testing.T) {
	var d fake.Delivery
	u := providers.DeliveryUnit{Kind: "Kustomization", Namespace: "flux-system", Name: "apps"}
	if _, err := d.Status(context.Background(), u); !providers.IsNotFound(err) {
		t.Errorf("unknown unit: %v", err)
	}
	d.Set(u, providers.DeliveryStatus{AppliedRevision: "abc", Ready: true})
	s, err := d.Status(context.Background(), u)
	if err != nil || s.AppliedRevision != "abc" || !s.Ready {
		t.Errorf("status = %+v, %v", s, err)
	}
}

func TestModel(t *testing.T) {
	var m fake.Model
	m.Script(providers.Completion{Content: "one"}, providers.Completion{Content: "two"})
	ctx := context.Background()
	for _, want := range []string{"one", "two"} {
		c, err := m.Complete(ctx, providers.CompletionRequest{})
		if err != nil || c.Content != want {
			t.Fatalf("got %q, %v; want %q", c.Content, err, want)
		}
	}
	if _, err := m.Complete(ctx, providers.CompletionRequest{}); !providers.IsRetryable(err) {
		t.Errorf("exhausted script: %v", err)
	}
	if n := len(m.Requests()); n != 3 {
		t.Errorf("%d requests recorded, want 3", n)
	}
}

func TestModelCopiesData(t *testing.T) {
	reply := providers.Completion{ToolCalls: []providers.ToolCall{{Name: "get_resource", Arguments: []byte(`{"a":1}`)}}}
	var m1, m2 fake.Model
	m1.Script(reply)
	m2.Script(reply)
	ctx := context.Background()
	req := providers.CompletionRequest{
		Messages: []providers.Message{{Role: providers.RoleUser, Content: "hi"}},
		Tools:    []providers.ToolDefinition{{Name: "t", Parameters: []byte(`{}`)}},
	}
	got, _ := m1.Complete(ctx, req)
	got.ToolCalls[0].Arguments[2] = 'X'
	req.Messages[0].Content = "changed"
	req.Tools[0].Parameters[0] = 'X'
	if again, _ := m2.Complete(ctx, providers.CompletionRequest{}); string(again.ToolCalls[0].Arguments) != `{"a":1}` {
		t.Errorf("changing one model's reply changed another's: %s", again.ToolCalls[0].Arguments)
	}
	h := m1.Requests()
	if h[0].Messages[0].Content != "hi" || string(h[0].Tools[0].Parameters) != "{}" {
		t.Errorf("changing the request changed the history: %+v", h[0])
	}
	h[0].Messages[0].Content = "edited"
	if m1.Requests()[0].Messages[0].Content != "hi" {
		t.Error("changing the returned history changed the recorded request")
	}
}

func TestModelLostResponse(t *testing.T) {
	var m fake.Model
	m.Script(providers.Completion{Content: "lost"}, providers.Completion{Content: "next"})
	m.SetAfter(func(string) error { return &providers.RetryableError{Err: errors.New("reset")} })
	ctx := context.Background()
	if _, err := m.Complete(ctx, providers.CompletionRequest{}); !providers.IsRetryable(err) {
		t.Fatalf("want the injected error, got %v", err)
	}
	m.SetAfter(nil)
	c, err := m.Complete(ctx, providers.CompletionRequest{})
	if err != nil || c.Content != "next" {
		t.Errorf("the lost reply was not consumed: %q, %v", c.Content, err)
	}
	if n := len(m.Requests()); n != 2 {
		t.Errorf("%d requests recorded, want 2", n)
	}
}

// A fake told which templates select pods refuses a query that forgets the pod set or sends one where it does
// not belong; one that is not told checks nothing.
func TestMetricsPodTemplates(t *testing.T) {
	ctx := context.Background()
	const deployment = "deployment"
	pods := []string{"web-0"}
	m := fake.Metrics{PodTemplates: []string{memory}}
	m.Set(memory, nil)
	m.Set(deployment, nil)
	if _, err := m.Query(ctx, providers.MetricsQuery{Template: memory}); !providers.IsRejected(err) {
		t.Errorf("pod template without pods: %v", err)
	}
	if _, err := m.Query(ctx, providers.MetricsQuery{Template: deployment, Pods: pods}); !providers.IsRejected(err) {
		t.Errorf("other template with pods: %v", err)
	}
	if _, err := m.Query(ctx, providers.MetricsQuery{Template: memory, Pods: pods}); err != nil {
		t.Errorf("pod template with pods: %v", err)
	}
	if _, err := m.Query(ctx, providers.MetricsQuery{Template: deployment}); err != nil {
		t.Errorf("other template without pods: %v", err)
	}
	var unchecked fake.Metrics
	unchecked.Set(memory, nil)
	if _, err := unchecked.Query(ctx, providers.MetricsQuery{Template: memory}); err != nil {
		t.Errorf("fake without PodTemplates: %v", err)
	}
}

func TestMetricsAndLogs(t *testing.T) {
	ctx := context.Background()
	var m fake.Metrics
	if _, err := m.Query(ctx, providers.MetricsQuery{Template: memory}); !providers.IsNotFound(err) {
		t.Errorf("unknown template: %v", err)
	}
	m.Set(memory, []providers.Series{{Labels: map[string]string{"pod": web}}})
	r, err := m.Query(ctx, providers.MetricsQuery{Template: memory})
	if err != nil || len(r.Series) != 1 || r.Partial {
		t.Fatalf("query = %v, %v", r, err)
	}
	r.Series[0].Labels["pod"] = "changed"
	if again, _ := m.Query(ctx, providers.MetricsQuery{Template: memory}); again.Series[0].Labels["pod"] != web {
		t.Error("changing a result changed the stored series")
	}
	m.SetResult(memory, providers.MetricsResult{Partial: true})
	if r, err := m.Query(ctx, providers.MetricsQuery{Template: memory}); err != nil || !r.Partial || len(r.Series) != 0 {
		t.Errorf("partial result = %v, %v", r, err)
	}

	var l fake.Logs
	k := fake.LogKey{Namespace: "demo", Pod: "web-1", Container: web}
	l.Set(k, "0123456789")
	q := providers.LogQuery{Namespace: "demo", Pod: "web-1", Container: web, MaxBytes: 4}
	e, err := l.Logs(ctx, q)
	if err != nil || e.Text != "6789" || !e.Truncated {
		t.Errorf("bounded logs = %+v, %v", e, err)
	}
	l.Set(k, strings.Repeat("x", providers.DefaultLogBytes+10))
	q.MaxBytes = 0
	if e, _ := l.Logs(ctx, q); len(e.Text) != providers.DefaultLogBytes || !e.Truncated {
		t.Errorf("zero MaxBytes returned %d bytes, truncated=%v", len(e.Text), e.Truncated)
	}
	q.Previous = true
	if _, err := l.Logs(ctx, q); !providers.IsRejected(err) {
		t.Errorf("previous instance the pod does not have: %v", err)
	}
	q.Previous, q.Pod = false, "web-2"
	if _, err := l.Logs(ctx, q); !providers.IsNotFound(err) {
		t.Errorf("missing pod: %v", err)
	}
}

func TestNotifierOneThreadPerKey(t *testing.T) {
	var n fake.Notifier
	ctx := context.Background()
	r1, err := n.Notify(ctx, providers.Notification{Key: "req-1", Title: "a"})
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := n.Notify(ctx, providers.Notification{Key: "req-1", Title: "b"})
	if r1 != r2 {
		t.Errorf("second notify made a new thread: %v vs %v", r1, r2)
	}
	_, _ = n.Notify(ctx, providers.Notification{Key: "req-2", Title: "c"})
	th := n.Threads()
	if len(th) != 2 || th[0].Title != "b" || th[0].Updates != 1 {
		t.Errorf("threads = %+v", th)
	}
	if _, err := n.Notify(ctx, providers.Notification{}); err == nil {
		t.Error("notification without a key accepted")
	}
}

func TestStoreWriteOnce(t *testing.T) {
	var s fake.Store
	ctx := context.Background()
	rec := providers.AnalysisRecord{ID: "a1", Fingerprint: "sha256:x", Data: []byte("{}")}
	r1, err := s.Put(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Put(ctx, rec)
	if err != nil || r1 != r2 {
		t.Errorf("same record again = %q, %v", r2, err)
	}
	rec.Data = []byte(`{"x":1}`)
	if _, err := s.Put(ctx, rec); !providers.IsConflict(err) {
		t.Errorf("same ID, other data: %v", err)
	}
	recs := s.Records()
	if len(recs) != 1 {
		t.Fatal("record count")
	}
	recs["a1"].Data[0] = 'X'
	if _, err := s.Put(ctx, providers.AnalysisRecord{ID: "a1", Fingerprint: "sha256:x", Data: []byte("{}")}); err != nil {
		t.Errorf("changing Records() output changed the stored record: %v", err)
	}
}

func TestSigner(t *testing.T) {
	s, err := fake.NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	if s.Algorithm() != providers.RS256 {
		t.Errorf("algorithm %s", s.Algorithm())
	}
	d := sha256.Sum256([]byte("header.payload"))
	sig, err := s.Sign(context.Background(), d[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(s.Public(), crypto.SHA256, d[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
	if _, err := s.Sign(context.Background(), []byte("short")); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("short digest: %v", err)
	}
}
