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

package keda

import (
	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/kedaready"
	"github.com/pkalin/shadewright/internal/modules"
)

// Name is the module's name in RemediationSettings and its status.
const Name = kedaready.ModuleName

// API groups, versions and kinds the module reads. KEDA 2.x serves ScaledObject, ScaledJob and
// TriggerAuthentication in keda.sh/v1alpha1 only (kedacore/keda v2.21.0 apis/keda/v1alpha1/groupversion_info.go
// and config/crd/bases). The HorizontalPodAutoscaler is the one KEDA creates and owns for each ScaledObject
// (controllers/keda/hpa.go at the same tag), read in autoscaling/v2, which every supported Kubernetes serves.
const (
	Group   = kedaready.Group
	Version = kedaready.Version

	AutoscalingGroup   = "autoscaling"
	AutoscalingVersion = "v2"

	KindScaledObject = kedaready.KindScaledObject
	KindScaledJob    = kedaready.KindScaledJob
	KindHPA          = "HorizontalPodAutoscaler"
)

// module is the KEDA integration. It has no state.
type module struct{}

// Module returns the KEDA integration module, for the list of compiled-in modules.
func Module() modules.Integration { return module{} }

func (module) Name() string { return Name }

// Detection declares keda.sh/v1alpha1, the only group version KEDA 2.x serves.
func (module) Detection() modules.IntegrationDetection {
	return modules.IntegrationDetection{GroupVersions: []string{Group + "/" + Version}}
}

// Hooks contributes the symptom sources for ScaledObject, ScaledJob and the HPAs KEDA owns (whose symptoms are
// about the owning ScaledObject once the evaluator has verified it), the KedaReady
// verification check and the AdjustKedaMaxReplicas action, which has its typed parameters, policy rule, GitOps
// patcher and refusal tests. The policy check refuses the action unless the ScaledObject is ready by the same
// rule as KedaReady (CheckTargetReady in internal/policy), at the decision and before every write. It offers
// no tool.
func (module) Hooks() modules.Hooks {
	return modules.Hooks{
		Symptoms: []modules.SymptomSource{
			{
				Group: Group, Versions: []string{Version}, Kind: KindScaledObject,
				Resource: "scaledobjects", ReportingController: "keda-operator",
				Symptoms: scaledObjectSymptoms,
			},
			{
				Group: Group, Versions: []string{Version}, Kind: KindScaledJob,
				Resource: "scaledjobs", ReportingController: "keda-operator",
				Symptoms: scaledJobSymptoms,
			},
			{
				Group: AutoscalingGroup, Versions: []string{AutoscalingVersion}, Kind: KindHPA,
				Resource: "horizontalpodautoscalers", ReportingController: "horizontal-pod-autoscaler",
				Symptoms: hpaSymptoms,
				// The Incident is about the ScaledObject, the object AdjustKedaMaxReplicas patches (#380).
				Subject: modules.SubjectOwner{
					Group: Group, Version: Version, Kind: KindScaledObject, Resource: "scaledobjects",
					Claims: claimsHPA,
				},
			},
		},
		Actions: []v1alpha1.ActionType{v1alpha1.ActionAdjustKedaMaxReplicas},
		Checks: []modules.VerificationCheck{
			{Name: v1alpha1.CheckKedaReady, Group: Group, Kind: KindScaledObject, Holds: KedaReady},
		},
	}
}
