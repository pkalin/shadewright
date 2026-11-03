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

package secretsstore

import (
	"regexp"
	"strings"

	"github.com/pkalin/shadewright/internal/modules"
)

// RuleMountFailed is the name of the diagnosis rule for a secrets volume the kubelet could not set up.
const RuleMountFailed = "mount-failed"

// Symptom reasons of the rule.
const (
	// ReasonSecretProviderClassNotFound: the SecretProviderClass the volume names does not exist in the pod's
	// namespace. The driver's own name for this error (pkg/errors/errors.go SecretProviderClassNotFound).
	ReasonSecretProviderClassNotFound = "SecretProviderClassNotFound"
	// ReasonDriverNotRegistered: the driver is not registered with the kubelet on the pod's node.
	ReasonDriverNotRegistered = "SecretsStoreDriverNotRegistered"
	// ReasonProviderUnavailable: the driver could not reach the provider the SecretProviderClass names.
	ReasonProviderUnavailable = "SecretsStoreProviderUnavailable"
	// ReasonKeyVaultIdentityFailed: the Azure Key Vault provider could not get a token for its identity.
	ReasonKeyVaultIdentityFailed = "KeyVaultIdentityFailed"
	// ReasonKeyVaultAccessDenied: Key Vault answered 403 for the identity.
	ReasonKeyVaultAccessDenied = "KeyVaultAccessDenied"
	// ReasonKeyVaultNotFound: the vault's host name does not resolve.
	ReasonKeyVaultNotFound = "KeyVaultNotFound"
	// ReasonKeyVaultObjectNotFound: the vault has no object of the name or version asked for.
	ReasonKeyVaultObjectNotFound = "KeyVaultObjectNotFound"
	// ReasonProviderFailed: a provider error that none of the reasons above recognises.
	ReasonProviderFailed = "SecretsStoreProviderFailed"
	// ReasonMountFailed: a driver error that none of the reasons above recognises.
	ReasonMountFailed = "SecretsStoreMountFailed"
)

// Fixed summaries, one per reason. Each is written here, never taken from the cluster, and none suggests
// changing a secret, an identity or an access policy.
var summaries = map[string]string{
	ReasonSecretProviderClassNotFound: "The pod's secrets volume cannot be mounted: the Secrets Store CSI driver found no SecretProviderClass of the name the volume gives in the pod's namespace.",
	ReasonDriverNotRegistered:         "The pod's secrets volume cannot be mounted: the Secrets Store CSI driver is not registered with the kubelet on the pod's node.",
	ReasonProviderUnavailable:         "The pod's secrets volume cannot be mounted: the Secrets Store CSI driver could not reach the provider its SecretProviderClass names on the pod's node.",
	ReasonKeyVaultIdentityFailed:      "The pod's secrets volume cannot be mounted: the Azure Key Vault provider could not get a token for the identity of the SecretProviderClass (workload identity, managed identity or client credentials).",
	ReasonKeyVaultAccessDenied:        "The pod's secrets volume cannot be mounted: Azure Key Vault answered 403 Forbidden to the identity of the SecretProviderClass for an object.",
	ReasonKeyVaultNotFound:            "The pod's secrets volume cannot be mounted: the host name of the key vault the SecretProviderClass names does not resolve.",
	ReasonKeyVaultObjectNotFound:      "The pod's secrets volume cannot be mounted: the key vault has no secret, key or certificate of the name or version the SecretProviderClass asks for.",
	ReasonProviderFailed:              "The pod's secrets volume cannot be mounted: the secrets store provider failed to fetch the objects of the SecretProviderClass.",
	ReasonMountFailed:                 "The pod's secrets volume cannot be mounted: the Secrets Store CSI driver failed to set it up.",
}

// The event the kubelet writes when it cannot set up a volume (kubernetes v1.37.2):
// pkg/volume/util/operationexecutor/operation_generator.go GenerateMountVolumeFunc records
// kevents.FailedMountVolume, "FailedMount" (pkg/kubelet/events/event.go), as a Warning about the pod, with the
// recorder of cmd/kubelet/app/server.go, whose component server.ComponentKubelet ("kubelet",
// pkg/kubelet/server/server.go) client-go's tools/record/event.go writes as reportingController. A core event
// about a namespaced object sits in that object's namespace (pkg/apis/core/validation/events.go).
const (
	eventReasonFailedMount = "FailedMount"
	eventTypeWarning       = "Warning"
	reportingKubelet       = "kubelet"
	kindPod                = "Pod"
)

