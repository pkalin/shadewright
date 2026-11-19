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

package chart

import (
	"bytes"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	anID      = "11111111-1111-1111-1111-111111111111"
	tenantID  = "33333333-3333-3333-3333-333333333333"
	tokenPath = "/var/run/secrets/azure/tokens/azure-identity-token"
	tokenVol  = "azure-identity-token"
)

// tokenModeSet turns the mode on for both components, with placeholder IDs.
func tokenModeSet(extra ...string) []string {
	return append([]string{
		"executor.identity.azure.projectToken=true", "executor.identity.azure.tenantId=" + tenantID,
		"analyzer.identity.azure.clientId=" + anID, "analyzer.identity.azure.projectToken=true",
		"analyzer.identity.azure.tenantId=" + tenantID,
	}, extra...)
}

func podOf(t *testing.T, r rendered, k string) (corev1.PodTemplateSpec, corev1.Container) {
	t.Helper()
	c := container(t, r, k)
	d := r.chart[k]
	return convert[appsv1.Deployment](t, d).Spec.Template, c
}

// TestAzureProjectTokenRendersWhatTheWebhookAdds: the volume, the mount and the four variables of both
// identities, and no webhook label.
func TestAzureProjectTokenRendersWhatTheWebhookAdds(t *testing.T) {
	r := render(t, false, tokenModeSet()...)
	for k, client := range map[string]string{executorDeployment: id0, analyzerDeployment: anID} {
		tpl, c := podOf(t, r, k)
		if _, ok := tpl.Labels[identityLabel]; ok {
			t.Errorf("%s still has the %s label", k, identityLabel)
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		want := map[string]string{
			"AZURE_CLIENT_ID": client, "AZURE_TENANT_ID": tenantID, "AZURE_FEDERATED_TOKEN_FILE": tokenPath,
			"AZURE_AUTHORITY_HOST": "https://login.microsoftonline.com/",
		}
		for n, v := range want {
			if env[n] != v {
				t.Errorf("%s: %s = %q, want %q", k, n, env[n], v)
			}
		}
		var vol *corev1.Volume
		for i := range tpl.Spec.Volumes {
			if tpl.Spec.Volumes[i].Name == tokenVol {
				vol = &tpl.Spec.Volumes[i]
			}
		}
		if vol == nil || vol.Projected == nil || len(vol.Projected.Sources) != 1 {
			t.Fatalf("%s: no projected token volume: %+v", k, tpl.Spec.Volumes)
		}
		sat := vol.Projected.Sources[0].ServiceAccountToken
		if sat == nil || sat.Audience != "api://AzureADTokenExchange" || sat.Path != "azure-identity-token" ||
			sat.ExpirationSeconds == nil || *sat.ExpirationSeconds != 3600 {
			t.Errorf("%s: token source %+v", k, sat)
		}
		if vol.Projected.DefaultMode == nil || *vol.Projected.DefaultMode != 0o444 {
			t.Errorf("%s: defaultMode %v, want 0444", k, vol.Projected.DefaultMode)
		}
		found := false
		for _, m := range c.VolumeMounts {
			if m.Name == tokenVol && m.MountPath == "/var/run/secrets/azure/tokens" && m.ReadOnly {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no read-only mount of the token volume: %+v", k, c.VolumeMounts)
		}
	}
	// The verifier has no identity: nothing is added to it.
	r2 := render(t, false, tokenModeSet(withVerifier)...)
	if body := allObjects(r2)[verifierDeployment]; strings.Contains(body, "AZURE_") || strings.Contains(body, tokenVol) {
		t.Errorf("the verifier got Azure token settings: %s", body)
	}
}

// TestAzureProjectTokenKeepsOtherVolumes: the metrics certificate mount stays beside the token mount.
func TestAzureProjectTokenKeepsOtherVolumes(t *testing.T) {
	r := render(t, false, tokenModeSet("executor.metricsTLS.secretName=tls", "analyzer.metricsTLS.secretName=tls")...)
	for _, k := range []string{executorDeployment, analyzerDeployment} {
		tpl, c := podOf(t, r, k)
		if len(tpl.Spec.Volumes) != 2 || len(c.VolumeMounts) != 2 {
			t.Errorf("%s: want the metrics-tls and token volumes and mounts, have %d and %d", k,
				len(tpl.Spec.Volumes), len(c.VolumeMounts))
		}
	}
}

// TestAzureProjectTokenOneComponentOnly: switching the executor leaves the analyzer's Pod as the webhook mode
// has it (label, no volume).
func TestAzureProjectTokenOneComponentOnly(t *testing.T) {
	r := render(t, false, "executor.identity.azure.projectToken=true", "executor.identity.azure.tenantId="+tenantID,
		"analyzer.identity.azure.clientId="+anID)
	tpl, c := podOf(t, r, analyzerDeployment)
	if tpl.Labels[identityLabel] != "true" || len(tpl.Spec.Volumes) != 0 || len(c.Env) > 4 {
		t.Errorf("the analyzer changed: labels %v, volumes %v, env %v", tpl.Labels, tpl.Spec.Volumes, c.Env)
	}
	for _, e := range c.Env {
		if strings.HasPrefix(e.Name, "AZURE_") {
			t.Errorf("the analyzer got %s", e.Name)
		}
	}
}

// TestAzureProjectTokenRefusals: the mode needs the client ID and the tenant ID, and the tenant ID means nothing
// without the mode.
func TestAzureProjectTokenRefusals(t *testing.T) {
	cases := []struct {
		set  []string
		want string
	}{
		{[]string{"analyzer.identity.azure.projectToken=true", "analyzer.identity.azure.tenantId=" + tenantID},
			"analyzer.identity.azure.clientId"},
		{[]string{"analyzer.identity.azure.projectToken=true", "analyzer.identity.azure.clientId=" + anID},
			"analyzer.identity.azure.tenantId"},
		{[]string{"executor.identity.azure.projectToken=true"}, "executor.identity.azure.tenantId"},
		{[]string{"executor.identity.azure.projectToken=true", "executor.identity.azure.clientId="},
			"executor.identity.azure.clientId"},
		{[]string{"analyzer.identity.azure.tenantId=" + tenantID}, "analyzer.identity.azure.projectToken"},
		{[]string{"analyzer.identity.azure.projectToken=true", "analyzer.identity.azure.clientId=" + anID,
			"analyzer.identity.azure.tenantId=not a tenant"}, "tenantId"},
		{[]string{executorOff, "executor.identity.azure.projectToken=true"}, "executor.identity.azure.projectToken"},
	}
	for _, c := range cases {
		out := helmFails(t, c.set...)
		if !strings.Contains(out, c.want) {
			t.Errorf("%v: error does not name %s:\n%s", c.set, c.want, out)
		}
	}
}

// TestAzureProjectTokenOffRendersAsBefore: the default and an explicit false render the same bytes, with the
// webhook label and none of the token objects.
func TestAzureProjectTokenOffRendersAsBefore(t *testing.T) {
	helm := tool(t, "HELM", "helm")
	out := func(set ...string) []byte {
		return run(t, repoRoot(t), helm, helmArgsWith(t, ciValues(t),
			append([]string{"clusterId=chart-test", "analyzer.identity.azure.clientId=" + anID,
				"executor.identity.azure.clientId=" + id0, withVerifier}, set...)...)...)
	}
	def := out()
	off := out("executor.identity.azure.projectToken=false", "analyzer.identity.azure.projectToken=false")
	if !bytes.Equal(def, off) {
		t.Error("projectToken=false renders differently from the default")
	}
	for _, s := range []string{tokenVol, "AZURE_TENANT_ID", "AZURE_FEDERATED_TOKEN_FILE", "AZURE_AUTHORITY_HOST"} {
		if bytes.Contains(def, []byte(s)) {
			t.Errorf("the default render contains %s", s)
		}
	}
	if n := bytes.Count(def, []byte(identityLabel+": \"true\"")); n != 2 {
		t.Errorf("the default render has the webhook label %d times, want 2", n)
	}
}
