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

// Package refcheck tells whether a Secret or ConfigMap that a pod references is missing, inferred only from
// what the analyzer already reads: the pod (its spec and status) and the events.k8s.io/v1 Events in its
// namespace (design 10.3). It never reads a Secret or a ConfigMap and needs no permission on either.
//
// The answer is Missing or Unknown, never "exists". Kubernetes RBAC has no metadata-only verb: get or list on
// secrets, also through the metadata API, lets the holder read the data, so a true answer about a Secret's
// existence would need a grant the analyzer must not have (invariant 6). The absence of evidence is not
// evidence of existence either: the kubelet may not have tried yet, the Event may have expired, or the
// reference may be optional.
//
// Missing needs one of two pieces of evidence, both written by the kubelet in fixed shapes:
//
//   - a container (or init container) of the pod waiting with reason CreateContainerConfigError and a message
//     that is exactly the kubelet's error for that container's own non-optional env or envFrom reference: the
//     object not found, or a referenced key not in it;
//   - while the pod is Pending and none of its containers got past volume setup, a Warning FailedMount Event
//     from the kubelet about this pod (same namespace, name and UID) whose note is exactly the kubelet's error
//     for one of the pod's own non-optional secret, configMap or projected volume sources: the object not
//     found. It must be the latest SetUp error of that volume, and the volume must still fail: that Event, or
//     the kubelet's "unmounted volumes" Event listing the volume, was last observed within [EventWindow].
//
// Nothing is parsed out of a message but the list of unmounted volumes, and its names are only compared with
// those of the pod spec.
//
// Untrusted input (invariant 4): the pod status and every Event field are untrusted; anyone who may create
// Events in the namespace can write one. No name is taken from a message. The check builds the exact
// texts the kubelet would write for each reference in the pod spec and compares them with the message, so
// the name, key, container and volume in a [Result] always come from the pod spec, and a crafted message
// cannot make the check name an object the pod does not reference. A crafted Event can still make a
// referenced object look missing; the Result says which source the evidence came from, and a status
// (written by the kubelet through pods/status) is stronger than an Event. No message text is ever returned.
//
// A key missing from a volume's items, a missing image pull Secret and ephemeral containers are not inferred:
// the answer is Unknown.
package refcheck
