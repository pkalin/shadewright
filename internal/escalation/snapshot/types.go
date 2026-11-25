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

// Package snapshot encodes the version 1 application-code handoff in ADR-0049.
package snapshot

import "time"

// Mapping contains the two aliases the installation approved for this container.
type Mapping struct {
	RepositoryAlias string
	ContainerAlias  string
}

// WorkloadKind is one of the workload kinds eligible for handoff.
type WorkloadKind string

// The selection rules support these workload kinds.
const (
	Deployment  WorkloadKind = "Deployment"
	StatefulSet WorkloadKind = "StatefulSet"
)

// Workload identifies the affected workload.
type Workload struct {
	Kind      WorkloadKind `json:"kind"`
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
}

// Reason identifies a qualifying failure observed for the container.
type Reason string

// These are the three observation reasons in ADR-0049 decision 1.
const (
	NonzeroExit      Reason = "NonzeroExit"
	ContainerBackoff Reason = "ContainerBackoff"
	FailedReadiness  Reason = "FailedReadiness"
)

func (r Reason) valid() bool {
	return r == NonzeroExit || r == ContainerBackoff || r == FailedReadiness
}

// RuleID identifies a check the selector reports as matched.
type RuleID string

// Version 1 has eight rule IDs.
const (
	AnalysisEscalated      RuleID = "analysis-escalated"
	NoRequest              RuleID = "no-request"
	WorkloadMapped         RuleID = "workload-mapped"
	PolicySourceExcluded   RuleID = "policy-source-excluded"
	InfrastructureExcluded RuleID = "infrastructure-excluded"
	DistinctObservations   RuleID = "distinct-observations"
	CurrentFailure         RuleID = "current-failure"
	CheckedLogs            RuleID = "checked-logs"
)

// Observation supplies the facts for one code-written evidence line.
type Observation struct {
	Reason     Reason
	ObservedAt time.Time
}

// Excerpt holds one log excerpt and the collector's metadata. At least one
// non-optional excerpt must have usable text. Withheld discards Text even when set.
type Excerpt struct {
	CollectedAt time.Time
	Text        string
	Cut         bool
	Withheld    bool
	Optional    bool
}

// Candidate holds the untrusted fields of an already selected incident. All
// observations and excerpts must come from the container identified by Mapping.
type Candidate struct {
	IncidentUID    string
	Fingerprint    string
	Reason         Reason
	Count          int64
	FirstSeen      time.Time
	LastSeen       time.Time
	Workload       Workload
	ImageDigest    string
	MatchedRuleIDs []RuleID
	Evidence       []Observation
	Excerpts       []Excerpt
	ModelHint      string
}

type payload struct {
	SchemaVersion   int       `json:"schemaVersion"`
	Verdict         string    `json:"verdict"`
	RuleVersion     int       `json:"ruleVersion"`
	RepositoryAlias string    `json:"repositoryAlias"`
	Untrusted       untrusted `json:"untrusted"`
}

type untrusted struct {
	IncidentUID    string        `json:"incidentUID"`
	Fingerprint    string        `json:"fingerprint"`
	Reason         Reason        `json:"reason"`
	Count          int64         `json:"count"`
	FirstSeen      string        `json:"firstSeen"`
	LastSeen       string        `json:"lastSeen"`
	Workload       Workload      `json:"workload"`
	ContainerAlias string        `json:"containerAlias"`
	ImageDigest    string        `json:"imageDigest,omitempty"`
	MatchedRuleIDs []RuleID      `json:"matchedRuleIDs"`
	Evidence       []string      `json:"evidence"`
	Excerpts       []wireExcerpt `json:"excerpts"`
	ModelHint      string        `json:"modelHint,omitempty"`
}

type wireExcerpt struct {
	ContainerAlias string `json:"containerAlias"`
	CollectedAt    string `json:"collectedAt,omitempty"`
	Cut            bool   `json:"cut"`
	Withheld       bool   `json:"withheld"`
	Text           string `json:"text,omitempty"`
}
