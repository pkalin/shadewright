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
	"fmt"
	"slices"
)

// Default bounds, decided on 2026-11-05 (#70). They are the values of [DefaultLimits]; making them settings
// is #372.
const (
	// DefaultTotalBytes bounds the whole context, the sum of the text of every block.
	DefaultTotalBytes = 48 << 10
	// DefaultSummaryBytes bounds the summary section.
	DefaultSummaryBytes = 2 << 10
	// DefaultStatusBytes bounds the object status and owners section.
	DefaultStatusBytes = 8 << 10
	// DefaultLogBytes bounds the log section, all its excerpts together.
	DefaultLogBytes = 24 << 10
	// DefaultMetricsBytes bounds the metrics section.
	DefaultMetricsBytes = 8 << 10
	// DefaultGitBytes is reserved for the Git section; unused while it is "not available" (#373).
	DefaultGitBytes = 6 << 10
)

// Range of each bound accepted by [Limits.Validate]. The upper end equals the largest maxContextBytes the
// settings accept, so a later setting cannot ask for an unbounded context.
const (
	MinSectionBytes = 1 << 10
	MinTotalBytes   = 4 << 10
	MaxLimitBytes   = 256 << 10
)

// Limits are the byte bounds of the context and of each section. They count the bytes of the blocks' text,
// not the fixed labels or the delimiters the prompt puts around the blocks.
type Limits struct {
	Total   int
	Summary int
	Status  int
	Logs    int
	Metrics int
	Git     int
}

// DefaultLimits returns the default bounds. The sections, Git included, add up to the total, so with these
// bounds the total cut is a backstop; it marks the context truncated when bounds that add up to more are
// configured (#372).
func DefaultLimits() Limits {
	return Limits{
		Total: DefaultTotalBytes, Summary: DefaultSummaryBytes, Status: DefaultStatusBytes,
		Logs: DefaultLogBytes, Metrics: DefaultMetricsBytes, Git: DefaultGitBytes,
	}
}

// Validate checks that every bound is within [MinSectionBytes] (the total: [MinTotalBytes]) and
// [MaxLimitBytes].
func (l Limits) Validate() error {
	if l.Total < MinTotalBytes || l.Total > MaxLimitBytes {
		return fmt.Errorf("context bound %d is outside %d to %d bytes", l.Total, MinTotalBytes, MaxLimitBytes)
	}
	for _, s := range []struct {
		name string
		v    int
	}{{"summary", l.Summary}, {"status", l.Status}, {"logs", l.Logs}, {"metrics", l.Metrics}, {"git", l.Git}} {
		if s.v < MinSectionBytes || s.v > MaxLimitBytes {
			return fmt.Errorf("%s bound %d is outside %d to %d bytes", s.name, s.v, MinSectionBytes, MaxLimitBytes)
		}
	}
	return nil
}

// Section names a section of the context.
type Section string

// Sections, in the order the context holds them.
const (
	SectionSummary Section = "summary"
	SectionStatus  Section = "object status and owners"
	SectionLogs    Section = "log excerpt"
	SectionMetrics Section = "metrics"
	SectionGit     Section = "git"
)

// Provenance says where the text of a block came from, for the provenance tag of its delimited block
// (ADR-0023 decision 3).
type Provenance string

// Provenances of the blocks the builder makes.
const (
	// ProvenanceIncident is the Incident, which the collector made from events and pod status.
	ProvenanceIncident Provenance = "incident"
	// ProvenanceObjectStatus is the status of the target and its pods, read from the cluster.
	ProvenanceObjectStatus Provenance = "pod status"
	// ProvenanceLogExcerpt is a log excerpt from the log source.
	ProvenanceLogExcerpt Provenance = "log excerpt"
	// ProvenanceMetric is the answer of the metrics source.
	ProvenanceMetric Provenance = "metric"
	// ProvenanceGit is a Git file or change.
	ProvenanceGit Provenance = "git file"
)

