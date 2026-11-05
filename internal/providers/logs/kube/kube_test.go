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

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

const (
	testNS        = "demo"
	testPod       = "web-7d4b9c-x2x9z"
	testContainer = "app"
	// planted is text that must never reach an error: it stands for log text and names.
	planted = "planted-SECRET-9f8e7d"
	testUID = "6f1c2a9e-3b4d-4e5f-8a7b-0c1d2e3f4a5b"
	// testCID and prevCID are the container IDs of the running and the last terminated instance.
	testCID = "containerd://aaaa1111"
	prevCID = "containerd://bbbb2222"
)

var podsResource = corev1.SchemeGroupVersion.WithResource("pods")

// testPodObject is the pod the queries name: its container restarted once.
func testPodObject() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testPod, UID: testUID},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:         testContainer,
			ContainerID:  testCID,
			RestartCount: 1,
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ContainerID: prevCID},
			},
		}}},
	}
}

// logServer is a fake clientset whose pods/log answers with current or previous text, and which records the
// options of each logs request. Like the API server, it keeps the last TailLines lines and then cuts the
// result to LimitBytes from its start; with ignoreLimits it sends the whole text, like a server that does not
// honour them.
type logServer struct {
	cs           *fake.Clientset
	current      []byte
	previous     []byte
	err          error
	ignoreLimits bool
	opts         []*corev1.PodLogOptions
	// onLog, when set, runs at each logs request (n counts from 1) before it is answered. It runs under the
	// fake clientset's lock, so it may change objects through the tracker only.
	onLog func(n int)
}

// serve applies TailLines and LimitBytes to text the way the API server does.
func serve(text []byte, o *corev1.PodLogOptions) []byte {
	if o.TailLines != nil {
		lines := bytes.SplitAfter(text, []byte{'\n'})
		if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
			lines = lines[:len(lines)-1]
		}
		if n := int(*o.TailLines); n < len(lines) {
			lines = lines[len(lines)-n:]
		}
		text = bytes.Join(lines, nil)
	}
	if o.LimitBytes != nil && int64(len(text)) > *o.LimitBytes {
		text = text[:*o.LimitBytes]
	}
	return text
}

func newLogServer(current string) *logServer {
	l := &logServer{cs: fake.NewClientset(testPodObject()), current: []byte(current)}
	l.cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		opts := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		l.opts = append(l.opts, opts)
		if l.onLog != nil {
			l.onLog(len(l.opts))
		}
		if l.err != nil {
			return true, nil, l.err
		}
		text := l.current
		if opts.Previous {
			text = l.previous
		}
		if !l.ignoreLimits {
			text = serve(text, opts)
		}
		return true, &runtime.Unknown{Raw: text}, nil
	})
	return l
}

func (l *logServer) source(t *testing.T, namespaces ...string) *Source {
	t.Helper()
	if namespaces == nil {
		namespaces = []string{testNS}
	}
	s, err := New(Config{Pods: fakePods(t, l.cs), Namespaces: namespaces})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeREST pairs the pods client of a fake clientset with a REST client of a real clientset whose transport
// answers pod reads from the fake clientset (its reactors and tracker), since the fake has no REST client.
type fakeREST struct {
	corev1client.PodsGetter
	rest rest.Interface
}

func (f fakeREST) RESTClient() rest.Interface { return f.rest }

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakePods(t *testing.T, cs *fake.Clientset) PodsGetter {
	t.Helper()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"), "/")
		if r.Method != http.MethodGet || len(parts) != 3 || parts[1] != podsResource.Resource {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, errors.New("unexpected request")
		}
		var (
			code = http.StatusOK
			body any
		)
		pod, err := cs.CoreV1().Pods(parts[0]).Get(r.Context(), parts[2], metav1.GetOptions{})
		var st apierrors.APIStatus
		switch {
		case errors.As(err, &st):
			s := st.Status()
			s.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
			code, body = int(s.Code), s
		case err != nil:
			return nil, err
		default:
			pod.TypeMeta = metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}
			body = pod
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Request:    r,
		}, nil
	})
	real, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "http://fake.invalid"}, &http.Client{Transport: rt})
	if err != nil {
		t.Fatal(err)
	}
	return fakeREST{PodsGetter: cs.CoreV1(), rest: real.CoreV1().RESTClient()}
}

func query() providers.LogQuery {
	return providers.LogQuery{Namespace: testNS, Pod: testPod, Container: testContainer}
}

// numbered returns n lines "line 000001 ...", each width bytes with its newline.
func numbered(n, width int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		l := fmt.Sprintf("line %06d ", i)
		b.WriteString(l + strings.Repeat(".", width-len(l)-1) + "\n")
	}
	return b.String()
}

