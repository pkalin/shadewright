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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/fluxstatus"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/incidents"
)

// The levels of a check. Only a fail makes the command exit non-zero.
const (
	levelPass = "pass"
	levelWarn = "warn"
	levelFail = "fail"
)

// doctorURL is the published how-to page of doctor. Links are this URL and a fixed anchor per check; nothing
// from the cluster goes into a link.
const doctorURL = "https://shadewright.io/how-to/check-install/"

// The anchors of the "## " sections of docs/how-to/check-install.md, one per kind of check.
const (
	anchorCRDs         = "#crds"
	anchorSettings     = "#settings"
	anchorKillSwitch   = "#kill-switch"
	anchorDeployments  = "#deployments"
	anchorRepositories = "#repositories"
	anchorFlux         = "#flux"
)

// errChecksFailed is returned after the output when at least one check failed, so the command exits non-zero.
var errChecksFailed = errors.New("at least one check failed")

// doctorCRDs are the CRDs whose presence doctor checks: the ones the other commands read, and the policies.
var doctorCRDs = append(slices.Clone(shadewrightCRDs), "remediationpolicies.remediation.shadewright.io")

// The components whose Deployments doctor checks, by their control-plane label (the same in the chart and in
// config/manager).
var doctorComponents = []string{"analyzer", "executor"}

// componentLabel is the label both installs put on the analyzer and executor Deployments.
const componentLabel = "control-plane"

// Flux kinds doctor lists, as plain objects: the plugin does not depend on Flux's Go types.
var (
	fluxGitRepositories = schema.GroupVersionKind{Group: fluxstatus.SourceGroup, Version: fluxstatus.SourceVersion,
		Kind: fluxstatus.KindGitRepository + "List"}
	fluxKustomizations = schema.GroupVersionKind{Group: fluxstatus.KustomizeGroup,
		Version: fluxstatus.KustomizeVersion, Kind: fluxstatus.KindKustomization + "List"}
)

// List bounds for the cluster-wide Flux lists: pages of listPageSize, at most listMaxPages of them.
const (
	listPageSize = 500
	listMaxPages = 20
)

// Check is the result of one check.
type Check struct {
	// Name says what was checked, for example "CRD incidents.remediation.shadewright.io".
	Name string `json:"name"`
	// Level is pass, warn or fail.
	Level string `json:"level"`
	// Detail says why, in one line.
	Detail string `json:"detail"`
	// Reported is what the analyzer reports in the RemediationSettings status (platform and integration
	// detection). It is shown only: no check reads it to decide its level (invariant 6).
	Reported string `json:"reported,omitempty"`
	// Link is the section of the how-to page that explains the check.
	Link string `json:"link"`
}

// Diagnosis is the output of doctor.
type Diagnosis struct {
	// Namespace is the operator namespace read.
	Namespace string `json:"namespace"`
	// Checks are the results, in the order they ran.
	Checks []Check `json:"checks"`
	// Failed is true when any check failed.
	Failed bool `json:"failed"`
}

func (d *Diagnosis) add(name, level, detail, anchor string) *Check {
	d.Checks = append(d.Checks, Check{Name: name, Level: level, Detail: detail, Link: doctorURL + anchor})
	if level == levelFail {
		d.Failed = true
	}
	return &d.Checks[len(d.Checks)-1]
}

// doctor runs every check with the caller's permissions and returns the results. It only reads (get and
// list), calls no Git host, Azure or model, and never decides anything from the RemediationSettings status: it
// only shows what the analyzer reports there. A read the caller may not make is a warn, since it says nothing
// about the install; any other read error is a fail, since the check could not run.
func doctor(ctx context.Context, c client.Reader, namespace, plugin string) *Diagnosis {
	d := &Diagnosis{Namespace: namespace, Checks: []Check{}}
	for _, name := range doctorCRDs {
		checkCRD(ctx, c, d, name, plugin)
	}
	settings, state := checkSettings(ctx, c, d)
	checkKillSwitch(d, settings, state)
	for _, comp := range doctorComponents {
		checkDeployments(ctx, c, d, namespace, comp)
	}
	checkRepositories(d, settings)
	checkFlux(ctx, c, d, settings)
	return d
}