var (
	// setUpNote matches the start of the note: VolumeToMount.GenerateError("MountVolume.SetUp failed", err)
	// in pkg/volume/util/operationexecutor/operation_executor.go builds `<prefix> for volume "<name>" : <err>`
	// (generateVolumeMsg and errSuffix), with the pod's volume name, a DNS label. Group 1 is the volume. When
	// the kubelet's recorder aggregates similar events it writes one with the message prefixed by
	// "(combined from similar events): " (client-go tools/record/events_cache.go
	// EventAggregatorByReasonMessageFunc), which is accepted too.
	setUpNote = regexp.MustCompile(`^(?:\(combined from similar events\): )?MountVolume\.SetUp failed for volume "([a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)" : `)

	// notRegistered is the error of a CSI driver the kubelet does not know on the node: pkg/volume/csi
	// csi_mounter.go SetUpAt wraps, with the "kubernetes.io/csi: " prefix of csi_util.go log, the error of
	// csi_client.go "driver name %s not found in the list of registered CSI drivers", here with the driver's
	// default name (charts/secrets-store-csi-driver/templates/csidriver.yaml at driver v1.6.1).
	notRegistered = regexp.MustCompile(`^kubernetes\.io/csi: mounter\.SetUpAt failed to get CSI client: driver name secrets-store\.csi\.k8s\.io not found in the list of registered CSI drivers`)

	// driverError matches an error the driver returned from NodePublishVolume (secrets-store-csi-driver v1.6.1
	// pkg/secrets-store/nodeserver.go), which gRPC carries as `rpc error: code = <code> desc = <message>`. Group
	// 1 is the message, from the first of the driver's fixed texts on: those of nodeserver.go (the class, the
	// provider's mount, the pod status, a volume without a class) and of pkg/secrets-store/utils.go
	// getProviderFromSPC and getParametersFromSPC (spec.provider is optional in the CRD). Only these texts
	// make the note the driver's: other CSI drivers fail with other words.
	driverError = regexp.MustCompile(`^rpc error: code = [A-Za-z]{1,32} desc = ((?:failed to get secretproviderclass |failed to mount secrets store objects for pod |failed to create secret provider class pod status for pod |secretProviderClass is not set|provider not set in |parameters not set in )(?s:.*))$`)

	// spcNotFound is getSecretProviderItem's error (pkg/secrets-store/utils.go "failed to get
	// secretproviderclass %s/%s, error: %w") around the NotFound of the driver's cached client
	// (controller-runtime v0.24.1 pkg/cache/internal/cache_reader.go: the kind as resource, so
	// `SecretProviderClass.secrets-store.csi.x-k8s.io "<name>" not found`). Group 1 is the namespace, groups 2
	// and 3 are the name twice.
	spcNotFound = regexp.MustCompile(`^failed to get secretproviderclass ([a-z0-9][-a-z0-9]{0,62})/([a-z0-9][-a-z0-9.]{0,252}), error: SecretProviderClass\.secrets-store\.csi\.x-k8s\.io "([a-z0-9][-a-z0-9.]{0,252})" not found`)

	// providerError is NodePublishVolume's error around every error of mountSecretsStoreObjectContent
	// (nodeserver.go "failed to mount secrets store objects for pod %s/%s, err: %w"). Group 1 is the inner
	// error: the provider's Mount error, or the driver's own when it could not reach the provider.
	providerError = regexp.MustCompile(`^failed to mount secrets store objects for pod [a-z0-9][-a-z0-9]{0,62}/[a-z0-9][-a-z0-9.]{0,252}, err: ((?s:.*))$`)

	// providerUnreachable is mountSecretsStoreObjectContent's error when the provider's socket is missing or
	// does not answer (nodeserver.go "error connecting to provider %q: %w"), the inner error of
	// providerError. Group 1 is the provider's name.
	providerUnreachable = regexp.MustCompile(`^error connecting to provider "([A-Za-z0-9][-A-Za-z0-9_.]{0,62})"`)

	// azureMount is the Azure Key Vault provider's wrapper of every mount error
	// (secrets-store-csi-driver-provider-azure v1.8.2 pkg/server/server.go "failed to mount objects, error:
	// %w"), after gRPC's `rpc error: code = <code> desc = `. Only with it are the Key Vault reasons given.
	azureMount = regexp.MustCompile(`^rpc error: code = [A-Za-z]{1,32} desc = failed to mount objects, error: `)

	// keyVaultIdentity matches the provider's identity errors: the azidentity credentials it builds
	// (pkg/auth/auth.go: ClientAssertionCredential for workload identity, ManagedIdentityCredential,
	// ClientSecretCredential), whose errors are "<credential> authentication failed. ..." or "<credential>:
	// ..." (azidentity v1.14.0-beta.2.0.20260124023332-4c5175309ebb, the provider's pin in go.mod, errors.go
	// AuthenticationFailedError.Error; Entra ID's AADSTS codes are inside them), and the fixed texts of
	// pkg/auth/auth.go and pkg/provider/provider.go about missing tokens, client IDs and client secrets. It runs
	// on the error part only, after the object part, and every alternative holds a space or a colon after the
	// name: a Key Vault object name, which the user chooses and which azcore also prints in the request URL,
	// holds neither.
	keyVaultIdentity = regexp.MustCompile(`\b(?:ClientAssertionCredential|ManagedIdentityCredential|ClientSecretCredential|WorkloadIdentityCredential)(?: authentication failed|: )|failed to parse workload identity tokens|service account tokens not found|token for audience [^ ]{1,256} not found|nodePublishSecretRef secret is not set|could not find client(?:id|secret) in secrets|clientID is required for identity binding`)

	// keyVaultStatus and keyVaultCode read the lines that azcore v1.20.0 (internal/exported/response_error.go
	// ResponseError.Error) writes for a failed Key Vault call: "RESPONSE <code>: <status>" and "ERROR CODE:
	// <code>", the error.code of Key Vault's answer.
	keyVaultStatus = regexp.MustCompile(`(?m)^RESPONSE ([0-9]{3}): `)
	keyVaultCode   = regexp.MustCompile(`(?m)^ERROR CODE: ([A-Za-z]{1,64})\r?$`)

	// keyVaultHostNotFound is Go's resolver error for the vault's host ("lookup <host>[ on <server>]: no such
	// host", net/net.go DNSError, where the server is host:port, bracketed for IPv6), in any Azure cloud's
	// vault domain. A vault name is 3 to 24 letters, digits and hyphens. Group 1 is the host.
	keyVaultHostNotFound = regexp.MustCompile(`\blookup ([A-Za-z0-9][-A-Za-z0-9]{1,22}[A-Za-z0-9]\.vault\.[a-z0-9.-]{1,64}[a-z])(?: on (?:\[[0-9A-Fa-f:.%a-z]{2,64}\]|[^ :\[\]]{1,64}):[0-9]{1,5})?: no such host`)

	// keyVaultObject is the provider's wrapObjectTypeError (pkg/provider/provider.go "failed to get
	// objectType:%s, objectName:%s, objectVersion:%s: <error>", pkg/errors Wrapf). Groups 1 and 2 are the type
	// and the name, a Key Vault object name of 1 to 127 letters, digits and hyphens. The version (an id of
	// letters and digits, or empty) is matched to find where the error starts, never kept. It is the outermost
	// wrapper inside azureMount, so it is anchored there: text before it stays in the error part.
	keyVaultObject = regexp.MustCompile(`^failed to get objectType:(secret|key|cert), objectName:([0-9A-Za-z-]{1,127}), objectVersion:[0-9A-Za-z]{0,64}: `)
)

