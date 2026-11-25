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
	"maps"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/incidents/causereason"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/redact"
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
// StringChecked is String with the flag of a withheld text (false: redaction withheld s whole, #873): the builder
// reads that flag for every section it shows, so a redactor without it, or a wrapper that drops it, does not
// compile (decision of 2026-11-21 on #873).
type Redactor interface {
	String(s string) string
	StringChecked(s string) (string, bool)
}

// contextRedactor is a [Redactor] whose wait for the heap limit of the redactions running at once can end with a
// context (*redact.Redactor's WithContext).
type contextRedactor interface {
	WithContext(ctx context.Context) *redact.Redactor
}

// Config configures a [Builder].
type Config struct {
	// Pods reads pods: the analyzer's cache, which already holds the pods of the watched namespaces (a list in
	// one namespace and a get). It also gets the HPA a ScaledObject subject names, as an unstructured
	// autoscaling/v2 object, the informer the KEDA module's symptom source runs on the same cache. Required.
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
	// Policies names the RemediationPolicy a request of each action type refers to: the analyzer's
	// --action-policies, the same map the analysis loop puts in a request's policyRef.
	Policies map[remediationv1alpha1.ActionType]remediationv1alpha1.Name
	// PolicyReader gets the RemediationPolicies Policies names and the RemediationSettings, one object each by
	// name, from the API server: the analyzer may get those policies, not list or watch them. Nil means the
	// policy bounds section is "not available".
	PolicyReader client.Reader
	// RequestNamespace is the namespace the analyzer creates requests in, the operator namespace: a policy's
	// requestNamespaces must list it. Required with PolicyReader.
	RequestNamespace string
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
	pods, objects    client.Reader
	namespaces       map[string]struct{}
	policies         map[remediationv1alpha1.ActionType]remediationv1alpha1.Name
	policyReader     client.Reader
	requestNamespace string
	metrics          providers.MetricsSource
	logs             providers.LogSource
	redactor         Redactor
	limits           Limits
	timeout          time.Duration
	buildTimeout     time.Duration
	now              func() time.Time
	// showsTally, set by tests only, replaces the list of reasons whose summary shows the absorbed tally. Nil,
	// as New leaves it, is that list: causereason.Tallied (tallyShown).
	showsTally func(reason string) bool
}