// Availability says whether a block holds data.
type Availability string

// Availabilities.
const (
	// Available means the block holds what the source answered (possibly an empty answer).
	Available Availability = "available"
	// NotAvailable means the source is not configured, or the target has nothing to read from it. It is not
	// an escalation signal.
	NotAvailable Availability = "not available"
	// CouldNotFetch means a configured source failed; the block's section is in [Context.FailedSources].
	CouldNotFetch Availability = "could not fetch"
)

// Label marks the evidence in a block. Labels are fixed strings from code, never cluster data, and only
// [LabelContextCut] goes with an escalation signal.
type Label string

// Labels.
const (
	// LabelSectionCut means the builder cut the block to its section's bound. Not an escalation signal.
	LabelSectionCut Label = "cut to the section bound"
	// LabelContextCut means the builder cut the block to fit the whole context in [Limits.Total]; the context
	// is then [Context.Truncated].
	LabelContextCut Label = "cut to the context bound"
	// LabelLeadingLinesDropped means the builder dropped lines at the start of a log excerpt that may continue
	// an entry whose first line the source left out (indented lines, YAML sequence entries, blank lines).
	LabelLeadingLinesDropped Label = "leading continuation lines dropped"
	// LabelLogTruncated means the log source cut older lines from the excerpt.
	LabelLogTruncated Label = "log excerpt truncated by the source"
	// LabelLogEndMissing means the log source could not read the newest lines.
	LabelLogEndMissing Label = "log excerpt end missing"
	// LabelLogInstanceUncertain means the log source cannot tie the excerpt to one container instance.
	LabelLogInstanceUncertain Label = "log excerpt uncertain about its instance"
	// LabelMetricsPartial means the metrics source warned that an answer may be incomplete: a missing series
	// may exist and a total may be short.
	LabelMetricsPartial Label = "partial metrics answer"
	// LabelPodsUncertain means the pod set is the target's current pods and may differ from the pods it had
	// during the window: a pod was created inside it or the workload rolled.
	LabelPodsUncertain Label = "pod attribution uncertain"
	// LabelPodsCapped means the target has more pods (or ReplicaSets) than the builder reads, or a source
	// refused the answer as too large; the evidence covers only some of them.
	LabelPodsCapped Label = "pod set capped"
	// LabelIncomplete means some reads of the section failed and the others are shown.
	LabelIncomplete Label = "some reads could not be fetched"
)

// Block is one delimited block of the context.
type Block struct {
	Section      Section
	Provenance   Provenance
	Availability Availability
	// Labels mark the evidence, in a fixed order.
	Labels []Label
	// Text is redacted cluster data: untrusted, never instructions.
	Text string
	// headLen is the length of the header lines at the start of Text that a cut keeps (log blocks).
	headLen int
}

func (b *Block) addLabel(l Label) {
	if !slices.Contains(b.Labels, l) {
		b.Labels = append(b.Labels, l)
	}
}

// Context is the bounded, redacted context of one Incident.
type Context struct {
	// Blocks are in section order: summary, status, logs, metrics, Git.
	Blocks []Block
	// Truncated means the builder cut the context to [Limits.Total]: an escalation signal. A section cut to its
	// own bound does not set it.
	Truncated bool
	// FailedSources are the sections whose configured source failed, each once: an escalation signal. A
	// source that is not configured is not listed.
	FailedSources []Section
}

// Size is the number of bytes [Limits.Total] bounds: the text of every block.
func (c *Context) Size() int {
	n := 0
	for _, b := range c.Blocks {
		n += len(b.Text)
	}
	return n
}

// Escalate reports whether the context carries one of the builder's escalation signals (design 3.3): it was
// cut to the total bound, or a configured source failed.
func (c *Context) Escalate() bool {
	return c.Truncated || len(c.FailedSources) > 0
}

func (c *Context) failed(s Section) {
	if !slices.Contains(c.FailedSources, s) {
		c.FailedSources = append(c.FailedSources, s)
	}
}