// keyVaultObjectNotFound are the error codes Key Vault answers for a missing object (azsecrets, azkeys and
// azcertificates: SecretNotFound, KeyNotFound, CertificateNotFound).
var keyVaultObjectNotFound = map[string]bool{"SecretNotFound": true, "KeyNotFound": true, "CertificateNotFound": true}

// Evidence fields of the rule besides the event's reason and reporting controller. The pod is the finding's
// subject, so it is not repeated here.
const (
	fieldVolume              = "volume.name"
	fieldSecretProviderClass = "secretProviderClass.name"
	fieldProvider            = "secretsStore.provider"
	fieldObjectType          = "keyVault.objectType"
	fieldObjectName          = "keyVault.objectName"
	fieldErrorCode           = "keyVault.errorCode"
	fieldVaultHost           = "keyVault.host"
	// fieldError is the error part of the note: the note without the kubelet's, the driver's and the Azure
	// provider's fixed prefixes and without the object part of wrapObjectTypeError (its type and name are
	// evidence of their own, its version is not kept). The evaluator keeps 256 bytes of a value, which the
	// prefixes alone would fill.
	fieldError = "event.note.error"
)

// mountFailed finds the kubelet's FailedMount event of a pod whose secrets volume the Secrets Store CSI driver
// could not set up, and says why as far as the note shows it. The event must be the kubelet's Warning about a
// Pod (apiVersion v1, or empty), in the pod's own namespace; its note must start with the kubelet's SetUp
// text and its error must be one of the driver's. Reason and summary are fixed; names read from the note and
// its error part are evidence only.
func mountFailed(ev modules.Event) (modules.Symptom, bool) {
	if ev.Type != eventTypeWarning || ev.Reason != eventReasonFailedMount || ev.ReportingController != reportingKubelet {
		return modules.Symptom{}, false
	}
	reg := ev.Regarding
	if reg.Kind != kindPod || reg.APIVersion != "" && reg.APIVersion != "v1" || reg.Name == "" ||
		reg.Namespace == "" || ev.Namespace != reg.Namespace {
		return modules.Symptom{}, false
	}
	m := setUpNote.FindStringSubmatchIndex(ev.Note)
	if m == nil {
		return modules.Symptom{}, false
	}
	rest := ev.Note[m[1]:]
	var c cause
	if notRegistered.MatchString(rest) {
		c = cause{reason: ReasonDriverNotRegistered, detail: rest}
	} else if d := driverError.FindStringSubmatch(rest); d != nil {
		c = driverReason(d[1], reg.Namespace)
	} else {
		return modules.Symptom{}, false
	}
	evidence := make([]modules.Evidence, 0, len(c.evidence)+4)
	evidence = append(evidence,
		modules.Evidence{Field: modules.EvidenceEventReason, Value: ev.Reason},
		modules.Evidence{Field: modules.EvidenceEventReportingController, Value: ev.ReportingController},
		modules.Evidence{Field: fieldVolume, Value: ev.Note[m[2]:m[3]]})
	evidence = append(evidence, c.evidence...)
	evidence = append(evidence, modules.Evidence{Field: fieldError, Value: c.detail})
	return modules.Symptom{Reason: c.reason, Summary: summaries[c.reason], Evidence: evidence}, true
}

