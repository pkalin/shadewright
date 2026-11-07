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

package incidentcontext

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// Timeouts of one source call (a cluster read, a metrics query or a log read) and of a whole build. A build
// makes up to about 30 calls one after the other, so the build deadline, not the sum of the call timeouts,
// bounds how long it takes: the calls left when it passes are not made, and their sources are marked "could
// not fetch".
const (
	DefaultSourceTimeout = 30 * time.Second
	MaxSourceTimeout     = 2 * time.Minute
	DefaultBuildTimeout  = 2 * time.Minute
	MaxBuildTimeout      = 5 * time.Minute
)

// The window of an analysis: from a little before the Incident was first seen until now.
const (
	// DefaultWindow is the window when the Incident has no firstSeen.
	DefaultWindow = time.Hour
	// MinWindow is the shortest window, so that an Incident first seen a moment ago still gets some history.
	MinWindow = 15 * time.Minute
	// MaxWindow is the longest window: the range bound of the metrics source (design section 4).
	MaxWindow = 6 * time.Hour
	// windowLead is how long before firstSeen the window starts, to show the lead-up.
	windowLead = 10 * time.Minute
)

// maxValueBytes bounds one value from the cluster (a message, a reason) after redaction. A longer value is
// left out whole, never cut, and replaced by a note of its size.
const maxValueBytes = 1024

// Redactor redacts text: in the analyzer, a *redact.Redactor with the built-in rules (operator patterns are
// planned; nothing configures them yet). Its output must never be longer than its input, as package redact guarantees.
type Redactor interface {
	String(s string) string
}

