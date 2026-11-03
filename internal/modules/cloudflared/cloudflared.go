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

package cloudflared

import "github.com/pkalin/shadewright/internal/modules"

// Name is the module's name in RemediationSettings and its status.
const Name = "cloudflared"

// The kind the module reads: core v1 Pods, which every cluster serves. cloudflared has no API of its own.
const (
	CoreVersion = "v1"
	KindPod     = "Pod"
)

// reportingKubelet is the component that writes pod status. It is the reporting controller the collector
// gives its own observations read from pod status too.
const reportingKubelet = "kubelet"

// module is the cloudflared integration. It has no state.
type module struct{}

// Module returns the cloudflared integration module, for the list of compiled-in modules.
func Module() modules.Integration { return module{} }

func (module) Name() string { return Name }

// Detection declares the core group version v1, the one of the Pods the module reads: cloudflared has no API
// group, so there is nothing more specific to detect through discovery. The module is therefore detected on
// every cluster, detection does not narrow it, and it is active wherever the cluster admin enables it
// (ADR-0018 amendment of 2026-11-04, [modules.CoreOnly]: such a module may only diagnose). The symptom source
// recognises cloudflared pod by pod (package documentation).
func (module) Detection() modules.IntegrationDetection {
	return modules.IntegrationDetection{GroupVersions: []string{CoreVersion}}
}

// Hooks contributes the Pod symptom source and nothing else: no rules, tools, actions or checks. The module
// diagnoses only (design 2.5).
func (module) Hooks() modules.Hooks {
	return modules.Hooks{
		Symptoms: []modules.SymptomSource{{
			Group: "", Versions: []string{CoreVersion}, Kind: KindPod,
			Resource: "pods", ReportingController: reportingKubelet,
			Symptoms: podSymptoms,
		}},
	}
}
