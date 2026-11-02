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
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

const web = "web"

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

func TestMetricsAndLogs(t *testing.T) {
	ctx := context.Background()
	var m fake.Metrics
	if _, err := m.Query(ctx, providers.MetricsQuery{Template: "memory"}); !providers.IsNotFound(err) {
		t.Errorf("unknown template: %v", err)
	}
	m.Set("memory", []providers.Series{{Labels: map[string]string{"pod": web}}})
	if s, err := m.Query(ctx, providers.MetricsQuery{Template: "memory"}); err != nil || len(s) != 1 {
		t.Errorf("query = %v, %v", s, err)
	}

	var l fake.Logs
	k := fake.LogKey{Namespace: "demo", Pod: "web-1", Container: web}
	l.Set(k, "0123456789")
	q := providers.LogQuery{Namespace: "demo", Pod: "web-1", Container: web, MaxBytes: 4}
	e, err := l.Logs(ctx, q)
	if err != nil || e.Text != "6789" || !e.Truncated {
		t.Errorf("bounded logs = %+v, %v", e, err)
	}
	q.Previous = true
	if _, err := l.Logs(ctx, q); !providers.IsNotFound(err) {
		t.Errorf("previous container: %v", err)
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
	if len(s.Records()) != 1 {
		t.Error("record count")
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