// readLevel is the level and detail of a read that failed: forbidden is a warn, anything else a fail.
func readLevel(err error, what string) (string, string) {
	state, msg := readState(err)
	switch state {
	case stateForbidden:
		return levelWarn, what + " is not readable with your permissions, so it was not checked"
	case stateNotFound:
		return levelFail, what + " does not exist"
	}
	return levelFail, what + " could not be read: " + msg
}

// checkCRD checks that a CRD exists, is established, serves the API version the plugin was built for, and,
// when both carry a release, comes from the plugin's release.
func checkCRD(ctx context.Context, c client.Reader, d *Diagnosis, name, plugin string) {
	check := "CRD " + name
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
		level, detail := readLevel(err, "the CRD")
		d.add(check, level, detail, anchorCRDs)
		return
	}
	want := remediationv1alpha1.GroupVersion.Version
	var served []string
	ok := false
	for _, v := range crd.Spec.Versions {
		if v.Served {
			served = append(served, clean(v.Name, 63))
			ok = ok || v.Name == want
		}
	}
	if !ok {
		d.add(check, levelFail, fmt.Sprintf("does not serve %s, the version this plugin reads (serves: %s)", want,
			orNone(strings.Join(served, ", "))), anchorCRDs)
		return
	}
	established := false
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
			established = true
		}
	}
	if !established {
		d.add(check, levelFail, "is not Established: the API server does not serve it yet", anchorCRDs)
		return
	}
	if release, label := releaseVersion(plugin), crd.Labels[crdVersionLabel]; release != "" && label != "" &&
		strings.TrimPrefix(label, "v") != release {
		d.add(check, levelWarn, fmt.Sprintf("serves %s, but is version %s and this plugin is %s; upgrade one of them",
			want, clean(label, 63), clean(plugin, 63)), anchorCRDs)
		return
	}
	d.add(check, levelPass, "established, serves "+want, anchorCRDs)
}

// checkSettings reads the RemediationSettings and checks that its spec parses into the plugin's type and sets
// the daily analysis limit. It returns the parsed object, or nil with the read state. The status is decoded
// only to be shown.
func checkSettings(ctx context.Context, c client.Reader, d *Diagnosis) (*remediationv1alpha1.RemediationSettings,
	string) {
	const check = "RemediationSettings " + remediationv1alpha1.SettingsName
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(remediationv1alpha1.GroupVersion.WithKind("RemediationSettings"))
	if err := c.Get(ctx, client.ObjectKey{Name: remediationv1alpha1.SettingsName}, u); err != nil {
		state, _ := readState(err)
		level, detail := readLevel(err, "the RemediationSettings")
		if state == stateNotFound {
			detail += ": the analyzer refuses every analysis and the executor every write"
		}
		d.add(check, level, detail, anchorSettings)
		return nil, state
	}
	raw, err := json.Marshal(u.Object["spec"])
	if err != nil {
		d.add(check, levelFail, "the spec could not be encoded: "+clean(err.Error(), maxMessage), anchorSettings)
		return nil, stateUnreadable
	}
	s := &remediationv1alpha1.RemediationSettings{}
	if err := json.Unmarshal(raw, &s.Spec); err != nil {
		d.add(check, levelFail, "the spec does not parse: "+clean(err.Error(), maxMessage), anchorSettings)
		return nil, stateUnreadable
	}
	reported := reportedDetection(u.Object["status"])

	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	var level, detail string
	limit, hasLimit := incidents.MaxAnalysesPerDay(s)
	switch {
	case !hasLimit:
		level, detail = levelFail, "spec.analysis.maxAnalysesPerDay is not set: the analyzer refuses every analysis"
	case strict.Decode(&remediationv1alpha1.RemediationSettingsSpec{}) != nil:
		level, detail = levelWarn, "the spec has fields this plugin does not know: the CRDs may be from a newer release"
	case limit == 0:
		level, detail = levelWarn, "the daily analysis limit is 0: analysis is disabled"
	default:
		level, detail = levelPass, fmt.Sprintf("the spec parses, at most %d analyses per day", limit)
	}
	d.add(check, level, detail, anchorSettings).Reported = reported
	return s, stateOK
}

