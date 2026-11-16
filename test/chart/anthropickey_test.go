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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	anthropicMount  = "/var/run/secrets/shadewright/anthropic"
	anthropicVolume = "anthropic-api-key"
)

// keySources are the ways to put the model API key file in the analyzer (ADR-0037 decision 3).
var keySources = map[string][]string{
	"secret": {keySecret},
	"secret, key": {
		keySecret,
		"analyzer.analysis.anthropic.apiKey.secretKey=token",
	},
	"csi volume": {
		"analyzer.analysis.anthropic.apiKey.volume.csi.driver=secrets-store.csi.k8s.io",
		"analyzer.analysis.anthropic.apiKey.volume.csi.readOnly=true",
		"analyzer.analysis.anthropic.apiKey.volume.csi.volumeAttributes.secretProviderClass=model-key",
	},
	"csi, file": {
		"analyzer.analysis.anthropic.apiKey.volume.csi.driver=secrets-store.csi.k8s.io",
		"analyzer.analysis.anthropic.apiKey.fileName=anthropic-key",
	},
}

func deployment(t *testing.T, r rendered, k string) appsv1.Deployment {
	t.Helper()
	u := r.chart[k]
	if u == nil {
		t.Fatalf("%s not rendered", k)
	}
	return *convert[appsv1.Deployment](t, u)
}

// helmTemplate renders the chart with the value files and --set arguments and returns helm's error text, or
// "" when it renders.
func helmTemplate(t *testing.T, files []string, set ...string) string {
	t.Helper()
	cmd := exec.Command(tool(t, "HELM", "helm"), helmArgsWith(t, files, set...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return err.Error() + "\n" + stderr.String()
	}
	return ""
}

func keyFlag(args []string) []string {
	var found []string
	for _, a := range args {
		if strings.HasPrefix(a, "--anthropic-api-key-file") {
			found = append(found, a)
		}
	}
	return found
}

// TestAnthropicKeyIsMountedIntoTheAnalyzerOnly: a key source renders one whole, read-only volume at the fixed
// path (no subPath) in the analyzer container, and the flag naming the file. Nothing else in the analyzer
// Deployment, and nothing in the executor or the verifier, differs from a render with no key source.
func TestAnthropicKeyIsMountedIntoTheAnalyzerOnly(t *testing.T) {
	base := render(t, false)
	if got := keyFlag(container(t, base, analyzerDeployment).Args); len(got) != 0 {
		t.Fatalf("no key source, but the flag is rendered: %v", got)
	}
	baseA := deployment(t, base, analyzerDeployment)
	for _, v := range baseA.Spec.Template.Spec.Volumes {
		if v.Name == anthropicVolume {
			t.Fatal("no key source, but the volume is rendered")
		}
	}
	for name, set := range keySources {
		t.Run(name, func(t *testing.T) {
			r := render(t, false, set...)
			a := deployment(t, r, analyzerDeployment)
			wantFile := "api-key"
			switch name {
			case "secret, key":
				wantFile = "token"
			case "csi, file":
				wantFile = "anthropic-key"
			}
			wantFlag := []string{"--anthropic-api-key-file=" + anthropicMount + "/" + wantFile}
			if got := keyFlag(a.Spec.Template.Spec.Containers[0].Args); !reflect.DeepEqual(got, wantFlag) {
				t.Errorf("flag = %v, want the file %s", got, wantFile)
			}
			var vols []corev1.Volume
			for _, v := range a.Spec.Template.Spec.Volumes {
				if v.Name == anthropicVolume {
					vols = append(vols, v)
				}
			}
			if len(vols) != 1 {
				t.Fatalf("want one %s volume, got %d", anthropicVolume, len(vols))
			}
			if strings.HasPrefix(name, "secret") {
				s := vols[0].Secret
				if s == nil || s.SecretName != "model-key" {
					t.Errorf("volume = %+v, want the Secret model-key", vols[0])
				}
			} else if vols[0].CSI == nil || vols[0].CSI.Driver != "secrets-store.csi.k8s.io" {
				t.Errorf("volume = %+v, want the CSI source", vols[0])
			}
			var mounts []corev1.VolumeMount
			for _, m := range a.Spec.Template.Spec.Containers[0].VolumeMounts {
				if m.Name == anthropicVolume {
					mounts = append(mounts, m)
				}
			}
			if len(mounts) != 1 || mounts[0].MountPath != anthropicMount || !mounts[0].ReadOnly ||
				mounts[0].SubPath != "" || mounts[0].SubPathExpr != "" {
				t.Errorf("mounts = %+v, want one read-only whole mount at %s", mounts, anthropicMount)
			}

			// Taking the key pieces away leaves the render with no key source: nothing else moved.
			pod := &a.Spec.Template.Spec
			pod.Volumes = slices.DeleteFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == anthropicVolume })
			c := &pod.Containers[0]
			c.VolumeMounts = slices.DeleteFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
				return m.Name == anthropicVolume
			})
			c.Args = slices.DeleteFunc(c.Args, func(s string) bool { return strings.HasPrefix(s, "--anthropic-api-key-file") })
			if len(pod.Volumes) == 0 {
				pod.Volumes = nil
			}
			if len(c.VolumeMounts) == 0 {
				c.VolumeMounts = nil
			}
			if !reflect.DeepEqual(a, baseA) {
				t.Errorf("the analyzer Deployment differs beyond the key volume, mount and flag")
			}

			// The executor and the verifier get no such volume, flag or mount, in any combination.
			for _, k := range []string{executorDeployment, verifierDeployment} {
				d := deployment(t, r, k)
				if !reflect.DeepEqual(d, deployment(t, base, k)) {
					t.Errorf("%s changed with a key source", k)
				}
				for _, v := range d.Spec.Template.Spec.Volumes {
					if v.Name == anthropicVolume {
						t.Errorf("%s has the key volume", k)
					}
				}
			}
		})
	}
}

