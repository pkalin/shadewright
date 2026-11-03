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

package modules

import (
	"slices"
	"strings"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

// Enabled is what the cluster admin enabled in RemediationSettings spec.modules.
type Enabled struct {
	// Platforms are the platform names enabled besides generic.
	Platforms []string
	// Integrations are the integration names enabled.
	Integrations []string
}

// Detection is what was detected on the cluster: the result of [Registry.DetectPlatform] and
// [Registry.DetectIntegrations]. The zero value is nothing detected.
type Detection struct {
	// Platform is the detected platform; nil or generic when no platform claims the cluster.
	Platform Platform
	// Integrations are the detected integrations.
	Integrations []DetectedIntegration
}

// Detect runs both detections on input the caller collected.
func (r *Registry) Detect(nodes []Node, groupVersions []string) Detection {
	return Detection{Platform: r.DetectPlatform(nodes), Integrations: r.DetectIntegrations(groupVersions)}
}

// IntegrationState is one integration that is enabled, detected or both.
type IntegrationState struct {
	Name     string
	Detected bool
	// Version is the group version that detected the add-on; "" when it is not detected.
	Version string
	// Active is true when the integration is enabled, compiled in and detected.
	Active bool
}

// ActiveModule is a module that contributes its hooks.
type ActiveModule struct {
	Name  string
	Hooks Hooks
}

// Selection is which modules are active: worked out from what is enabled and what is detected, never from a
// status written earlier. A module is active only when it is enabled, compiled in and detected; detection
// alone never makes it active. The generic platform is always active, also next to another platform: its
// rules hold on every cluster. Build one with [Registry.Select]; the zero value has no module at all.
type Selection struct {
	detected     string // the detected platform, "" for generic
	active       platformEntry
	generic      platformEntry
	integrations []IntegrationState
	activeInts   []integrationEntry
	unknownPlat  []string
	unknownInt   []string
	// activeList is Active, worked out once by Select.
	activeList []ActiveModule
}

// Select works out the active modules. Names in enabled that no compiled-in module has are reported by
// [Selection.Unknown] and contribute nothing. Repeated names count once, and only the first [MaxPlatforms]
// platform and [MaxIntegrations] integration names are read, the caps of the RemediationSettings schema.
func (r *Registry) Select(enabled Enabled, d Detection) Selection {
	s := Selection{generic: r.platformByName[GenericPlatform]}
	s.active = s.generic

	platforms := uniqueCapped(enabled.Platforms, MaxPlatforms)
	for _, name := range platforms {
		if _, ok := r.platformByName[name]; !ok {
			s.unknownPlat = append(s.unknownPlat, name)
		}
	}
	if d.Platform != nil {
		if e, ok := r.platformByName[d.Platform.Name()]; ok && e.name != GenericPlatform {
			s.detected = e.name
			if slices.Contains(platforms, e.name) {
				s.active = e
			}
		}
	}

	ints := uniqueCapped(enabled.Integrations, MaxIntegrations)
	detected := map[string]string{}
	for _, di := range d.Integrations {
		if di.Integration == nil {
			continue
		}
		if _, ok := r.integrationByName[di.Integration.Name()]; ok {
			detected[di.Integration.Name()] = di.GroupVersion
		}
	}
	names := slices.Clone(ints)
	for name := range detected {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		e, known := r.integrationByName[name]
		if !known {
			s.unknownInt = append(s.unknownInt, name)
		}
		version, isDetected := detected[name]
		st := IntegrationState{Name: name, Detected: isDetected, Version: version}
		st.Active = known && isDetected && slices.Contains(ints, name)
		if st.Active {
			s.activeInts = append(s.activeInts, e)
		}
		s.integrations = append(s.integrations, st)
	}
	slices.Sort(s.unknownPlat)
	s.activeList = s.buildActive()
	return s
}

// uniqueCapped returns the first n distinct names.
func uniqueCapped(names []string, n int) []string {
	var out []string
	for _, name := range names {
		if len(out) == n {
			break
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// DetectedPlatform returns the detected platform's name, "" when no platform claims the cluster.
func (s Selection) DetectedPlatform() string { return s.detected }

// ActivePlatform returns the name of the platform whose module is used: the detected one when it is enabled,
// generic otherwise.
func (s Selection) ActivePlatform() string { return s.active.name }

// Integrations returns every integration that is enabled or detected, sorted by name.
func (s Selection) Integrations() []IntegrationState { return slices.Clone(s.integrations) }

// Unknown returns the enabled names that no compiled-in module has, sorted, per axis.
func (s Selection) Unknown() (platforms, integrations []string) {
	return slices.Clone(s.unknownPlat), slices.Clone(s.unknownInt)
}

// Active returns the modules that contribute hooks: generic, then the active platform when it is another
// one, then the active integrations by name.
func (s Selection) Active() []ActiveModule { return slices.Clone(s.activeList) }

// buildActive lists the active modules, for Select.
func (s Selection) buildActive() []ActiveModule {
	var out []ActiveModule
	if s.generic.module != nil {
		out = append(out, ActiveModule{Name: s.generic.name, Hooks: s.generic.hooks})
	}
	if s.active.module != nil && s.active.name != GenericPlatform {
		out = append(out, ActiveModule{Name: s.active.name, Hooks: s.active.hooks})
	}
	for _, e := range s.activeInts {
		out = append(out, ActiveModule{Name: e.name, Hooks: e.hooks})
	}
	return out
}

// EvaluateEvent runs the event rules of every active module on one event and returns what they found, with
// evidence redacted and bounded.
func (s Selection) EvaluateEvent(ev Event) []Finding {
	out := make([]Finding, 0, len(s.activeList))
	for _, m := range s.activeList {
		out = append(out, evaluateEvent(m.Name, m.Hooks.Rules, ev)...)
	}
	return out
}

// EvaluateNode runs the node rules of every active module on one node.
func (s Selection) EvaluateNode(n NodeState) []Finding {
	out := make([]Finding, 0, len(s.activeList))
	for _, m := range s.activeList {
		out = append(out, evaluateNode(m.Name, m.Hooks.Rules, n)...)
	}
	return out
}

// PlatformStatus returns the selection as RemediationSettings status.platform.
func (s Selection) PlatformStatus() v1alpha1.PlatformStatus {
	active := s.active.name
	if active == "" {
		active = GenericPlatform
	}
	return v1alpha1.PlatformStatus{Detected: v1alpha1.ModuleName(s.detected), Active: v1alpha1.ModuleName(active)}
}

// IntegrationStatus returns the selection as RemediationSettings status.integrations, sorted by name.
func (s Selection) IntegrationStatus() []v1alpha1.IntegrationStatus {
	if len(s.integrations) == 0 {
		return nil
	}
	out := make([]v1alpha1.IntegrationStatus, 0, len(s.integrations))
	for _, st := range s.integrations {
		out = append(out, v1alpha1.IntegrationStatus{
			Name: v1alpha1.ModuleName(st.Name), Detected: st.Detected, Version: st.Version, Active: st.Active,
		})
	}
	return out
}

// UnknownMessage describes the unknown names for a status condition, "" when there are none.
func (s Selection) UnknownMessage() string {
	var parts []string
	if len(s.unknownPlat) > 0 {
		parts = append(parts, "platforms "+strings.Join(s.unknownPlat, ", "))
	}
	if len(s.unknownInt) > 0 {
		parts = append(parts, "integrations "+strings.Join(s.unknownInt, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	msg := "Enabled modules that this analyzer does not have, which contribute nothing: " + strings.Join(parts, "; ")
	if len(msg) > MaxUnknownMessage {
		// Names are ASCII (the schema's pattern), so a byte cut keeps the text valid.
		msg = msg[:MaxUnknownMessage-len(truncatedSuffix)] + truncatedSuffix
	}
	return msg
}

// MaxUnknownMessage is the longest message [Selection.UnknownMessage] returns, in bytes.
const MaxUnknownMessage = 1024

const truncatedSuffix = " (and more)"
