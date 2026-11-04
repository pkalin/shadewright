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
	"k8s.io/apimachinery/pkg/util/validation"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// Limits on queries and excerpts. They are fixed in code so that no configuration can widen them.
const (
	// DefaultBytes bounds the excerpt when a query sets no MaxBytes; it equals the fake's default.
	DefaultBytes = 64 << 10
	// MaxBytes bounds the MaxBytes of a query.
	MaxBytes = 256 << 10
	// MaxReadBytes bounds what is read of one stream, and is sent as LimitBytes. The excerpt is the newest
	// MaxBytes (or less) of it.
	MaxReadBytes = 1 << 20
	// TailLines is sent with every request: the server returns at most the last TailLines lines of the time
	// window.
	TailLines = 2000
	// DefaultSince is how far back a query reads when it sets no Since.
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

// readChunk is the size of one read from the stream.
const readChunk = 32 << 10

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
	// Pods reads the logs, usually CoreV1() of the analyzer's clientset. Required.
	Pods PodsGetter
	// Namespaces are the namespaces a query may name, each a DNS-1123 label, at most MaxNamespaces; the
	// analyzer passes its watched namespaces (--watch-namespaces). Empty refuses every query.
	Namespaces []string
	// Redactor redacts the excerpt; nil means the built-in rules of package redact.
	Redactor *redact.Redactor
	// Timeout bounds one query, from the request to the end of the stream. Zero means DefaultTimeout; at
	// most MaxTimeout.
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
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	stream, err := s.pods.Pods(q.Namespace).GetLogs(q.Pod, opts).MaxRetries(0).Stream(ctx)
	if err != nil {
		return providers.LogExcerpt{}, requestError(ctx, err)
	}
	defer func() { _ = stream.Close() }()
	tail, total, err := readTail(io.LimitReader(stream, MaxReadBytes), keep)
	if ctx.Err() != nil {
		// A stream that ended because the context did is never returned as complete.
		return providers.LogExcerpt{}, fmt.Errorf("logs were not read: %w", ctx.Err())
	}
	if err != nil {
		return providers.LogExcerpt{}, &providers.RetryableError{Err: errors.New("logs stream was cut short")}
	}
	text, truncated := trim(tail, total)
	return providers.LogExcerpt{Text: s.redact(strings.ToValidUTF8(text, "?")), Truncated: truncated}, nil
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
	case since == 0:
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
	// Whole seconds, rounded up, so the window asked for is never shorter than Since.
	sinceSeconds := int64((since + time.Second - 1) / time.Second)
	limit, tail := int64(MaxReadBytes), int64(TailLines)
	return &corev1.PodLogOptions{
		Container:    q.Container,
		Previous:     q.Previous,
		SinceSeconds: &sinceSeconds,
		TailLines:    &tail,
		LimitBytes:   &limit,
	}, int(keep), nil
}

// readTail reads r to its end and returns the last keep bytes of it and the number of bytes read. At most
// keep plus one chunk is held in memory.
func readTail(r io.Reader, keep int) ([]byte, int64, error) {
	buf := make([]byte, 0, keep+readChunk)
	var total int64
	for {
		if len(buf) > keep {
			buf = buf[:copy(buf, buf[len(buf)-keep:])]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		total += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, total, err
		}
	}
	if len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	return buf, total, nil
}

// trim drops the first line, which may be partial, when bytes were dropped before tail, and the last line
// after its final newline when the read stopped at MaxReadBytes, so that no cut line reaches the redactor. It reports whether anything was cut.
func trim(tail []byte, total int64) (string, bool) {
	cutFront := total > int64(len(tail))
	cutEnd := total >= MaxReadBytes
	if cutFront {
		i := bytes.IndexByte(tail, '\n')
		if i < 0 {
			return "", true
		}
		tail = tail[i+1:]
	}
	if cutEnd {
		i := bytes.LastIndexByte(tail, '\n')
		tail = tail[:i+1]
	}
	return string(tail), cutFront || cutEnd
}

// requestError classifies a failed request by its status code. The error of client-go is dropped: its
// message holds the server's text, which can repeat names and log text.
func requestError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("logs request failed: %w", cerr)
	}
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return &providers.RetryableError{Err: errors.New("logs request failed")}
	}
	code := int(st.Status().Code)
	switch {
	case code == http.StatusNotFound:
		return fmt.Errorf("pod or container not found (status %d): %w", code, providers.ErrNotFound)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("the API server refused the identity (status %d): %w", code, providers.ErrForbidden)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		var delay time.Duration
		if secs, ok := apierrors.SuggestsClientDelay(err); ok {
			// Capped in seconds first: a huge value must not overflow the Duration.
			delay = time.Duration(min(max(secs, 0), int(maxRetryAfter/time.Second))) * time.Second
		}
		return &providers.RetryableError{
			RetryAfter: delay,
			Err:        fmt.Errorf("the API server could not serve the logs (status %d)", code),
		}
	default:
		return fmt.Errorf("the API server refused the logs request (status %d): %w", code, providers.ErrRejected)
	}
}