func TestRequestOptions(t *testing.T) {
	l := newLogServer("hello\n")
	s := l.source(t)
	q := query()
	if _, err := s.Logs(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	q.Previous, q.Since = true, 1500*time.Millisecond
	if _, err := s.Logs(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	q.Since = 0
	if _, err := s.Logs(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if len(l.opts) != 3 {
		t.Fatalf("got %d requests, want 3", len(l.opts))
	}
	for i, want := range []struct {
		previous bool
		since    int64 // 0: no SinceSeconds
	}{{false, int64(DefaultSince / time.Second)}, {true, 2}, {true, 0}} {
		o := l.opts[i]
		if o.Container != testContainer || o.Previous != want.previous || o.Follow || o.Timestamps ||
			o.InsecureSkipTLSVerifyBackend || o.SinceTime != nil {
			t.Errorf("request %d: options %+v", i, o)
		}
		switch {
		case want.since == 0 && o.SinceSeconds != nil:
			t.Errorf("request %d: SinceSeconds = %d, want none (previous instance, no Since)", i, *o.SinceSeconds)
		case want.since != 0 && (o.SinceSeconds == nil || *o.SinceSeconds != want.since):
			t.Errorf("request %d: SinceSeconds = %v, want %d", i, o.SinceSeconds, want.since)
		}
		if o.TailLines == nil || *o.TailLines != TailLines {
			t.Errorf("request %d: TailLines = %v, want %d", i, o.TailLines, TailLines)
		}
		if o.LimitBytes == nil || *o.LimitBytes != MaxReadBytes+1 {
			t.Errorf("request %d: LimitBytes = %v, want %d", i, o.LimitBytes, MaxReadBytes+1)
		}
	}
}

func TestPreviousContainer(t *testing.T) {
	l := newLogServer("current instance\n")
	l.previous = []byte("previous instance: panic: out of memory\n")
	s := l.source(t)
	q := query()
	q.Previous = true
	got, err := s.Logs(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != string(l.previous) || got.Truncated {
		t.Errorf("Logs(previous) = %+v", got)
	}
}

func TestSmallLogIsReturnedWhole(t *testing.T) {
	text := numbered(10, 40)
	got, err := newLogServer(text).source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != text || got.Truncated {
		t.Errorf("Logs = %q, truncated %v", got.Text, got.Truncated)
	}
}

// A log larger than the excerpt keeps its newest whole lines.
func TestExcerptKeepsTheNewestWholeLines(t *testing.T) {
	const width = 100
	text := numbered(2000, width) // 200,000 bytes, under MaxReadBytes
	for _, maxBytes := range []int64{0, 4096, MaxBytes} {
		q := query()
		q.MaxBytes = maxBytes
		got, err := newLogServer(text).source(t).Logs(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		limit := maxBytes
		if limit == 0 {
			limit = DefaultBytes
		}
		if int64(len(text)) <= limit {
			if got.Text != text || got.Truncated {
				t.Errorf("MaxBytes %d: whole log not returned", maxBytes)
			}
			continue
		}
		if !got.Truncated || int64(len(got.Text)) > limit || int64(len(got.Text)) < limit-width {
			t.Errorf("MaxBytes %d: %d bytes, truncated %v", maxBytes, len(got.Text), got.Truncated)
		}
		if !strings.HasSuffix(text, got.Text) || !strings.HasPrefix(got.Text, "line ") {
			t.Errorf("MaxBytes %d: excerpt is not the newest whole lines: %q...", maxBytes, got.Text[:20])
		}
		if got.EndMissing {
			t.Errorf("MaxBytes %d: end reported missing", maxBytes)
		}
	}
	// A cut that falls right after a newline keeps the whole line after it.
	q := query()
	q.MaxBytes = 5 * width
	got, err := newLogServer(text).source(t).Logs(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != text[len(text)-5*width:] || !got.Truncated {
		t.Errorf("cut on a line boundary: %d bytes, truncated %v", len(got.Text), got.Truncated)
	}
}

// TailLines lines that hold more than MaxReadBytes come back cut from their start (LimitBytes); a second
// request with fewer lines gets the newest ones.
func TestNewestLinesAreReadWhenTheWindowIsLarge(t *testing.T) {
	const width = 1000
	text := numbered(TailLines, width) // 2 MB in TailLines lines
	l := newLogServer(text)
	got, err := l.source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.opts) != 2 || *l.opts[1].TailLines >= TailLines || *l.opts[1].TailLines < 1 {
		t.Fatalf("requests %d, second TailLines %v", len(l.opts), l.opts[len(l.opts)-1].TailLines)
	}
	if !strings.HasSuffix(got.Text, fmt.Sprintf("line %06d ", TailLines)+strings.Repeat(".", width-13)+"\n") {
		t.Errorf("newest line missing; excerpt ends %q", got.Text[max(len(got.Text)-40, 0):])
	}
	if !got.Truncated || got.EndMissing || len(got.Text) > DefaultBytes || !strings.HasSuffix(text, got.Text) {
		t.Errorf("%d bytes, truncated %v, end missing %v", len(got.Text), got.Truncated, got.EndMissing)
	}
}

// The second request asks for fewer lines than the mean length would fit, so newest lines somewhat longer
// than the mean still fit.
func TestMarginCoversLongerNewestLines(t *testing.T) {
	text := numbered(1500, 600) + numbered(500, 1000)
	l := newLogServer(text)
	got, err := l.source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.opts) != 2 || got.EndMissing || !strings.HasSuffix(text, got.Text) {
		t.Errorf("requests %d, end missing %v", len(l.opts), got.EndMissing)
	}
}

// When the newest lines are much longer than the others, the second request is cut too: the excerpt ends
// at the last whole line read and says that its end is missing.
func TestEndMissingWhenTheNewestLinesAreLong(t *testing.T) {
	text := numbered(TailLines-10, 100) + numbered(10, 200<<10)
	l := newLogServer(text)
	q := query()
	q.MaxBytes = MaxBytes
	got, err := l.source(t).Logs(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.opts) != 2 || !got.EndMissing {
		t.Fatalf("requests %d, end missing %v", len(l.opts), got.EndMissing)
	}
	if strings.Contains(got.Text, "line 000010 ") || !strings.HasSuffix(got.Text, "\n") {
		t.Errorf("excerpt holds the newest line or a cut one")
	}
}

// A server that ignores LimitBytes is read no further than MaxReadBytes, and the cut last line is dropped.
func TestOversizedStreamIsCut(t *testing.T) {
	const width = 1000
	text := numbered(3*MaxReadBytes/width, width) + "the end\n"
	l := newLogServer(text)
	l.ignoreLimits = true
	got, err := l.source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || !got.EndMissing || len(got.Text) > DefaultBytes {
		t.Fatalf("%d bytes, truncated %v, end missing %v", len(got.Text), got.Truncated, got.EndMissing)
	}
	if strings.Contains(got.Text, "the end") {
		t.Error("the stream was read past MaxReadBytes")
	}
	// The read stops at MaxReadBytes: the last whole line before it is line MaxReadBytes/width.
	last := fmt.Sprintf("line %06d ", MaxReadBytes/width)
	lines := strings.Split(strings.TrimSuffix(got.Text, "\n"), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], last) || !strings.HasSuffix(got.Text, "\n") {
		t.Errorf("last line %q, want %q...", lines[len(lines)-1], last)
	}
	for _, l := range lines {
		if len(l) != width-1 || !strings.HasPrefix(l, "line ") {
			t.Fatalf("partial line in the excerpt: %q", l)
		}
	}
}

// A log of exactly MaxReadBytes is read whole in one request: its end is not missing.
func TestLogOfExactlyMaxReadBytes(t *testing.T) {
	text := numbered(MaxReadBytes/1024, 1024)
	l := newLogServer(text)
	got, err := l.source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if len(text) != MaxReadBytes || len(l.opts) != 1 || got.EndMissing || !strings.HasSuffix(text, got.Text) ||
		len(got.Text) != DefaultBytes {
		t.Errorf("requests %d, %d bytes, end missing %v", len(l.opts), len(got.Text), got.EndMissing)
	}
}

// A line longer than the excerpt is never returned in part.
func TestOneHugeLineGivesAnEmptyExcerpt(t *testing.T) {
	for name, n := range map[string]int{"longer than the excerpt": DefaultBytes, "longer than a read": MaxReadBytes} {
		l := newLogServer("token " + strings.Repeat("a1", n) + "\n")
		got, err := l.source(t).Logs(context.Background(), query())
		if err != nil {
			t.Fatal(err)
		}
		if got.Text != "" || got.Truncated == got.EndMissing {
			t.Errorf("%s: %d bytes, truncated %v, end missing %v", name, len(got.Text), got.Truncated, got.EndMissing)
		}
		if n == MaxReadBytes && (len(l.opts) != 2 || *l.opts[1].TailLines != 1) {
			t.Errorf("%s: requests %d", name, len(l.opts))
		}
	}
}

func TestReadAllStopsWithoutProgress(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := readAll(stuckReader{})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errNoProgress) {
			t.Errorf("err = %v, want errNoProgress", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readAll spins on a reader that returns nothing")
	}
	// Some empty reads between data are fine.
	r := &slowReader{data: []byte("one\ntwo\n")}
	got, err := readAll(r)
	if err != nil || string(got) != "one\ntwo\n" {
		t.Errorf("readAll = %q, %v", got, err)
	}
}

// stuckReader never returns a byte or an error.
type stuckReader struct{}

func (stuckReader) Read([]byte) (int, error) { return 0, nil }

// slowReader returns one byte after every maxEmptyReads-1 empty reads.
type slowReader struct {
	data  []byte
	calls int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	r.calls++
	if r.calls%maxEmptyReads != 0 {
		return 0, nil
	}
	p[0], r.data = r.data[0], r.data[1:]
	return 1, nil
}

func TestQueryBoundsAreRefusedBeforeAnyRequest(t *testing.T) {
	for name, mutate := range map[string]func(*providers.LogQuery){
		"since too long":     func(q *providers.LogQuery) { q.Since = MaxSince + time.Second },
		"negative since":     func(q *providers.LogQuery) { q.Since = -time.Second },
		"max bytes too high": func(q *providers.LogQuery) { q.MaxBytes = MaxBytes + 1 },
		"negative max bytes": func(q *providers.LogQuery) { q.MaxBytes = -1 },
		"no container":       func(q *providers.LogQuery) { q.Container = "" },
		"bad container":      func(q *providers.LogQuery) { q.Container = "App/" + planted },
		"no pod":             func(q *providers.LogQuery) { q.Pod = "" },
		"bad pod":            func(q *providers.LogQuery) { q.Pod = "../" + planted },
	} {
		t.Run(name, func(t *testing.T) {
			l := newLogServer("x\n")
			q := query()
			mutate(&q)
			_, err := l.source(t).Logs(context.Background(), q)
			if !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("err = %v, want ErrInvalidQuery", err)
			}
			if err != nil && strings.Contains(err.Error(), planted) {
				t.Errorf("error repeats the value: %v", err)
			}
			if n := len(l.cs.Actions()); n != 0 {
				t.Errorf("%d requests sent", n)
			}
		})
	}
	// The limits themselves are accepted.
	l := newLogServer("x\n")
	q := query()
	q.Since, q.MaxBytes = MaxSince, MaxBytes
	if _, err := l.source(t).Logs(context.Background(), q); err != nil {
		t.Errorf("query at the limits refused: %v", err)
	}
}

func TestNamespaceMustBeAllowed(t *testing.T) {
	l := newLogServer("x\n")
	s := l.source(t, testNS, "other")
	q := query()
	q.Namespace = "kube-system"
	_, err := s.Logs(context.Background(), q)
	if !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
	if strings.Contains(err.Error(), "kube-system") {
		t.Errorf("error repeats the namespace: %v", err)
	}
	if n := len(l.cs.Actions()); n != 0 {
		t.Errorf("%d requests sent for a namespace outside the set", n)
	}
	q.Namespace = "other"
	other := testPodObject()
	other.Namespace = q.Namespace
	if err := l.cs.Tracker().Add(other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Logs(context.Background(), q); err != nil {
		t.Errorf("allowed namespace refused: %v", err)
	}
}

func TestEmptyNamespaceSetRefusesEveryQuery(t *testing.T) {
	l := newLogServer("x\n")
	s := l.source(t, []string{}...)
	for _, ns := range []string{testNS, "", "default", "kube-system"} {
		q := query()
		q.Namespace = ns
		if _, err := s.Logs(context.Background(), q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("namespace %q: err = %v, want ErrInvalidQuery", ns, err)
		}
	}
	if n := len(l.cs.Actions()); n != 0 {
		t.Errorf("%d requests sent with no allowed namespace", n)
	}
}

func TestExcerptIsRedacted(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PSR5wHnYp3qk"
	conn := "Server=db;Database=app;User Id=admin;Password=S3cr3tPassw0rd;"
	text := "starting\nAuthorization: Bearer " + jwt + "\nconnecting to " + conn + "\nready\n"
	got, err := newLogServer(text).source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{jwt, "S3cr3tPassw0rd"} {
		if strings.Contains(got.Text, secret) {
			t.Errorf("secret %q not redacted: %q", secret, got.Text)
		}
	}
	if !strings.Contains(got.Text, "[REDACTED") || !strings.HasPrefix(got.Text, "starting\n") {
		t.Errorf("Logs = %q", got.Text)
	}

	// The operator's own patterns apply too.
	r, err := redact.New(redact.Pattern{Kind: "ticket", Expr: `TICKET-[0-9]+`})
	if err != nil {
		t.Fatal(err)
	}
	l := newLogServer("opened TICKET-4711\n")
	s, err := New(Config{Pods: fakePods(t, l.cs), Namespaces: []string{testNS}, Redactor: r})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Text, "4711") {
		t.Errorf("operator pattern not applied: %q", got.Text)
	}
}

func TestInvalidUTF8IsReplaced(t *testing.T) {
	got, err := newLogServer("bad \xff\xfe byte\n").source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "bad ? byte\n" {
		t.Errorf("Logs = %q", got.Text)
	}
}

func TestErrorClasses(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	msg := "log line: password=" + planted
	for name, tc := range map[string]struct {
		err   error
		check func(error) bool
		delay time.Duration
	}{
		"not found":      {apierrors.NewNotFound(gr, planted), providers.IsNotFound, 0},
		"forbidden":      {apierrors.NewForbidden(gr, planted, errors.New(msg)), providers.IsForbidden, 0},
		"unauthorized":   {apierrors.NewUnauthorized(msg), providers.IsForbidden, 0},
		"bad request":    {apierrors.NewBadRequest(msg), providers.IsRejected, 0},
		"conflict":       {apierrors.NewConflict(gr, planted, errors.New(msg)), providers.IsRejected, 0},
		"too many":       {apierrors.NewTooManyRequests(msg, 7), providers.IsRetryable, 7 * time.Second},
		"huge delay":     {apierrors.NewTooManyRequests(msg, 1<<30), providers.IsRetryable, maxRetryAfter},
		"internal":       {apierrors.NewInternalError(errors.New(msg)), providers.IsRetryable, 0},
		"unavailable":    {apierrors.NewServiceUnavailable(msg), providers.IsRetryable, 0},
		"server timeout": {apierrors.NewTimeoutError(msg, 3), providers.IsRetryable, 3 * time.Second},
		"request timeout": {&apierrors.StatusError{ErrStatus: metav1.Status{Code: http.StatusRequestTimeout, Message: msg}},
			providers.IsRetryable, 0},
		"transport": {errors.New("dial tcp 10.0.0.1:443: " + msg), providers.IsRetryable, 0},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLogServer("x\n")
			l.err = tc.err
			_, err := l.source(t).Logs(context.Background(), query())
			if err == nil || !tc.check(err) {
				t.Fatalf("err = %v, wrong class", err)
			}
			classes := 0
			for _, c := range []func(error) bool{
				providers.IsNotFound, providers.IsForbidden, providers.IsRejected, providers.IsRetryable,
			} {
				if c(err) {
					classes++
				}
			}
			if classes != 1 {
				t.Errorf("err = %v is in %d classes", err, classes)
			}
			if strings.Contains(err.Error(), planted) || strings.Contains(err.Error(), "10.0.0.1") {
				t.Errorf("error carries the server's text: %v", err)
			}
			var apiStatus apierrors.APIStatus
			if errors.As(err, &apiStatus) {
				t.Errorf("error wraps the client-go error: %v", err)
			}
			if d, _ := providers.RetryAfter(err); d != tc.delay {
				t.Errorf("RetryAfter = %v, want %v", d, tc.delay)
			}
		})
	}
}

