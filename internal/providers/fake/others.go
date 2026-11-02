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

package fake

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/pkalin/shadewright/internal/providers"
)

var (
	_ providers.DeliveryVerifier = (*Delivery)(nil)
	_ providers.ModelProvider    = (*Model)(nil)
	_ providers.MetricsSource    = (*Metrics)(nil)
	_ providers.LogSource        = (*Logs)(nil)
	_ providers.Notifier         = (*Notifier)(nil)
	_ providers.AnalysisStore    = (*Store)(nil)
	_ providers.Signer           = (*Signer)(nil)
)

// Delivery is a delivery system whose unit statuses tests set directly.
type Delivery struct {
	Hook
	mu    sync.Mutex
	units map[providers.DeliveryUnit]providers.DeliveryStatus
}

// Set records the status of a unit.
func (d *Delivery) Set(u providers.DeliveryUnit, s providers.DeliveryStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.units == nil {
		d.units = map[providers.DeliveryUnit]providers.DeliveryStatus{}
	}
	d.units[u] = s
}

// Status returns the recorded status of a unit.
func (d *Delivery) Status(ctx context.Context, u providers.DeliveryUnit) (providers.DeliveryStatus, error) {
	if err := d.before("Status"); err != nil {
		return providers.DeliveryStatus{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.units[u]
	if !ok {
		return providers.DeliveryStatus{}, fmt.Errorf("%s %s/%s: %w", u.Kind, u.Namespace, u.Name, providers.ErrNotFound)
	}
	return s, nil
}

// Model replays scripted completions in order and records every request.
type Model struct {
	Hook
	mu       sync.Mutex
	replies  []providers.Completion
	requests []providers.CompletionRequest
}

// Script appends completions to return, one per call.
func (m *Model) Script(c ...providers.Completion) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replies = append(m.replies, c...)
}

// Requests returns the requests received so far.
func (m *Model) Requests() []providers.CompletionRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

// Complete returns the next scripted completion, or a retryable error when the script is exhausted.
func (m *Model) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	if err := m.before("Complete"); err != nil {
		return providers.Completion{}, err
	}
	m.mu.Lock()
	m.requests = append(m.requests, req)
	if len(m.replies) == 0 {
		m.mu.Unlock()
		return providers.Completion{}, &providers.RetryableError{Err: fmt.Errorf("fake model has no scripted reply")}
	}
	c := m.replies[0]
	m.replies = m.replies[1:]
	m.mu.Unlock()
	if err := m.after("Complete"); err != nil {
		return providers.Completion{}, err
	}
	return c, nil
}

// Metrics answers queries from results registered per template.
type Metrics struct {
	Hook
	mu      sync.Mutex
	results map[string][]providers.Series
}

// Set registers the result for a template.
func (m *Metrics) Set(template string, s []providers.Series) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.results == nil {
		m.results = map[string][]providers.Series{}
	}
	m.results[template] = cloneSeries(s)
}

