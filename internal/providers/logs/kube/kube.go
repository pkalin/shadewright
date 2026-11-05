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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// Limits on queries and excerpts. They are fixed in code so that no configuration can widen them.
const (
	// DefaultBytes bounds the excerpt when a query sets no MaxBytes.
	DefaultBytes = providers.DefaultLogBytes
	// MaxBytes bounds the MaxBytes of a query.
	MaxBytes = 256 << 10
	// MaxReadBytes bounds what is read of one stream. LimitBytes is sent as MaxReadBytes plus one, so that
	// a log of exactly MaxReadBytes is told apart from a longer one, and no more than that is read whatever
	// the server sends. The excerpt is the newest MaxBytes (or less) of what was read.
	MaxReadBytes = 1 << 20
	// TailLines is sent with the first request: the server returns at most the last TailLines lines of the
	// time window. When they hold more than MaxReadBytes, a second request asks for fewer lines.
	TailLines = 2000
	// DefaultSince is how far back a query of the current container reads when it sets no Since. A query of
	// the previous instance that sets no Since has no time limit: that instance stopped at some time in the
	// past, and its last lines are what a diagnosis needs.
	DefaultSince = time.Hour
	// MaxSince bounds the Since of a query.
	MaxSince = 24 * time.Hour
	// DefaultTimeout bounds one query when Config.Timeout is zero.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout bounds Config.Timeout.
	MaxTimeout = 2 * time.Minute
	// MaxNamespaces bounds Config.Namespaces.
	MaxNamespaces = 256
)

// readChunk is the first buffer size of a read from the stream; the buffer grows up to MaxReadBytes+1.
const readChunk = 32 << 10

// maxEmptyReads bounds the reads in a row that return no byte and no error, as in package bufio.
const maxEmptyReads = 100

// tailMargin is the share of the lines that would fit in MaxReadBytes, at the mean line length of the first
// read, that the second request asks for, so that longer newest lines still fit.
const tailMargin = 0.75

// maxRetryAfter caps a delay asked for by the server.
const maxRetryAfter = 10 * time.Minute

// ErrInvalidQuery means the query was refused before any request: a namespace outside Config.Namespaces, a
// pod or container name that is not valid, no container, or a Since or MaxBytes outside the limits. It is
// permanent.
var ErrInvalidQuery = errors.New("invalid logs query")

// PodsGetter is the part of a Kubernetes clientset the adapter uses: CoreV1() of a clientset, real or fake
// (k8s.io/client-go/kubernetes/fake), satisfies it.
type PodsGetter = corev1client.PodsGetter

// Config configures a [Source].
type Config struct {
	// Pods reads the logs and the pods, usually CoreV1() of the analyzer's clientset. Required.
	Pods PodsGetter
	// Namespaces are the namespaces a query may name, each a DNS-1123 label, at most MaxNamespaces; the
	// analyzer passes its watched namespaces (--watch-namespaces). Empty refuses every query.
	Namespaces []string
	// Redactor redacts the excerpt; nil means the built-in rules of package redact.
	Redactor *redact.Redactor
	// Timeout bounds one query, both logs requests and both pod reads included. Zero means
	// DefaultTimeout; at most MaxTimeout.
	Timeout time.Duration
}

// Source is a [providers.LogSource] reading pod logs from the Kubernetes API. It is safe for concurrent use.
type Source struct {
	pods       PodsGetter
	namespaces map[string]struct{}
	redact     func(string) string
	timeout    time.Duration
}

var _ providers.LogSource = (*Source)(nil)