func TestCanceledContextSendsNothing(t *testing.T) {
	l := newLogServer("x\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := l.source(t).Logs(ctx, query())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := len(l.cs.Actions()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

func TestNewValidatesTheConfig(t *testing.T) {
	pods := fakePods(t, fake.NewClientset())
	many := make([]string, MaxNamespaces+1)
	for i := range many {
		many[i] = fmt.Sprintf("ns-%d", i)
	}
	for name, cfg := range map[string]Config{
		"no client":          {Namespaces: []string{testNS}},
		"no REST client":     {Pods: fake.NewClientset().CoreV1(), Namespaces: []string{testNS}},
		"bad namespace":      {Pods: pods, Namespaces: []string{"Demo_" + planted}},
		"too many":           {Pods: pods, Namespaces: many},
		"negative timeout":   {Pods: pods, Timeout: -time.Second},
		"timeout above max":  {Pods: pods, Timeout: MaxTimeout + time.Second},
		"wildcard namespace": {Pods: pods, Namespaces: []string{"*"}},
	} {
		_, err := New(cfg)
		if !providers.IsInvalidConfig(err) {
			t.Errorf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
		if err != nil && strings.Contains(err.Error(), planted) {
			t.Errorf("%s: error repeats the namespace: %v", name, err)
		}
	}
	if _, err := New(Config{Pods: pods, Namespaces: many[:MaxNamespaces], Timeout: MaxTimeout}); err != nil {
		t.Errorf("config at the limits refused: %v", err)
	}
}

// apiSource returns a Source on a real clientset talking to an HTTP server, so that the request path of
// client-go (status decoding, its own retries, the body stream) is the one a cluster sees. The server answers
// a read of the pod itself with testPodObject; h gets every other request, which it counts.
func apiSource(t *testing.T, timeout time.Duration, h http.HandlerFunc) (*Source, *atomic.Int32) {
	t.Helper()
	p := testPodObject()
	p.TypeMeta = metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}
	pod, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	s := apiServer(t, timeout, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == podPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(pod)
			return
		}
		n.Add(1)
		h(w, r)
	})
	return s, &n
}