// Query returns the registered result for the template.
func (m *Metrics) Query(ctx context.Context, q providers.MetricsQuery) ([]providers.Series, error) {
	if err := m.before("Query"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.results[q.Template]
	if !ok {
		return nil, fmt.Errorf("template %q: %w", q.Template, providers.ErrNotFound)
	}
	return cloneSeries(s), nil
}

func cloneSeries(in []providers.Series) []providers.Series {
	out := make([]providers.Series, len(in))
	for i, s := range in {
		out[i] = providers.Series{Labels: maps.Clone(s.Labels), Samples: slices.Clone(s.Samples)}
	}
	return out
}

// LogKey identifies one container's logs in the Logs fake.
type LogKey struct {
	Namespace, Pod, Container string
	Previous                  bool
}

// Logs returns logs registered per container, honouring MaxBytes.
type Logs struct {
	Hook
	mu   sync.Mutex
	logs map[LogKey]string
}

// Set registers the logs of a container.
func (l *Logs) Set(k LogKey, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logs == nil {
		l.logs = map[LogKey]string{}
	}
	l.logs[k] = text
}

// Logs returns the most recent MaxBytes of the registered logs.
func (l *Logs) Logs(ctx context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	if err := l.before("Logs"); err != nil {
		return providers.LogExcerpt{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.logs[LogKey{q.Namespace, q.Pod, q.Container, q.Previous}]
	if !ok {
		return providers.LogExcerpt{}, fmt.Errorf("logs of %s/%s/%s: %w", q.Namespace, q.Pod, q.Container, providers.ErrNotFound)
	}
	if q.MaxBytes > 0 && int64(len(t)) > q.MaxBytes {
		return providers.LogExcerpt{Text: t[int64(len(t))-q.MaxBytes:], Truncated: true}, nil
	}
	return providers.LogExcerpt{Text: t}, nil
}

// Notifier keeps one thread per key; notifying again replaces its title and body.
type Notifier struct {
	Hook
	mu      sync.Mutex
	threads map[string]*Thread
	order   []string
}

// Thread is a notification thread in the Notifier fake.
type Thread struct {
	Ref     providers.NotificationRef
	Title   string
	Body    string
	Updates int
}

// Notify creates or updates the thread for the key.
func (n *Notifier) Notify(ctx context.Context, msg providers.Notification) (providers.NotificationRef, error) {
	if err := n.before("Notify"); err != nil {
		return providers.NotificationRef{}, err
	}
	if msg.Key == "" {
		return providers.NotificationRef{}, fmt.Errorf("notification without a key")
	}
	n.mu.Lock()
	if n.threads == nil {
		n.threads = map[string]*Thread{}
	}
	t, ok := n.threads[msg.Key]
	if !ok {
		id := strconv.Itoa(len(n.order) + 1)
		t = &Thread{Ref: providers.NotificationRef{ID: id, URL: "https://notify.example.test/" + id}}
		n.threads[msg.Key] = t
		n.order = append(n.order, msg.Key)
	} else {
		t.Updates++
	}
	t.Title, t.Body = msg.Title, msg.Body
	ref := t.Ref
	n.mu.Unlock()
	return ref, n.after("Notify")
}

// Threads returns a copy of every thread, oldest first.
func (n *Notifier) Threads() []Thread {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Thread, 0, len(n.order))
	for _, k := range n.order {
		out = append(out, *n.threads[k])
	}
	return out
}

// Store keeps analysis records in memory, write-once.
type Store struct {
	Hook
	mu   sync.Mutex
	recs map[string]providers.AnalysisRecord
}

// Put writes a record once; the same ID with the same data again is a no-op.
func (s *Store) Put(ctx context.Context, rec providers.AnalysisRecord) (string, error) {
	if err := s.before("Put"); err != nil {
		return "", err
	}
	if rec.ID == "" {
		return "", fmt.Errorf("analysis record without an ID")
	}
	s.mu.Lock()
	if s.recs == nil {
		s.recs = map[string]providers.AnalysisRecord{}
	}
	ref := "memory://analyses/" + rec.ID
	if old, ok := s.recs[rec.ID]; ok {
		s.mu.Unlock()
		if bytes.Equal(old.Data, rec.Data) && old.Fingerprint == rec.Fingerprint {
			return ref, nil
		}
		return "", fmt.Errorf("analysis record %q exists with other content: %w", rec.ID, providers.ErrConflict)
	}
	rec.Data = slices.Clone(rec.Data)
	s.recs[rec.ID] = rec
	s.mu.Unlock()
	return ref, s.after("Put")
}

// Records returns a deep copy of every record by ID.
func (s *Store) Records() map[string]providers.AnalysisRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]providers.AnalysisRecord, len(s.recs))
	for id, rec := range s.recs {
		rec.Data = slices.Clone(rec.Data)
		out[id] = rec
	}
	return out
}

// Signer signs RS256 with an RSA key generated in memory; the public key is exposed for verification.
type Signer struct {
	Hook
	key *rsa.PrivateKey
}

// NewSigner generates a 2048-bit test key.
func NewSigner() (*Signer, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate test key: %w", err)
	}
	return &Signer{key: k}, nil
}

// Public returns the public key.
func (s *Signer) Public() *rsa.PublicKey { return &s.key.PublicKey }

// Algorithm reports RS256.
func (s *Signer) Algorithm() providers.SignatureAlgorithm { return providers.RS256 }

// Sign signs a SHA-256 digest.
func (s *Signer) Sign(ctx context.Context, digest []byte) ([]byte, error) {
	if err := s.before("Sign"); err != nil {
		return nil, err
	}
	if len(digest) != crypto.SHA256.Size() {
		return nil, fmt.Errorf("digest is %d bytes, want %d", len(digest), crypto.SHA256.Size())
	}
	return rsa.SignPKCS1v15(nil, s.key, crypto.SHA256, digest)
}