// reportedDetection says in one line what the analyzer reports in the RemediationSettings status. It is shown
// only; nothing in doctor reads it to decide a level (invariant 6).
func reportedDetection(status any) string {
	raw, err := json.Marshal(status)
	if err != nil || status == nil {
		return ""
	}
	var st remediationv1alpha1.RemediationSettingsStatus
	if json.Unmarshal(raw, &st) != nil {
		return "the status does not parse"
	}
	var parts []string
	if p := st.Platform; p != nil {
		detected := string(p.Detected)
		if detected == "" {
			detected = "none"
		}
		parts = append(parts, fmt.Sprintf("platform %s detected, %s active", clean(detected, 63),
			clean(string(p.Active), 63)))
	}
	for _, i := range st.Integrations {
		state := "enabled, not detected"
		switch {
		case i.Active:
			state = "active"
		case i.Detected:
			state = "detected, not enabled"
		}
		parts = append(parts, fmt.Sprintf("integration %s %s", clean(string(i.Name), 63), state))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}

// checkKillSwitch reports spec.paused. Paused is a warn: writes are held back on purpose.
func checkKillSwitch(d *Diagnosis, s *remediationv1alpha1.RemediationSettings, state string) {
	const check = "Kill switch"
	switch {
	case s == nil && state == stateNotFound:
		d.add(check, levelWarn, "no RemediationSettings: the executor blocks every write", anchorKillSwitch)
	case s == nil:
		d.add(check, levelWarn, "unknown: the RemediationSettings was not read", anchorKillSwitch)
	case s.Spec.Paused != nil && *s.Spec.Paused:
		d.add(check, levelWarn, "ON: writes are paused (spec.paused)", anchorKillSwitch)
	default:
		d.add(check, levelPass, "off: writes are allowed", anchorKillSwitch)
	}
}

// checkDeployments checks that the Deployments of one component in the operator namespace are available.
func checkDeployments(ctx context.Context, c client.Reader, d *Diagnosis, namespace, comp string) {
	check := "Deployment " + comp
	list := &appsv1.DeploymentList{}
	if err := c.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{componentLabel: comp}); err != nil {
		level, detail := readLevel(err, "the Deployments of namespace "+clean(namespace, maxName))
		d.add(check, level, detail, anchorDeployments)
		return
	}
	if len(list.Items) == 0 {
		d.add(check, levelFail, fmt.Sprintf("no Deployment labelled %s=%s in namespace %s; is this the operator "+
			"namespace? Pass it with -n", componentLabel, comp, clean(namespace, maxName)), anchorDeployments)
		return
	}
	for i := range list.Items {
		level, detail := deploymentLevel(&list.Items[i])
		d.add(check+" "+clean(list.Items[i].Name, maxName), level, detail, anchorDeployments)
	}
}