// podPath is the API path of the pod the queries name.
const podPath = "/api/v1/namespaces/" + testNS + "/pods/" + testPod

// apiServer returns a Source on a real clientset talking to an HTTP server that h answers in full.
func apiServer(t *testing.T, timeout time.Duration, h http.HandlerFunc) *Source {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Pods: cs.CoreV1(), Namespaces: []string{testNS}, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A pod read that fails with a status carrying Retry-After, or with a reset connection, is not retried by
// client-go: one request, a retryable error, no logs request.
func TestPodReadIsNotRetriedByClientGo(t *testing.T) {
	for name, fail := range map[string]func(w http.ResponseWriter){
		"429": func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"503": func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		},
		"reset": func(w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0) // close with RST
			}
			_ = conn.Close()
		},
	} {
		t.Run(name, func(t *testing.T) {
			var pods, logs atomic.Int32
			s := apiServer(t, 0, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != podPath {
					logs.Add(1)
					return
				}
				pods.Add(1)
				fail(w)
			})
			_, err := s.Logs(context.Background(), query())
			if !providers.IsRetryable(err) || strings.Contains(err.Error(), testPod) {
				t.Errorf("err = %v, want a retryable error without the pod name", err)
			}
			if p, l := pods.Load(), logs.Load(); p != 1 || l != 0 {
				t.Errorf("%d pod reads and %d logs requests, want 1 and 0", p, l)
			}
		})
	}
}