// Config configures a [Builder].
type Config struct {
	// Pods reads pods: the analyzer's cache, which already holds the pods of the watched namespaces (a list in
	// one namespace and a get). Required.
	Pods client.Reader
	// Objects gets Deployments, StatefulSets and ReplicaSets from the API server: the analyzer may get them,
	// not list or watch them. Required.
	Objects client.Reader
	// Namespaces are the namespaces a subject may be in: the analyzer's watched namespaces
	// (--watch-namespaces), the same set its cache and both sources are given. A subject in any other
	// namespace is refused before any read or source call, and its status, logs and metrics are "not
	// available". Empty refuses every subject.
	Namespaces []string
	// Metrics is the metrics source; nil means not configured, so the metrics section is "not available".
	Metrics providers.MetricsSource
	// Logs is the log source; nil means not configured, so the log section is "not available". Its excerpts
	// are redacted again here, whatever the source did.
	Logs providers.LogSource
	// Redactor redacts every piece of text. Required.
	Redactor Redactor
	// Limits are the byte bounds; the zero value means [DefaultLimits].
	Limits Limits
	// SourceTimeout bounds each source call; zero means [DefaultSourceTimeout], at most [MaxSourceTimeout].
	SourceTimeout time.Duration
	// BuildTimeout bounds a whole build; zero means [DefaultBuildTimeout], at most [MaxBuildTimeout].
	BuildTimeout time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// Builder builds the context of an Incident. It is safe for concurrent use when its sources are.
type Builder struct {
	pods, objects client.Reader
	namespaces    map[string]struct{}
	metrics       providers.MetricsSource
	logs          providers.LogSource
	redactor      Redactor
	limits        Limits
	timeout       time.Duration
	buildTimeout  time.Duration
	now           func() time.Time
}

// New returns a builder, or an error when a required field is missing or a bound is out of range.
func New(cfg Config) (*Builder, error) {
	if cfg.Pods == nil || cfg.Objects == nil {
		return nil, errors.New("context builder: a pod reader and an object reader are required")
	}
	if cfg.Redactor == nil {
		return nil, errors.New("context builder: a redactor is required")
	}
	if cfg.Limits == (Limits{}) {
		cfg.Limits = DefaultLimits()
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("context builder: %w", err)
	}
	if cfg.SourceTimeout == 0 {
		cfg.SourceTimeout = DefaultSourceTimeout
	}
	if cfg.SourceTimeout < 0 || cfg.SourceTimeout > MaxSourceTimeout {
		return nil, fmt.Errorf("context builder: source timeout %s is outside 0 to %s", cfg.SourceTimeout, MaxSourceTimeout)
	}
	if cfg.BuildTimeout == 0 {
		cfg.BuildTimeout = DefaultBuildTimeout
	}
	if cfg.BuildTimeout < 0 || cfg.BuildTimeout > MaxBuildTimeout {
		return nil, fmt.Errorf("context builder: build timeout %s is outside 0 to %s", cfg.BuildTimeout, MaxBuildTimeout)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	namespaces := make(map[string]struct{}, len(cfg.Namespaces))
	for _, ns := range cfg.Namespaces {
		namespaces[ns] = struct{}{}
	}
	return &Builder{
		pods: cfg.Pods, objects: cfg.Objects, namespaces: namespaces, metrics: cfg.Metrics, logs: cfg.Logs, redactor: cfg.Redactor,
		limits: cfg.Limits, timeout: cfg.SourceTimeout, buildTimeout: cfg.BuildTimeout, now: cfg.Now,
	}, nil
}

// window is the time range the sources are asked about.
type window struct {
	start, end time.Time
}

func windowOf(inc *remediationv1alpha1.Incident, now time.Time) window {
	start := now.Add(-DefaultWindow)
	if fs := inc.Status.FirstSeen; fs != nil {
		start = fs.Add(-windowLead)
	}
	if latest := now.Add(-MinWindow); start.After(latest) {
		start = latest
	}
	if earliest := now.Add(-MaxWindow); start.Before(earliest) {
		start = earliest
	}
	return window{start: start, end: now}
}

// Build assembles the context of inc. It returns an error only when inc is nil or ctx is done: a failed
// source is marked in the context instead (design 3.3).
func (b *Builder) Build(ctx context.Context, inc *remediationv1alpha1.Incident) (*Context, error) {
	if inc == nil {
		return nil, errors.New("building the context: no Incident")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("building the context: %w", err)
	}
	w := windowOf(inc, b.now().UTC())
	ctx = context.WithValue(ctx, buildDeadlineKey{}, time.Now().Add(b.buildTimeout))
	c := &Context{}
	c.Blocks = append(c.Blocks, b.summaryBlock(inc))

	t := b.resolve(ctx, inc.Spec.Subject, w)
	if err := abandoned(ctx); err != nil {
		return nil, err
	}
	for i := range t.pods {
		c.Pods = append(c.Pods, t.pods[i].Name)
	}
	c.Template = templateOf(&t)
	status := b.statusBlock(&t)
	if t.state == targetFailed {
		c.failed(SectionStatus)
	}
	c.Blocks = append(c.Blocks, status)

	logs, failed, err := b.logBlocks(ctx, &t, w)
	if err != nil {
		return nil, err
	}
	if failed {
		c.failed(SectionLogs)
	}
	c.Blocks = append(c.Blocks, logs...)

	metrics, failed, err := b.metricsBlock(ctx, &t, w)
	if err != nil {
		return nil, err
	}
	if failed {
		c.failed(SectionMetrics)
	}
	c.Blocks = append(c.Blocks, metrics)

	c.Blocks = append(c.Blocks, Block{
		Section: SectionGit, Provenance: ProvenanceGit, Availability: NotAvailable,
		Text: "not available: no Git source is configured\n",
	})
	if err := abandoned(ctx); err != nil {
		return nil, err
	}
	b.fitTotal(c)
	return c, nil
}

// buildDeadlineKey carries the deadline of one build in its context. It is a value, not the context's own
// deadline, so that a source call cut short by it is a failed source, while a cancelled or expired context of
// the caller still abandons the build.
type buildDeadlineKey struct{}

// errBuildDeadline is the error of a source call that the build deadline cut short or kept from starting.
var errBuildDeadline = errors.New("the context build ran out of time")

// call runs one source call with the builder's timeout, cut to what is left of the build deadline. Once the
// deadline has passed it does not call the source at all.
func (b *Builder) call(ctx context.Context, f func(ctx context.Context) error) error {
	deadline := time.Now().Add(b.timeout)
	if bd, ok := ctx.Value(buildDeadlineKey{}).(time.Time); ok {
		if !time.Now().Before(bd) {
			return errBuildDeadline
		}
		if bd.Before(deadline) {
			deadline = bd
		}
	}
	cctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return f(cctx)
}

// abandoned returns an error when ctx is done: a source call that failed then was cut short by the caller of
// Build, not by the source, so the build stops instead of marking a source failed.
func abandoned(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("building the context: %w", err)
	}
	return nil
}

// redact makes s valid UTF-8 and redacts it ([Redact]).
func (b *Builder) redact(s string) string { return Redact(b.redactor.String, s) }

// value redacts one value from the cluster for a line of a section ([Value]).
func (b *Builder) value(s string) string { return Value(b.redactor.String, s) }

// finish redacts the assembled text of a section block as a whole, then cuts it to n bytes in whole lines,
// keeping the head: the sections it finishes are written most relevant first. The values the lines recorded
// become the block's quotable values; [Block.Data] keeps those the redaction and the cuts left in the text.
func (b *Builder) finish(blk *Block, l *lines, n int) {
	text, cut := keepHead(b.redact(l.String()), n)
	if cut {
		blk.addLabel(LabelSectionCut)
	}
	blk.Text, blk.Values = text, l.values
}

// cutOrder is the order in which the total bound cuts sections: the attacker-controlled bulk first, the
// summary last.
var cutOrder = []Section{SectionLogs, SectionMetrics, SectionStatus, SectionGit, SectionSummary}

// fitTotal cuts the context to the total bound, latest block of a section first, and marks it truncated
// when it had to. A log block keeps its header and the tail of its excerpt; other blocks keep their head.
func (b *Builder) fitTotal(c *Context) {
	over := c.Size() - b.limits.Total
	if over <= 0 {
		return
	}
	c.Truncated = true
	for _, sec := range cutOrder {
		for i := len(c.Blocks) - 1; i >= 0 && over > 0; i-- {
			blk := &c.Blocks[i]
			if blk.Section != sec || blk.Text == "" {
				continue
			}
			before := len(blk.Text)
			keep := max(0, before-over)
			if sec == SectionLogs {
				head := blk.Text[:blk.headLen]
				body, _ := keepTail(blk.Text[blk.headLen:], keep-len(head))
				if keep < len(head) {
					head, body = "", ""
				}
				blk.Text = head + body
				blk.headLen = len(head)
			} else {
				blk.Text, _ = keepHead(blk.Text, keep)
			}
			over -= before - len(blk.Text)
			blk.addLabel(LabelContextCut)
		}
	}
}