// TestAnthropicKeyNeedsNoRBAC: the analyzer's Roles and ClusterRoles (and every other RBAC object) are the
// same with and without a key source; the kubelet or the CSI driver mounts the file.
func TestAnthropicKeyNeedsNoRBAC(t *testing.T) {
	want := rbacOnly(render(t, false).chart, false)
	for name, set := range keySources {
		got := rbacOnly(render(t, false, set...).chart, false)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the RBAC objects differ from a render with no key source", name)
		}
	}
}

func TestAnthropicKeyRefusals(t *testing.T) {
	both := []string{
		"analyzer.analysis.anthropic.apiKey.secretName=k",
		"analyzer.analysis.anthropic.apiKey.volume.emptyDir.medium=Memory",
	}
	if out := helmFails(t, both...); !strings.Contains(out, "sets both secretName and volume") {
		t.Errorf("both sources: %s", out)
	}
	badKey := []string{
		"analyzer.analysis.anthropic.apiKey.secretName=k",
		"analyzer.analysis.anthropic.apiKey.secretKey=a/b",
	}
	if out := helmFails(t, badKey...); !strings.Contains(out, "secretKey") {
		t.Errorf("a key with a slash: %s", out)
	}
	named := []string{
		"analyzer.analysis.anthropic.apiKey.volume.name=x",
		"analyzer.analysis.anthropic.apiKey.volume.emptyDir.medium=Memory",
	}
	if out := helmFails(t, named...); !strings.Contains(out, "may not set name") {
		t.Errorf("a named volume: %s", out)
	}
	extra := "analyzer.extraArgs={--anthropic-api-key-file=/x}"
	if out := helmFails(t, extra); !strings.Contains(out, "anthropic-api-key-file") {
		t.Errorf("extraArgs: %s", out)
	}
}

// TestAnthropicDeploymentNeedsAKeySource: settings.spec naming an Anthropic deployment fails the render with
// no key source (the issue's convenience check; the analyzer is the real gate), and renders with one.
func TestAnthropicDeploymentNeedsAKeySource(t *testing.T) {
	dir := t.TempDir()
	for role, spec := range map[string]string{
		"triage": "spec:\n  analysis:\n    models:\n" +
			"      triage: {provider: Anthropic, endpoint: https://api.anthropic.com, deployment: m}\n",
		"escalation": "spec:\n  analysis:\n    models:\n" +
			"      escalation: {provider: Anthropic, endpoint: https://api.anthropic.com, deployment: m}\n",
	} {
		f := filepath.Join(dir, role+".yaml")
		body := "settings:\n  create: true\n  " + strings.ReplaceAll(strings.TrimSuffix(spec, "\n"), "\n", "\n  ") + "\n"
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		files := []string{"-f", testValues(t), "-f", f}
		if err := helmTemplate(t, files); err == "" {
			t.Fatalf("%s: rendered with no key source", role)
		} else if !strings.Contains(err, "settings.spec.analysis.models."+role) ||
			!strings.Contains(err, "analyzer.analysis.anthropic.apiKey") {
			t.Errorf("%s: unhelpful message: %s", role, err)
		}
		if err := helmTemplate(t, files, "analyzer.analysis.anthropic.apiKey.secretName=k"); err != "" {
			t.Errorf("%s: with a Secret: %s", role, err)
		}
		if err := helmTemplate(t, files, "analyzer.analysis.anthropic.apiKey.volume.emptyDir.medium=Memory"); err != "" {
			t.Errorf("%s: with a volume: %s", role, err)
		}
	}
}