// A 429 from the API server is returned at once with its delay: client-go does not retry it on its own.
func TestAPIServerStatusIsNotRetriedByClientGo(t *testing.T) {
	s, n := apiSource(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/"+testNS+"/pods/"+testPod+"/log" {
			t.Errorf("path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":%q,`+
			`"reason":"TooManyRequests","details":{"retryAfterSeconds":1},"code":429}`, planted)
	})
	_, err := s.Logs(context.Background(), query())
	if !providers.IsRetryable(err) || strings.Contains(err.Error(), planted) {
		t.Fatalf("err = %v", err)
	}
	if d, _ := providers.RetryAfter(err); d != time.Second {
		t.Errorf("RetryAfter = %v, want 1s", d)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("%d requests, want 1", got)
	}
}

func TestAPIServerNotFound(t *testing.T) {
	s, _ := apiSource(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":%q,`+
			`"reason":"NotFound","code":404}`, planted)
	})
	_, err := s.Logs(context.Background(), query())
	if !providers.IsNotFound(err) || strings.Contains(err.Error(), planted) {
		t.Errorf("err = %v", err)
	}
}

func TestStreamCutShortIsRetryable(t *testing.T) {
	s, _ := apiSource(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write([]byte("password=" + planted + "\n"))
		// The handler returns with the body short of its length: the client sees an unexpected EOF.
	})
	_, err := s.Logs(context.Background(), query())
	if !providers.IsRetryable(err) || strings.Contains(err.Error(), planted) {
		t.Errorf("err = %v", err)
	}
}