// deploymentLevel says whether a Deployment is available: pass when its Available condition is True for the
// current generation with all replicas updated and ready, warn while a rollout is under way, when fewer
// replicas are ready than desired or when it is scaled to 0, fail when it is not available or its rollout
// passed its progress deadline.
func deploymentLevel(dep *appsv1.Deployment) (string, string) {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if desired == 0 {
		return levelWarn, "scaled to 0 replicas"
	}
	var available, progressing *appsv1.DeploymentCondition
	for i := range dep.Status.Conditions {
		switch dep.Status.Conditions[i].Type {
		case appsv1.DeploymentAvailable:
			available = &dep.Status.Conditions[i]
		case appsv1.DeploymentProgressing:
			progressing = &dep.Status.Conditions[i]
		}
	}
	replicas := fmt.Sprintf("%d of %d replicas available", dep.Status.AvailableReplicas, desired)
	switch {
	case available == nil || available.Status != corev1.ConditionTrue || dep.Status.AvailableReplicas == 0:
		why := "no Available condition yet"
		if available != nil {
			why = conditionWhy(available)
		}
		return levelFail, "not available, " + replicas + " (" + why + ")"
	case progressing != nil && progressing.Status == corev1.ConditionFalse &&
		progressing.Reason == deploymentProgressDeadlineExceeded:
		// The rollout stopped: the old replicas keep serving, but the new spec will not roll out by itself.
		return levelFail, "rollout stuck, " + replicas + " (" + conditionWhy(progressing) + ")"
	case dep.Status.ObservedGeneration < dep.Generation || dep.Status.UpdatedReplicas < desired:
		return levelWarn, "available, but a rollout is under way: " + replicas
	case dep.Status.ReadyReplicas < desired:
		return levelWarn, fmt.Sprintf("available, but only %d of %d replicas ready", dep.Status.ReadyReplicas, desired)
	}
	return levelPass, "available, " + replicas
}

// deploymentProgressDeadlineExceeded is the reason of the Progressing condition of a Deployment whose rollout
// passed spec.progressDeadlineSeconds.
const deploymentProgressDeadlineExceeded = "ProgressDeadlineExceeded"

// conditionWhy is a condition's reason and message, cleaned for output.
func conditionWhy(c *appsv1.DeploymentCondition) string {
	why := clean(c.Reason, maxName)
	if c.Message != "" {
		why += ": " + clean(c.Message, maxMessage)
	}
	return why
}

// checkRepositories checks that each repository entry is complete: URL, provider settings, installation ID,
// App client ID, signing key and target branch. It calls no Git host.
func checkRepositories(d *Diagnosis, s *remediationv1alpha1.RemediationSettings) {
	if s == nil {
		d.add("Repositories", levelWarn, "not checked: the RemediationSettings was not read", anchorRepositories)
		return
	}
	if len(s.Spec.Repositories) == 0 {
		d.add("Repositories", levelWarn, "none configured: no pull request can be opened (observe-only)",
			anchorRepositories)
		return
	}
	for _, repo := range s.Spec.Repositories {
		check := "Repository " + clean(string(repo.Name), maxName)
		var missing []string
		if repo.URL == "" {
			missing = append(missing, "url")
		}
		switch repo.Provider {
		case remediationv1alpha1.GitProviderGitHub:
			missing = append(missing, missingGitHub(repo.GitHub)...)
		default:
			d.add(check, levelFail, fmt.Sprintf("provider %q is not one this plugin knows",
				clean(string(repo.Provider), 63)), anchorRepositories)
			continue
		}
		switch {
		case len(missing) > 0:
			d.add(check, levelFail, "incomplete, missing: "+strings.Join(missing, ", "), anchorRepositories)
		case repo.TargetBranch == "":
			d.add(check, levelWarn, "targetBranch is not set: main is used", anchorRepositories)
		default:
			d.add(check, levelPass, fmt.Sprintf("complete: provider %s, target branch %s", repo.Provider,
				clean(string(repo.TargetBranch), maxName)), anchorRepositories)
		}
	}
}

func missingGitHub(gh *remediationv1alpha1.GitHubRepository) []string {
	if gh == nil {
		return []string{"github"}
	}
	var missing []string
	if gh.AppClientID == "" {
		missing = append(missing, "github.appClientID")
	}
	if gh.InstallationID <= 0 {
		missing = append(missing, "github.installationID")
	}
	if gh.Signer.AzureKeyVault == nil || gh.Signer.AzureKeyVault.KeyID == "" {
		missing = append(missing, "github.signer.azureKeyVault.keyID")
	}
	return missing
}