// New validates cfg and returns a Source. It makes no request. Errors never repeat a namespace.
func New(cfg Config) (*Source, error) {
	if cfg.Pods == nil {
		return nil, fmt.Errorf("pods client is required: %w", providers.ErrInvalidConfig)
	}
	if cfg.Timeout < 0 || cfg.Timeout > MaxTimeout {
		return nil, fmt.Errorf("timeout must be between 0 and %s: %w", MaxTimeout, providers.ErrInvalidConfig)
	}
	if len(cfg.Namespaces) > MaxNamespaces {
		return nil, fmt.Errorf("more than %d namespaces: %w", MaxNamespaces, providers.ErrInvalidConfig)
	}
	namespaces := make(map[string]struct{}, len(cfg.Namespaces))
	for i, ns := range cfg.Namespaces {
		if len(validation.IsDNS1123Label(ns)) > 0 {
			return nil, fmt.Errorf("namespace %d is not a DNS-1123 label: %w", i, providers.ErrInvalidConfig)
		}
		namespaces[ns] = struct{}{}
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	red := redact.String
	if cfg.Redactor != nil {
		red = cfg.Redactor.String
	}
	return &Source{pods: cfg.Pods, namespaces: namespaces, redact: red, timeout: timeout}, nil
}

// Logs returns the newest, redacted lines of one container within the bounds of q and of the package. See
// the package documentation for the errors.
func (s *Source) Logs(ctx context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	opts, keep, err := s.options(q)
	if err != nil {
		return providers.LogExcerpt{}, err
	}
	if err := ctx.Err(); err != nil {
		return providers.LogExcerpt{}, fmt.Errorf("logs query not sent: %w", err)
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// The log request names the pod and container by name only, so the instance is checked before the first
	// and after the last request. Pod UIDs are never reused and a restart count only grows, so equal checks
	// mean no other instance answered either request in between.
	before, err := s.instance(ctx, parent, q)
	if err != nil {
		return providers.LogExcerpt{}, err
	}
	data, over, err := s.read(ctx, parent, q, opts)
	if err != nil {
		return providers.LogExcerpt{}, err
	}
	if over {
		// The server cut the window at LimitBytes, from its start: the newest lines are missing. Ask again for
		// fewer lines, so that they fit.
		tail := fewerLines(data)
		opts.TailLines = &tail
		if data, over, err = s.read(ctx, parent, q, opts); err != nil {
			return providers.LogExcerpt{}, err
		}
	}
	collected := time.Now().UTC()
	after, err := s.instance(ctx, parent, q)
	switch {
	case errors.Is(err, providers.ErrNotFound):
		// The pod is gone: nothing ties the text to the instance checked before.
		after = instance{}
	case err != nil:
		return providers.LogExcerpt{}, err
	}
	text, truncated := cut(data, over, keep)
	return providers.LogExcerpt{
		Text:              s.redact(strings.ToValidUTF8(text, "?")),
		Truncated:         truncated,
		EndMissing:        over,
		Instance:          before.LogInstance,
		CollectedAt:       collected,
		InstanceUncertain: !before.known || after != before,
	}, nil
}

// instance is the pod and container instance a query names, as the pod's status gives it. known is false
// when the status names no instance or an ID is out of bounds; such an instance never ties an excerpt.
type instance struct {
	providers.LogInstance
	known bool
}

// instance reads the pod a query names and returns the instance of its container. A missing pod is
// providers.ErrNotFound; other errors are classified like those of a logs request.
func (s *Source) instance(ctx, parent context.Context, q providers.LogQuery) (instance, error) {
	pod, err := s.pods.Pods(q.Namespace).Get(ctx, q.Pod, metav1.GetOptions{})
	if err != nil {
		return instance{}, requestError(ctx, parent, "pod read", err)
	}
	if err := contextError(ctx, parent); err != nil {
		return instance{}, err
	}
	uid, uidOK := boundedID(string(pod.UID))
	inst := instance{LogInstance: providers.LogInstance{PodUID: uid}}
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses,
	} {
		for _, cs := range statuses {
			if cs.Name != q.Container {
				continue
			}
			raw := cs.ContainerID
			if q.Previous {
				raw = ""
				if t := cs.LastTerminationState.Terminated; t != nil {
					raw = t.ContainerID
				}
			}
			id, idOK := boundedID(raw)
			inst.ContainerID, inst.RestartCount = id, cs.RestartCount
			inst.known = uidOK && idOK && uid != "" && id != ""
			return inst, nil
		}
	}
	return inst, nil
}

// boundedID returns s when it is at most providers.MaxLogInstanceIDLength bytes of letters, digits and
// ._:/+-, and "" and false otherwise. An empty s is returned with true.
func boundedID(s string) (string, bool) {
	if len(s) > providers.MaxLogInstanceIDLength {
		return "", false
	}
	for _, c := range []byte(s) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:/+-", c) >= 0
		if !ok {
			return "", false
		}
	}
	return s, true
}

// read sends one request and reads at most MaxReadBytes+1 of the stream. over reports that the stream held
// more than MaxReadBytes; data is then cut to MaxReadBytes.
func (s *Source) read(ctx, parent context.Context, q providers.LogQuery, opts *corev1.PodLogOptions,
) (data []byte, over bool, err error) {
	stream, err := s.pods.Pods(q.Namespace).GetLogs(q.Pod, opts).MaxRetries(0).Stream(ctx)
	if err != nil {
		return nil, false, requestError(ctx, parent, "logs request", err)
	}
	defer func() { _ = stream.Close() }()
	data, err = readAll(io.LimitReader(stream, MaxReadBytes+1))
	// A stream that ended because a context did is never returned as complete.
	if err := contextError(ctx, parent); err != nil {
		return nil, false, err
	}
	if err != nil {
		return nil, false, &providers.RetryableError{Err: errors.New("logs stream was cut short")}
	}
	if len(data) > MaxReadBytes {
		return data[:MaxReadBytes], true, nil
	}
	return data, false, nil
}

// contextError returns the caller's context error when the caller's context is done, so that errors.Is
// matches it, and a retryable error when only the adapter's own timeout expired.
func contextError(ctx, parent context.Context) error {
	if err := parent.Err(); err != nil {
		return fmt.Errorf("logs were not read: %w", err)
	}
	if ctx.Err() != nil {
		return &providers.RetryableError{Err: errors.New("logs request timed out")}
	}
	return nil
}