// stalledServer sends one line and then nothing, for up to three seconds.
func stalledServer(t *testing.T) http.HandlerFunc {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial line\n"))
		w.(http.Flusher).Flush()
		// Only a deadline ends the stream sooner than this.
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(3 * time.Second):
		}
	}
}

// The adapter's own timeout ending a stalled stream is a transient failure, not the caller's deadline, and
// the part read is not returned.
func TestTimeoutEndsAStalledStream(t *testing.T) {
	s, _ := apiSource(t, 100*time.Millisecond, stalledServer(t))
	start := time.Now()
	got, err := s.Logs(context.Background(), query())
	if !providers.IsRetryable(err) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Logs = %+v, %v; want a retryable error", got, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

// The caller's deadline ending a stalled stream is returned as the caller's context error.
func TestCallerDeadlineEndsAStalledStream(t *testing.T) {
	s, _ := apiSource(t, MaxTimeout, stalledServer(t))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, err := s.Logs(ctx, query())
	if !errors.Is(err, context.DeadlineExceeded) || providers.IsRetryable(err) {
		t.Errorf("Logs = %+v, %v; want context.DeadlineExceeded", got, err)
	}
}

// podReads counts the reads of the pod itself (not of its log) a fake clientset saw.
func podReads(cs *fake.Clientset) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "get" && a.GetResource() == podsResource && a.GetSubresource() == "" {
			n++
		}
	}
	return n
}