// settingsFile writes a values file that renders RemediationSettings with the given models block (YAML lines
// under spec.analysis.models, indented by six spaces) and returns it.
func settingsFile(t *testing.T, models string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "settings.yaml")
	body := "settings:\n  create: true\n  spec:\n    analysis:\n      models:\n" + models
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// modelLine is one deployment of the models block of settingsFile.
func modelLine(role, fields string) string { return "        " + role + ": {" + fields + "}\n" }

const (
	anthropicFields = "provider: Anthropic, endpoint: https://api.anthropic.com, deployment: claude-haiku-5-5"
	foundryFields   = "endpoint: https://lab.services.ai.azure.com/, deployment: big, apiVersion: v1"
	keySecret       = "analyzer.analysis.anthropic.apiKey.secretName=model-key"
	claudeTriage    = "analyzer.analysis.triageModel=claude-haiku-5-5"
)

// An install whose model deployments in settings.spec are all Anthropic renders without
// analyzer.identity.azure.clientId and analyzer.analysis.endpointSuffixes, with no Azure identity on the
// analyzer and no --model-endpoint-suffixes; one Foundry deployment, or no deployments named at all, brings
// back both refusals with their messages (#814).
func TestAnthropicOnlyInstallNeedsNoAzureSettings(t *testing.T) {
	anthropicOnly := []string{"-f", testValues(t), "-f",
		settingsFile(t, modelLine("triage", anthropicFields)+modelLine("escalation", anthropicFields))}
	set := []string{keySecret, claudeTriage, "analyzer.analysis.escalationModel=claude-haiku-5-5"}
	if err := helmTemplate(t, anthropicOnly, set...); err != "" {
		t.Fatalf("an Anthropic-only install did not render: %s", err)
	}
	a := deployment(t, renderArgs(t, helmArgsWith(t, anthropicOnly, set...)), analyzerDeployment)
	if _, ok := a.Spec.Template.Labels["azure.workload.identity/use"]; ok {
		t.Error("the analyzer Pod has the Azure workload identity label")
	}
	args := a.Spec.Template.Spec.Containers[0].Args
	if slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, "--model-endpoint-suffixes") }) {
		t.Errorf("args %v hold --model-endpoint-suffixes", args)
	}
	for _, want := range []string{"--triage-model=claude-haiku-5-5", "--escalation-model=claude-haiku-5-5",
		"--anthropic-api-key-file=" + anthropicMount + "/api-key"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v lack %s", args, want)
		}
	}
	// A record store is written with the workload identity, so it needs the client ID even then.
	withStore := append(slices.Clone(set), "analyzer.analysis.recordStore.azureBlob.account=stswanabc")
	if err := helmTemplate(t, anthropicOnly, withStore...); !strings.Contains(err,
		"recordStore.azureBlob.account needs analyzer.identity.azure.clientId") {
		t.Errorf("a record store without the client ID: %q", err)
	}

	const (
		noClientID = "analyzer.analysis.triageModel needs analyzer.identity.azure.clientId"
		noSuffixes = "analyzer.analysis.triageModel needs analyzer.analysis.endpointSuffixes"
	)
	for name, models := range map[string]string{
		"mixed":             modelLine("triage", anthropicFields) + modelLine("escalation", foundryFields),
		"Foundry named":     modelLine("triage", "provider: Foundry, "+foundryFields),
		"provider left out": modelLine("escalation", foundryFields),
		"no settings.spec":  "",
	} {
		t.Run(name, func(t *testing.T) {
			files := []string{"-f", testValues(t)}
			if models != "" {
				files = append(files, "-f", settingsFile(t, models))
			}
			if err := helmTemplate(t, files, keySecret, claudeTriage, labSuffix); !strings.Contains(err, noClientID) {
				t.Errorf("without the client ID: %q", err)
			}
			if err := helmTemplate(t, files, keySecret, claudeTriage, analyzerClientID); !strings.Contains(err, noSuffixes) {
				t.Errorf("without suffixes: %q", err)
			}
			if err := helmTemplate(t, files, keySecret, claudeTriage, analyzerClientID, labSuffix); err != "" {
				t.Errorf("with both: %s", err)
			}
		})
	}
}
