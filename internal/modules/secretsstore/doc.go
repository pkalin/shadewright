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

// Package secretsstore is the Secrets Store CSI driver cluster integration module (ADR-0018, design 2.5): a pod
// whose secrets volume cannot be mounted, because its SecretProviderClass is missing, the driver or its
// provider is not running on the node, or the provider cannot fetch the objects from the store, becomes a
// symptom that says which of these it is. It diagnoses only and never reads a secret.
//
// Detection: the cluster serves secrets-store.csi.x-k8s.io/v1, read at secrets-store-csi-driver tag v1.6.1
// (apis/v1/doc.go, the +groupName marker; the CRDs in charts/secrets-store-csi-driver/crds serve v1 as the
// storage version and v1alpha1 as deprecated).
//
// No symptom sources. Neither kind of the group holds a status worth watching at v1.6.1:
//
//   - SecretProviderClass: apis/v1/secretproviderclass_types.go declares SecretProviderClassStatus with no
//     fields. Only the deprecated v1alpha1 had status.byPod.
//   - SecretProviderClassPodStatus: apis/v1/secretproviderclasspodstatus_types.go has no condition and no
//     error field, and the driver creates the object only after a mount succeeded, always with
//     status.mounted true (pkg/secrets-store/nodeserver.go NodePublishVolume, the call of
//     createOrUpdateSecretProviderClassPodStatus in pkg/secrets-store/utils.go). A failed mount leaves no such
//     object, so status.mounted false never comes from the driver. Its status.objects lists the ids and
//     versions of the objects mounted: not secrets, but nothing a diagnosis of a failure needs.
//
// So the module watches no kind and needs no permission on either; it never reads a Kubernetes Secret either,
// and adds no permission on Secrets.
//
// Diagnosis rule [RuleMountFailed], on events: the kubelet's Warning event with reason FailedMount about a Pod
// whose note says the Secrets Store CSI driver failed to set up the volume. The note is the kubelet's
// "MountVolume.SetUp failed for volume <name> : <error>" (see [setUpNote] for the sources), and the error
// part is matched against the fixed texts of the driver and of the Azure Key Vault provider to pick one of the
// reasons below. Reasons and summaries are written here, never taken from the cluster; the names read from the
// note (the volume, the SecretProviderClass, the provider, the vault host, the object's type and name, the
// store's error code) and the note itself are evidence, untrusted and copied as they are: the evaluator
// redacts and bounds them. Symptoms meet [modules.ValidSymptom].
//
// The module has no actions and no tools: a secrets manager guards security, so Shadewright explains why a
// secret is missing and never proposes to change a secret, an identity or an access policy (design 2.5).
// Nothing here writes to the cluster or holds a client.
//
// This package imports only the standard library and this module (ADR-0013): no driver or provider API
// package is needed to read an event.
package secretsstore