// An excerpt records the pod UID and the container instance it was read from, and when.
func TestExcerptIsTiedToTheInstance(t *testing.T) {
	for name, tc := range map[string]struct {
		previous bool
		id       string
	}{"current": {false, testCID}, "previous": {true, prevCID}} {
		t.Run(name, func(t *testing.T) {
			l := newLogServer("hello\n")
			l.previous = []byte("bye\n")
			q := query()
			q.Previous = tc.previous
			start := time.Now()
			got, err := l.source(t).Logs(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			want := providers.LogInstance{PodUID: testUID, ContainerID: tc.id, RestartCount: 1}
			if got.Instance != want || got.InstanceUncertain {
				t.Errorf("instance %+v, uncertain %v; want %+v, certain", got.Instance, got.InstanceUncertain, want)
			}
			if got.CollectedAt.Before(start) || got.CollectedAt.After(time.Now()) {
				t.Errorf("CollectedAt %v outside the call", got.CollectedAt)
			}
			if n := podReads(l.cs); n != 2 {
				t.Errorf("%d pod reads, want 2 (before and after)", n)
			}
		})
	}
}

// A StatefulSet pod is recreated under the same name while its logs are read: the excerpt may be the new
// pod's, so it is marked uncertain and keeps the UID of the pod checked before.
func TestStatefulSetPodRecreatedUnderTheSameName(t *testing.T) {
	l := newLogServer("hello\n")
	l.onLog = func(int) {
		recreated := testPodObject()
		recreated.UID = "0a0b0c0d-1111-2222-3333-444455556666"
		recreated.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "web"}}
		// Same container ID and restart count: only the pod UID tells the instances apart.
		if err := l.cs.Tracker().Delete(podsResource, testNS, testPod); err != nil {
			t.Error(err)
		}
		if err := l.cs.Tracker().Create(podsResource, recreated, testNS); err != nil {
			t.Error(err)
		}
	}
	got, err := l.source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if !got.InstanceUncertain || got.Instance.PodUID != testUID {
		t.Errorf("instance %+v, uncertain %v; want the first UID, uncertain", got.Instance, got.InstanceUncertain)
	}
}

// The container restarts between the adapter's two requests (after the first was answered, before the second
// is). The first read the old instance, the second the new one (or, for Previous, which is relative, the
// instance the first read as current): uncertain.
func TestRestartBetweenTheTwoRequests(t *testing.T) {
	for name, previous := range map[string]bool{"current": false, "previous": true} {
		t.Run(name, func(t *testing.T) {
			text := numbered(TailLines, 1000) // over MaxReadBytes: two requests
			l := newLogServer(text)
			l.previous = []byte(text)
			l.onLog = func(n int) {
				if n != 2 {
					return
				}
				restarted := testPodObject()
				cs := &restarted.Status.ContainerStatuses[0]
				cs.RestartCount = 2
				cs.ContainerID = "containerd://cccc3333"
				cs.LastTerminationState.Terminated.ContainerID = testCID
				if err := l.cs.Tracker().Update(podsResource, restarted, testNS); err != nil {
					t.Error(err)
				}
			}
			q := query()
			q.Previous = previous
			got, err := l.source(t).Logs(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if len(l.opts) != 2 {
				t.Fatalf("%d logs requests, want 2", len(l.opts))
			}
			if !got.InstanceUncertain || got.Instance.RestartCount != 1 {
				t.Errorf("instance %+v, uncertain %v; want restart count 1, uncertain", got.Instance, got.InstanceUncertain)
			}
		})
	}
}

// A pod missing before any logs request is not found, and no logs request is sent; a pod gone after the
// read leaves the excerpt uncertain.
func TestPodGoneBeforeOrAfterTheRead(t *testing.T) {
	l := newLogServer("hello\n")
	if err := l.cs.Tracker().Delete(podsResource, testNS, testPod); err != nil {
		t.Fatal(err)
	}
	if _, err := l.source(t).Logs(context.Background(), query()); !providers.IsNotFound(err) || len(l.opts) != 0 {
		t.Errorf("err = %v after %d logs requests; want not found and none", err, len(l.opts))
	}

	l = newLogServer("hello\n")
	l.onLog = func(int) {
		if err := l.cs.Tracker().Delete(podsResource, testNS, testPod); err != nil {
			t.Error(err)
		}
	}
	got, err := l.source(t).Logs(context.Background(), query())
	if err != nil || !got.InstanceUncertain || got.Text != "hello\n" {
		t.Errorf("Logs = %+v, %v; want the text, uncertain", got, err)
	}
}

// A failed check of the pod before reading returns no excerpt, and its error carries no name or server text.
func TestPodReadErrorBeforeTheRead(t *testing.T) {
	l := newLogServer("hello\n")
	l.cs.PrependReactor("get", podsResource.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewInternalError(errors.New("pod " + planted))
	})
	got, err := l.source(t).Logs(context.Background(), query())
	if !providers.IsRetryable(err) || got.Text != "" {
		t.Fatalf("Logs = %+v, %v; want a retryable error", got, err)
	}
	if strings.Contains(err.Error(), planted) || strings.Contains(err.Error(), testPod) {
		t.Errorf("error carries a name or the server's text: %v", err)
	}
	if len(l.opts) != 0 {
		t.Errorf("%d logs requests, want none", len(l.opts))
	}
}