// errNoProgress means the stream returned no byte and no error too many times in a row.
var errNoProgress = errors.New("logs stream made no progress")

// readAll reads r to its end. The buffer grows by doubling up to MaxReadBytes+1, the most a LimitReader of
// that size gives; reads that return nothing maxEmptyReads times in a row fail.
func readAll(r io.Reader) ([]byte, error) {
	buf := make([]byte, 0, readChunk)
	empty := 0
	for {
		if len(buf) == cap(buf) {
			if cap(buf) >= MaxReadBytes+1 {
				return buf, nil
			}
			grown := make([]byte, len(buf), min(2*cap(buf), MaxReadBytes+1))
			copy(grown, buf)
			buf = grown
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
		if n > 0 {
			empty = 0
		} else if empty++; empty >= maxEmptyReads {
			return nil, errNoProgress
		}
	}
}

// fewerLines returns the tail lines that should fit in MaxReadBytes, from the mean length of the lines in
// data (the first MaxReadBytes of the window), with a margin: at least 1, below TailLines.
func fewerLines(data []byte) int64 {
	lines := bytes.Count(data, []byte{'\n'})
	if lines == 0 {
		return 1
	}
	mean := float64(len(data)) / float64(lines)
	n := int64(float64(MaxReadBytes) / mean * tailMargin)
	return min(max(n, 1), TailLines-1)
}

// cut returns the newest whole lines of data that fit in keep bytes. When endMissing, the last line, which
// the read cut, is dropped. When older bytes are dropped, the first line is dropped too unless it starts
// right after a newline, so that no cut line reaches the redactor; truncated reports that.
func cut(data []byte, endMissing bool, keep int) (string, bool) {
	if endMissing {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	if len(data) <= keep {
		return string(data), false
	}
	from := len(data) - keep
	if data[from-1] != '\n' {
		i := bytes.IndexByte(data[from:], '\n')
		if i < 0 {
			return "", true
		}
		from += i + 1
	}
	return string(data[from:]), true
}

// options checks q and returns the request options and the bytes to keep. Errors never repeat a value.
func (s *Source) options(q providers.LogQuery) (*corev1.PodLogOptions, int, error) {
	if _, ok := s.namespaces[q.Namespace]; !ok {
		return nil, 0, fmt.Errorf("namespace is not an allowed namespace: %w", ErrInvalidQuery)
	}
	if len(validation.IsDNS1123Subdomain(q.Pod)) > 0 {
		return nil, 0, fmt.Errorf("pod is not a valid pod name: %w", ErrInvalidQuery)
	}
	if len(validation.IsDNS1123Label(q.Container)) > 0 {
		return nil, 0, fmt.Errorf("container is missing or not a valid container name: %w", ErrInvalidQuery)
	}
	since := q.Since
	switch {
	case since == 0 && !q.Previous:
		since = DefaultSince
	case since < 0 || since > MaxSince:
		return nil, 0, fmt.Errorf("since must be between 0 and %s: %w", MaxSince, ErrInvalidQuery)
	}
	keep := q.MaxBytes
	switch {
	case keep == 0:
		keep = DefaultBytes
	case keep < 0 || keep > MaxBytes:
		return nil, 0, fmt.Errorf("max bytes must be between 0 and %d: %w", MaxBytes, ErrInvalidQuery)
	}
	limit, tail := int64(MaxReadBytes+1), int64(TailLines)
	opts := &corev1.PodLogOptions{
		Container:  q.Container,
		Previous:   q.Previous,
		TailLines:  &tail,
		LimitBytes: &limit,
	}
	if since > 0 {
		// Whole seconds, rounded up, so the window asked for is never shorter than Since.
		sinceSeconds := int64((since + time.Second - 1) / time.Second)
		opts.SinceSeconds = &sinceSeconds
	}
	return opts, int(keep), nil
}

// requestError classifies a failed request by its status code; what is a fixed name of the request. The
// error of client-go is dropped: its message holds the server's text, which can repeat names and log text.
func requestError(ctx, parent context.Context, what string, err error) error {
	if cerr := contextError(ctx, parent); cerr != nil {
		return cerr
	}
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return &providers.RetryableError{Err: fmt.Errorf("%s failed", what)}
	}
	code := int(st.Status().Code)
	switch {
	case code == http.StatusNotFound:
		return fmt.Errorf("%s: pod or container not found (status %d): %w", what, code, providers.ErrNotFound)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%s: the API server refused the identity (status %d): %w", what, code, providers.ErrForbidden)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		var delay time.Duration
		if secs, ok := apierrors.SuggestsClientDelay(err); ok {
			// Capped in seconds first: a huge value must not overflow the Duration.
			delay = time.Duration(min(max(secs, 0), int(maxRetryAfter/time.Second))) * time.Second
		}
		return &providers.RetryableError{
			RetryAfter: delay,
			Err:        fmt.Errorf("%s: the API server could not serve it (status %d)", what, code),
		}
	default:
		return fmt.Errorf("%s: the API server refused it (status %d): %w", what, code, providers.ErrRejected)
	}
}