// cause is what the error part of a note says: the reason, the evidence it names, and the error without the
// fixed prefixes the reason was read from.
type cause struct {
	reason   string
	evidence []modules.Evidence
	detail   string
}

// driverReason picks the reason for an error the driver returned, msg, for a pod in namespace ns.
func driverReason(msg, ns string) cause {
	if s := spcNotFound.FindStringSubmatch(msg); s != nil && s[1] == ns && s[2] == s[3] {
		return cause{reason: ReasonSecretProviderClassNotFound, detail: msg,
			evidence: []modules.Evidence{{Field: fieldSecretProviderClass, Value: s[2]}}}
	}
	if p := providerError.FindStringSubmatch(msg); p != nil {
		return providerReason(p[1])
	}
	return cause{reason: ReasonMountFailed, detail: msg}
}

// providerReason picks the reason for the inner error of providerError, msg: the driver could not reach the
// provider, or the provider's own error. The Key Vault reasons need the Azure provider's wrapper; any other
// provider's error is ReasonProviderFailed. In the order checked: identity (a token is fetched on the first
// Key Vault call, so its error is wrapped in the object's error too), access denied, object not found, vault
// not found.
func providerReason(msg string) cause {
	if p := providerUnreachable.FindStringSubmatch(msg); p != nil {
		return cause{reason: ReasonProviderUnavailable, detail: msg,
			evidence: []modules.Evidence{{Field: fieldProvider, Value: p[1]}}}
	}
	w := azureMount.FindStringIndex(msg)
	if w == nil {
		return cause{reason: ReasonProviderFailed, detail: msg}
	}
	errPart := msg[w[1]:]
	var evidence []modules.Evidence
	if o := keyVaultObject.FindStringSubmatchIndex(errPart); o != nil {
		evidence = append(evidence,
			modules.Evidence{Field: fieldObjectType, Value: errPart[o[2]:o[3]]},
			modules.Evidence{Field: fieldObjectName, Value: errPart[o[4]:o[5]]})
		errPart = errPart[o[1]:]
	}
	code := ""
	if c := keyVaultCode.FindStringSubmatch(errPart); c != nil {
		code = c[1]
	}
	status := ""
	if s := keyVaultStatus.FindStringSubmatch(errPart); s != nil {
		status = s[1]
	}
	withCode := func(reason string) cause {
		if code != "" {
			evidence = append(evidence, modules.Evidence{Field: fieldErrorCode, Value: code})
		}
		return cause{reason: reason, evidence: evidence, detail: errPart}
	}
	switch {
	case keyVaultIdentity.MatchString(errPart):
		return withCode(ReasonKeyVaultIdentityFailed)
	case status == "403":
		return withCode(ReasonKeyVaultAccessDenied)
	case keyVaultObjectNotFound[code]:
		return withCode(ReasonKeyVaultObjectNotFound)
	}
	if h := keyVaultHostNotFound.FindStringSubmatch(errPart); h != nil {
		return cause{reason: ReasonKeyVaultNotFound, detail: errPart,
			evidence: append(evidence, modules.Evidence{Field: fieldVaultHost, Value: strings.ToLower(h[1])})}
	}
	return withCode(ReasonProviderFailed)
}