// New returns a builder, or an error when a required field is missing or a bound is out of range.
func New(cfg Config) (*Builder, error) {
	if cfg.Pods == nil || cfg.Objects == nil {
		return nil, errors.New("context builder: a pod reader and an object reader are required")
	}
	if cfg.Redactor == nil {
		return nil, errors.New("context builder: a redactor is required")
	}
	if cfg.PolicyReader != nil && cfg.RequestNamespace == "" {
		return nil, errors.New("context builder: a policy reader needs the request namespace")
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
		policies: maps.Clone(cfg.Policies), policyReader: cfg.PolicyReader, requestNamespace: cfg.RequestNamespace,
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

// Build assembles the context of inc within the configured bounds. It returns an error only when inc is nil
// or ctx is done: a failed source is marked in the context instead (design 3.3).
func (b *Builder) Build(ctx context.Context, inc *remediationv1alpha1.Incident) (*Context, error) {
	return b.BuildWithin(ctx, inc, 0)
}

// BuildWithin assembles the context of inc as [Builder.Build] does, with total in place of the configured
// [Limits.Total]: the analysis passes maxContextBytes of RemediationSettings (#981), which LimitsOf never
// leaves at zero. Zero keeps the configured total: [Builder.Build] passes it, and so do callers that set
// the total in the builder's own [Limits] (the evaluation harness, whose analysis.Limits leave it unset).
// The section text bounds stay as configured. Wrapping and escaping can take the rendered message above
// their sum, so a higher total can retain more of that text. A nonzero total outside
// [MinTotalBytes] to [MaxLimitBytes] is an error, before any read.
func (b *Builder) BuildWithin(ctx context.Context, inc *remediationv1alpha1.Incident, total int) (*Context, error) {
	if total == 0 || total == b.limits.Total {
		return b.build(ctx, inc)
	}
	if total < MinTotalBytes || total > MaxLimitBytes {
		return nil, fmt.Errorf("building the context: context bound %d is outside %d to %d bytes", total,
			MinTotalBytes, MaxLimitBytes)
	}
	within := *b
	within.limits.Total = total
	return within.build(ctx, inc)
}

func (b *Builder) build(ctx context.Context, inc *remediationv1alpha1.Incident) (*Context, error) {
	if inc == nil {
		return nil, errors.New("building the context: no Incident")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("building the context: %w", err)
	}
	w := windowOf(inc, b.now().UTC())
	deadline := time.Now().Add(b.buildTimeout)
	ctx = context.WithValue(ctx, buildDeadlineKey{}, deadline)
	if cr, ok := b.redactor.(contextRedactor); ok {
		// The redactions of this build wait for the heap limit of the redactions at once only as long as the build
		// may run (redact.Redactor.WithContext): their context has the build deadline as its own deadline, so a
		// wait past it ends and its block is withheld, a failed source. ctx keeps the deadline as a value only, so
		// the build itself is not abandoned by it.
		rctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		within := *b
		within.redactor = cr.WithContext(rctx)
		b = &within
	}
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
	c.ScaledObject = scaledObjectOf(&t)
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
	policyBlk, failed := b.policyBlock(ctx, inc, &t)
	if failed {
		c.failed(SectionPolicy)
	}
	c.Blocks = append(c.Blocks, policyBlk)
	if err := abandoned(ctx); err != nil {
		return nil, err
	}
	// A block that could not fetch is a failed source whatever made it so: a block redaction withheld too.
	for _, blk := range c.Blocks {
		if blk.Availability == CouldNotFetch {
			c.failed(blk.Section)
		}
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

// withBudget gives the source calls made with the returned context a deadline d from now, shared by all of
// them: a section's time budget. A build deadline that comes earlier stays.
func withBudget(ctx context.Context, d time.Duration) context.Context {
	deadline := time.Now().Add(d)
	if bd, ok := ctx.Value(buildDeadlineKey{}).(time.Time); ok && bd.Before(deadline) {
		return ctx
	}
	return context.WithValue(ctx, buildDeadlineKey{}, deadline)
}

// outOfTime reports whether the deadline of the source calls made with ctx, the build's or a section's, has
// passed.
func outOfTime(ctx context.Context) bool {
	bd, ok := ctx.Value(buildDeadlineKey{}).(time.Time)
	return ok && !time.Now().Before(bd)
}

// abandoned returns an error when ctx is done: a source call that failed then was cut short by the caller of
// Build, not by the source, so the build stops instead of marking a source failed.
func abandoned(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("building the context: %w", err)
	}
	return nil
}

// overLargestEntry reports whether an entry of size bytes, a section's pieces or a text the build would pass to
// the redactor, is larger than the largest entry the heap of one redaction was measured for (redact.LargestEntry,
// about 1.5 MiB: the largest object), and counts it as withheld with the reason size (#1106, decision of
// 2026-11-22). Its caller withholds the entry whole, so the block is could not fetch and its section a failed
// source, as for any entry redaction withholds. The status section is the one that can pass it: it holds the
// messages of up to MaxPods pods, each an object of up to that size, and no bound of the builder applies before the
// decision (TestStatusSectionOverTheLargestEntryIsWithheld). redact.MaxEntry stays the last check in the redactor.
func overLargestEntry(size int) bool { return redact.OverLargestEntry(size) }

// redact makes s valid UTF-8 and redacts it ([Redact]).
func (b *Builder) redact(s string) string { return Redact(Twice(b.redactor), s) }

// value returns s, a value from the cluster, as a fragment of a section: it is decided with the section
// ([decide]).
func (b *Builder) value(s string) frag { return val(s) }

// shown redacts one value from the cluster for a text that is not written as pieces ([Value]).
func (b *Builder) shown(s string) string { return Value(Twice(b.redactor), s) }

// finish decides the pieces of a section block once ([decide]), then cuts the text to n bytes in whole lines,
// keeping the head: the sections it finishes are written most relevant first. The values the lines recorded
// become the block's quotable values; [Block.Data] keeps those the redaction and the cuts left in the text.
//
// When redaction withholds the section whole, the block holds a fixed text and no values, could not fetch, and the
// build counts its source as failed (Build).
func (b *Builder) finish(blk *Block, l *lines, n int) {
	decided, values, withheld := decide(b.redactor, l.ps, l.quotes)
	if withheld {
		withholdBlock(blk)
		return
	}
	text, cut := keepHead(decided, n)
	if cut {
		blk.addLabel(LabelSectionCut)
	}
	blk.Text, blk.Values = text, values
}

// withheldText is the text of a block redaction withheld (LabelWithheld).
const withheldText = "could not fetch: withheld by redaction\n"

// withholdBlock makes blk a block redaction withheld whole: could not fetch, a fixed text, no values.
func withholdBlock(blk *Block) {
	blk.Availability, blk.Text, blk.Values, blk.headLen = CouldNotFetch, withheldText, nil, 0
	blk.requiredText = withheldText
	blk.addLabel(LabelWithheld)
}

// cutOrder is the order in which the total bound cuts sections: the attacker-controlled bulk first, the
// summary last.
var cutOrder = []Section{SectionLogs, SectionMetrics, SectionStatus, SectionGit, SectionPolicy, SectionSummary}

func (b *Builder) fitTotal(c *Context) {
	c.totalLimit = b.limits.Total
	c.fitTotal(b.limits.Total)
}

// ReserveMessageBytes fits the context within the total bound after reserving room for fixedBytes and up to
// optionalBytes of appended text. It returns the optional bytes reserved, or -1 if the required text alone
// cannot fit. Zero uses the total from the build. Cutting the context or the optional text marks it truncated.
func (c *Context) ReserveMessageBytes(total, fixedBytes, optionalBytes int) int {
	if total == 0 {
		total = c.totalLimit
		if total == 0 {
			total = DefaultTotalBytes
		}
	}
	if c.Size()+fixedBytes+optionalBytes <= total {
		return optionalBytes
	}
	// Allow for the cut labels as well as the text that must survive a cut.
	minimum := len(ContextPreamble)
	for i, blk := range c.Blocks {
		if blk.Text == blk.requiredText {
			minimum += renderedBlockLen(i, blk)
		} else {
			minimum += blockWrapperLenWithCutLabel(i, blk) + len(renderBody(blk.requiredText))
		}
	}
	room := total - fixedBytes - minimum
	if room < 0 {
		return -1
	}
	reserved := min(optionalBytes, room)
	if !c.fitTotal(total - fixedBytes - reserved) {
		return -1
	}
	c.Truncated = true
	return reserved
}

// fitTotal cuts the rendered message in section order, latest block first, keeping any redaction notice.
// A log block keeps its header and the tail of its excerpt when they fit. Other sections keep their head.
func (c *Context) fitTotal(limit int) bool {
	rendered := make([]int, len(c.Blocks))
	total := len(ContextPreamble)
	for i, blk := range c.Blocks {
		rendered[i] = renderedBlockLen(i, blk)
		total += rendered[i]
	}
	if total <= limit {
		return true
	}
	c.Truncated = true
	for _, sec := range cutOrder {
		for i := len(c.Blocks) - 1; i >= 0 && total > limit; i-- {
			blk := &c.Blocks[i]
			if blk.Section != sec || blk.Text == blk.requiredText {
				continue
			}
			before := len(blk.Text)
			others := total - rendered[i]
			wrapper := blockWrapperLenWithCutLabel(i, *blk)
			budget := limit - others - wrapper
			if sec == SectionLogs {
				head := blk.Text[:blk.headLen]
				if budget <= 0 || len(renderBody(head)) > budget {
					blk.Text = blk.requiredText
					blk.headLen = len(blk.Text)
				} else {
					body := keepTailRendered(head, blk.Text[blk.headLen:], budget)
					blk.Text = head + body
					blk.headLen = len(head)
				}
			} else {
				blk.Text = keepHeadRendered(blk.Text, budget)
				// A cut that leaves part of the unit at the end removes all of it.
				if blk.unitAt > 0 && len(blk.Text) > blk.unitAt && len(blk.Text) < before {
					blk.Text = blk.Text[:blk.unitAt]
					if len(renderBody(blk.Text)) > budget {
						blk.Text = keepHeadRendered(blk.Text, budget)
					}
				}
				if blk.unitAt >= len(blk.Text) {
					blk.unitAt = 0
				}
			}
			blk.addLabel(LabelContextCut)
			next := renderedBlockLen(i, *blk)
			total += next - rendered[i]
			rendered[i] = next
		}
	}
	return total <= limit
}

// tallyShown reports whether the summary of an Incident of reason shows its absorbed tally (ADR-0039 point 18,
// decided 2026-11-21 on #432): a reason of causereason.Tallied, an OOM kill and the waiting reasons the collector
// reads as a cause; a non-zero exit is not on it until #1080. A Builder not made by New (showsTally nil) reads the
// same list, so no Builder hides the tally of those reasons from the model.
func (b *Builder) tallyShown(reason string) bool {
	if b.showsTally != nil {
		return b.showsTally(reason)
	}
	return causereason.ShowsTally(reason)
}