// A failed check of the pod after reading (refused, failed, or past the deadline) returns the logs read,
// marked uncertain.
func TestPodReadErrorAfterTheRead(t *testing.T) {
	for name, fail := range map[string]func() error{
		"forbidden": func() error { return apierrors.NewForbidden(podsResource.GroupResource(), planted, nil) },
		"internal":  func() error { return apierrors.NewInternalError(errors.New(planted)) },
		"deadline": func() error {
			time.Sleep(300 * time.Millisecond) // past the Source's timeout below
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLogServer("hello\n")
			reads := 0
			l.cs.PrependReactor("get", podsResource.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetSubresource() != "" {
					return false, nil, nil
				}
				if reads++; reads == 1 {
					return false, nil, nil
				}
				if err := fail(); err != nil {
					return true, nil, err
				}
				return false, nil, nil
			})
			s, err := New(Config{Pods: fakePods(t, l.cs), Namespaces: []string{testNS}, Timeout: 100 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Logs(context.Background(), query())
			if err != nil || got.Text != "hello\n" || !got.InstanceUncertain || got.Instance.PodUID != testUID {
				t.Errorf("Logs = %+v, %v; want the text, uncertain", got, err)
			}
		})
	}
}

// A deadline or the adapter's timeout during a pod read is reported as the pod read's, in fixed text.
func TestContextEndsAPodRead(t *testing.T) {
	stalled := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == podPath {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
	}
	s := apiServer(t, MaxTimeout, stalled)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.Logs(ctx, query())
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "pod read") ||
		strings.Contains(err.Error(), "logs") {
		t.Errorf("caller deadline: err = %v", err)
	}
	s = apiServer(t, 100*time.Millisecond, stalled)
	_, err = s.Logs(context.Background(), query())
	if !providers.IsRetryable(err) || !strings.Contains(err.Error(), "pod read") || strings.Contains(err.Error(), "logs") {
		t.Errorf("adapter timeout: err = %v", err)
	}
}

// A status that names no instance, or an ID out of bounds, never ties an excerpt.
func TestUnknownInstanceIsUncertain(t *testing.T) {
	for name, change := range map[string]func(*corev1.Pod){
		"no status":       func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
		"empty ID":        func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" },
		"ID too long":     func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = strings.Repeat("a", 129) },
		"ID bad bytes":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "x://a\nb" },
		"UID bad bytes":   func(p *corev1.Pod) { p.UID = "a b" },
		"other container": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "sidecar" },
		// The kubelet serves the last terminated instance while the container waits.
		"waiting": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
		},
		"terminated without ID": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPodObject()
			change(p)
			l := newLogServer("hello\n")
			if err := l.cs.Tracker().Update(podsResource, p, testNS); err != nil {
				t.Fatal(err)
			}
			got, err := l.source(t).Logs(context.Background(), query())
			if err != nil || !got.InstanceUncertain {
				t.Errorf("Logs = %+v, %v; want uncertain", got, err)
			}
			if len(got.Instance.ContainerID) > providers.MaxLogInstanceIDLength ||
				strings.ContainsAny(got.Instance.ContainerID+got.Instance.PodUID, " \n") {
				t.Errorf("instance out of bounds: %+v", got.Instance)
			}
		})
	}
}