// checkFlux checks, for each repository with a URL, the Flux GitRepositories whose spec.url equals it exactly
// (the match the executor's source mapping makes, ADR-0012) and the Kustomizations that apply them: the
// spec rules the executor applies to them, the branch they track and whether they are Ready. A cluster
// without Flux, a caller who may not list its objects, or a list cut at listMaxPages pages gets a warn.
func checkFlux(ctx context.Context, c client.Reader, d *Diagnosis, s *remediationv1alpha1.RemediationSettings) {
	if s == nil || len(s.Spec.Repositories) == 0 {
		return
	}
	sources, srcTruncated, err := listAll(ctx, c, fluxGitRepositories)
	if err != nil {
		fluxReadFailed(d, "GitRepositories", err)
		return
	}
	if srcTruncated {
		listTruncated(d, "GitRepositories")
	}
	var kustomizations []unstructured.Unstructured
	var ksErr error
	listedKs, ksTruncated := false, false
	for _, repo := range s.Spec.Repositories {
		if repo.URL == "" {
			continue
		}
		name := clean(string(repo.Name), maxName)
		target := string(repo.TargetBranch)
		if target == "" {
			target = "main"
		}
		var matched []unstructured.Unstructured
		for _, src := range sources {
			if url, _, _ := unstructured.NestedString(src.Object, "spec", "url"); url == repo.URL {
				matched = append(matched, src)
			}
		}
		if len(matched) == 0 {
			detail := "no Flux GitRepository has the URL of this repository exactly, so no target delivered " +
				"from it can be changed"
			if srcTruncated {
				detail = "no Flux GitRepository among the ones read has the URL of this repository exactly " +
					"(the list was cut short), so one may exist"
			}
			d.add("Flux for repository "+name, levelWarn, detail, anchorFlux)
			continue
		}
		if !listedKs {
			// Listed once, and a failure or a cut reported once, for all the sources.
			kustomizations, ksTruncated, ksErr = listAll(ctx, c, fluxKustomizations)
			listedKs = true
			switch {
			case ksErr != nil:
				fluxReadFailed(d, "Kustomizations", ksErr)
			case ksTruncated:
				listTruncated(d, "Kustomizations")
			}
		}
		for i := range matched {
			src := &matched[i]
			checkGitRepository(d, src, name, target)
			if ksErr == nil {
				checkKustomizations(d, src, kustomizations, ksTruncated)
			}
		}
	}
}

func fluxReadFailed(d *Diagnosis, what string, err error) {
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		d.add("Flux "+what, levelWarn, "the cluster does not serve Flux "+what+", so delivery was not checked",
			anchorFlux)
		return
	}
	level, detail := readLevel(err, "the Flux "+what)
	d.add("Flux "+what, level, detail, anchorFlux)
}

// listTruncated reports a cluster-wide Flux list cut at listMaxPages pages.
func listTruncated(d *Diagnosis, what string) {
	d.add("Flux "+what, levelWarn, fmt.Sprintf("more than %d objects: only the first %d were read, so the checks "+
		"below may miss some", listPageSize*listMaxPages, listPageSize*listMaxPages), anchorFlux)
}

// checkGitRepository checks a GitRepository against the executor's own source rules (sourcemap, ADR-0012):
// a source the executor refuses is a fail, as is one that tracks another branch than the repository's target.
func checkGitRepository(d *Diagnosis, src *unstructured.Unstructured, repo, target string) {
	check := "Flux GitRepository " + objName(src) + " (repository " + repo + ")"
	_, branch, stop := sourcemap.CheckGitRepository(src)
	if stop != nil {
		d.add(check, levelFail, refusal(stop), anchorFlux)
		return
	}
	if branch != target {
		d.add(check, levelFail, fmt.Sprintf("tracks branch %q, not the target branch %s of the repository",
			clean(branch, maxName), clean(target, maxName)), anchorFlux)
		return
	}
	level, detail := fluxLevel(src.Object)
	d.add(check, level, detail, anchorFlux)
}

