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
	"context"
	"errors"
	"fmt"
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
)

// logServer is a fake clientset whose pods/log answers with current or previous text, and which records the
// options of each logs request.
type logServer struct {
	cs       *fake.Clientset
	current  []byte
	previous []byte
	err      error
	opts     []*corev1.PodLogOptions
}

func newLogServer(current string) *logServer {
	l := &logServer{cs: fake.NewClientset(), current: []byte(current)}
	l.cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		opts := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		l.opts = append(l.opts, opts)
		if l.err != nil {
			return true, nil, l.err
		}
		if opts.Previous {
			return true, &runtime.Unknown{Raw: l.previous}, nil
		}
		return true, &runtime.Unknown{Raw: l.current}, nil
	})
	return l
}

func (l *logServer) source(t *testing.T, namespaces ...string) *Source {
	t.Helper()
	if namespaces == nil {
		namespaces = []string{testNS}
	}
	s, err := New(Config{Pods: l.cs.CoreV1(), Namespaces: namespaces})
	if err != nil {
		t.Fatal(err)
	}
	return s
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
	if len(l.opts) != 2 {
		t.Fatalf("got %d requests, want 2", len(l.opts))
	}
	for i, want := range []struct {
		previous bool
		since    int64
	}{{false, int64(DefaultSince / time.Second)}, {true, 2}} {
		o := l.opts[i]
		if o.Container != testContainer || o.Previous != want.previous || o.Follow || o.Timestamps ||
			o.InsecureSkipTLSVerifyBackend || o.SinceTime != nil {
			t.Errorf("request %d: options %+v", i, o)
		}
		if o.SinceSeconds == nil || *o.SinceSeconds != want.since {
			t.Errorf("request %d: SinceSeconds = %v, want %d", i, o.SinceSeconds, want.since)
		}
		if o.TailLines == nil || *o.TailLines != TailLines {
			t.Errorf("request %d: TailLines = %v, want %d", i, o.TailLines, TailLines)
		}
		if o.LimitBytes == nil || *o.LimitBytes != MaxReadBytes {
			t.Errorf("request %d: LimitBytes = %v, want %d", i, o.LimitBytes, MaxReadBytes)
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
	}
}

// A server that ignores LimitBytes is read no further than MaxReadBytes, and the cut last line is dropped.
func TestOversizedStreamIsCut(t *testing.T) {
	const width = 1000
	text := numbered(3*MaxReadBytes/width, width) + "the end\n"
	got, err := newLogServer(text).source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Text) > DefaultBytes {
		t.Fatalf("%d bytes, truncated %v", len(got.Text), got.Truncated)
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

// A line longer than the excerpt is never returned in part.
func TestOneHugeLineGivesAnEmptyExcerpt(t *testing.T) {
	text := "token " + strings.Repeat("a1", DefaultBytes) + "\n"
	got, err := newLogServer(text).source(t).Logs(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "" || !got.Truncated {
		t.Errorf("Logs = %d bytes, truncated %v", len(got.Text), got.Truncated)
	}
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
	conn := "postgres://admin:S3cr3tPassw0rd@db.internal:5432/app"
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
	s, err := New(Config{Pods: l.cs.CoreV1(), Namespaces: []string{testNS}, Redactor: r})
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
	pods := fake.NewClientset().CoreV1()
	many := make([]string, MaxNamespaces+1)
	for i := range many {
		many[i] = fmt.Sprintf("ns-%d", i)
	}
	for name, cfg := range map[string]Config{
		"no client":          {Namespaces: []string{testNS}},
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
// client-go (status decoding, its own retries, the body stream) is the one a cluster sees.
func apiSource(t *testing.T, timeout time.Duration, h http.HandlerFunc) (*Source, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Pods: cs.CoreV1(), Namespaces: []string{testNS}, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return s, &n
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

// A server that stops sending runs into the deadline, and the part read is not returned.
func TestTimeoutEndsAStalledStream(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s, _ := apiSource(t, 100*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial line\n"))
		w.(http.Flusher).Flush()
		// The server would end the stream after a few seconds: only the adapter's own deadline ends it sooner.
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(3 * time.Second):
		}
	})
	start := time.Now()
	got, err := s.Logs(context.Background(), query())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Logs = %+v, %v; want context.DeadlineExceeded", got, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}