// refusal is the detail of a check that the executor's source mapping would stop on: its reason and message.
func refusal(stop *sourcemap.Result) string {
	return "the executor refuses it: " + clean(string(stop.Reason), maxName) + ": " + clean(stop.Message, maxMessage)
}

// checkKustomizations checks the Kustomizations that apply src: a spec the executor refuses (sourcemap,
// ADR-0012) is a fail, otherwise the level follows Ready. truncated says the list was cut short.
func checkKustomizations(d *Diagnosis, src *unstructured.Unstructured, all []unstructured.Unstructured,
	truncated bool) {
	found := false
	for i := range all {
		ks := &all[i]
		ref, _, _ := unstructured.NestedStringMap(ks.Object, "spec", "sourceRef")
		ns := ref["namespace"]
		if ns == "" {
			ns = ks.GetNamespace()
		}
		if ref["kind"] != fluxstatus.KindGitRepository || ref["name"] != src.GetName() || ns != src.GetNamespace() {
			continue
		}
		found = true
		if stop := sourcemap.KustomizationSpecRefusal(ks); stop != nil {
			d.add("Flux Kustomization "+objName(ks), levelFail, refusal(stop), anchorFlux)
			continue
		}
		level, detail := fluxLevel(ks.Object)
		d.add("Flux Kustomization "+objName(ks), level, detail, anchorFlux)
	}
	if !found {
		detail := "no Kustomization applies this GitRepository"
		if truncated {
			detail = "no Kustomization among the ones read applies this GitRepository (the list was cut short)"
		}
		d.add("Flux Kustomizations of "+objName(src), levelWarn, detail, anchorFlux)
	}
}

// fluxLevel is pass for a Ready object, warn while it reconciles, fail with the failing condition otherwise.
func fluxLevel(obj map[string]any) (string, string) {
	if fluxstatus.Ready(obj) {
		return levelPass, "Ready"
	}
	if fluxstatus.Reconciling(obj) {
		return levelWarn, "not Ready yet: reconciling"
	}
	generation, has := fluxstatus.Generation(obj)
	cond, typ := fluxstatus.Failing(fluxstatus.Status(obj), generation, has)
	reason, _ := cond["reason"].(string)
	msg, _ := cond["message"].(string)
	detail := typ + " " + clean(reason, maxName)
	if msg != "" {
		detail += ": " + clean(msg, maxMessage)
	}
	return levelFail, "failing, " + detail
}

func objName(o *unstructured.Unstructured) string {
	return clean(o.GetNamespace(), maxName) + "/" + clean(o.GetName(), maxName)
}

// listAll lists every object of a kind in all namespaces, in pages, up to listMaxPages pages. truncated is
// true when the server had more after the last page read.
func listAll(ctx context.Context, c client.Reader, gvk schema.GroupVersionKind) (
	items []unstructured.Unstructured, truncated bool, err error) {
	token := ""
	for range listMaxPages {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		opts := []client.ListOption{client.Limit(listPageSize)}
		if token != "" {
			opts = append(opts, client.Continue(token))
		}
		if err := c.List(ctx, list, opts...); err != nil {
			return nil, false, err
		}
		items = append(items, list.Items...)
		if token = list.GetContinue(); token == "" {
			return items, false, nil
		}
	}
	return items, true, nil
}

// renderDoctorText writes one line per check, the docs link under each warn and fail, and a count.
func renderDoctorText(w io.Writer, d *Diagnosis) error {
	p := &printer{w: w}
	p.line("Namespace %s", clean(d.Namespace, maxName))
	counts := map[string]int{}
	for _, c := range d.Checks {
		counts[c.Level]++
		p.line("%-4s  %s: %s", strings.ToUpper(c.Level), c.Name, c.Detail)
		if c.Reported != "" {
			p.line("      Reported by the analyzer (shown only): %s", c.Reported)
		}
		if c.Level != levelPass {
			p.line("      See %s", c.Link)
		}
	}
	p.line("")
	p.line("%d passed, %d warnings, %d failed", counts[levelPass], counts[levelWarn], counts[levelFail])
	return p.err
}
