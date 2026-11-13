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
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/fluxstatus"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
)

// The states of a delivery source in the report.
const (
	// sourceOK: the rules the report checks pass, and the settings name the repository with that branch.
	sourceOK = "ok"
	// sourceRefused: the source mapping stops on it whatever Flux does next, with the reason given.
	sourceRefused = "refused"
	// sourceNotReady: Flux has not (yet) reconciled it; the mapping stops on it until Flux has.
	sourceNotReady = "not-ready"
	// sourceUnknown: it could not be checked: a read failed or was not allowed, or the settings were not read.
	sourceUnknown = "unknown"
)

// How the RemediationSettings name the repository of a source: by its URL, exactly, as the mapping matches it.
const (
	settingsNamed     = "named"
	settingsNotNamed  = "not-named"
	settingsAmbiguous = "ambiguous"
	settingsUnknown   = "unknown"
)

// watchNamespacesFlag is the analyzer flag that holds the watched namespaces.
const watchNamespacesFlag = "watch-namespaces"

// maxWatched bounds the namespaces the report reads workloads in. The analyzer refuses more than this many when
// it reads pod logs or Prometheus.
const maxWatched = 256

// workloadKinds are the kinds the action catalog changes through the source mapping: the workloads counted.
var workloadKinds = []struct{ list, resource string }{
	{list: "DeploymentList", resource: "deployments.apps"},
	{list: "StatefulSetList", resource: "statefulsets.apps"},
}

// Delivery is the report of the delivery sources of the watched namespaces: which Flux Kustomizations and
// GitRepositories deliver their workloads, and whether the source mapping (ADR-0012) and the settings accept
// each. Everything in it is a name, a count or a typed value read from the cluster, cleaned and bounded.
type Delivery struct {
	// WatchedNamespaces are the namespaces of the analyzer's --watch-namespaces that were read.
	WatchedNamespaces []string `json:"watchedNamespaces"`
	// Unchecked says why the report, or part of it, could not be made, one entry per cause.
	Unchecked []string `json:"unchecked,omitempty"`
	// Namespaces counts the workloads of each watched namespace by how they are delivered.
	Namespaces []NamespaceDelivery `json:"namespaces"`
	// Sources are the Kustomizations named by the labels of those workloads, with their GitRepositories.
	Sources []DeliverySource `json:"sources"`
}

// NamespaceDelivery counts the Deployments and StatefulSets of one watched namespace by how they are delivered.
type NamespaceDelivery struct {
	// Namespace is the watched namespace.
	Namespace string `json:"namespace"`
	// Workloads is how many Deployments and StatefulSets were read.
	Workloads int `json:"workloads"`
	// Kustomization is how many carry the labels of a Flux Kustomization; their sources are listed apart.
	Kustomization int `json:"kustomization"`
	// HelmRelease is how many a HelmRelease installed: the mapping stops on them (HelmManaged).
	HelmRelease int `json:"helmRelease"`
	// NoFluxLabels is how many carry no Flux label: the mapping stops on them (NoFluxLabels).
	NoFluxLabels int `json:"noFluxLabels"`
	// InvalidFluxLabels is how many carry Flux labels that name no valid object: the mapping stops on them
	// (InvalidFluxLabels).
	InvalidFluxLabels int `json:"invalidFluxLabels"`
	// Unchecked names the reads that failed, so the counts may miss workloads.
	Unchecked []string `json:"unchecked,omitempty"`
}

// DeliverySource is one Kustomization that delivers workloads of the watched namespaces, and its GitRepository.
type DeliverySource struct {
	// Kustomization is the namespace and name of the Flux Kustomization.
	Kustomization string `json:"kustomization"`
	// Workloads is how many workloads of the watched namespaces name it in their labels.
	Workloads int `json:"workloads"`
	// GitRepository is the namespace and name of its source, when it names a GitRepository.
	GitRepository string `json:"gitRepository,omitempty"`
	// URL is the host and path of the GitRepository's spec.url: no scheme, user, password, port or query.
	URL string `json:"url,omitempty"`
	// Branch is the branch the GitRepository tracks, when the mapping accepts its spec.ref.
	Branch string `json:"branch,omitempty"`
	// Settings is named, not-named, ambiguous (more than one repository has the URL) or unknown.
	Settings string `json:"settings"`
	// Repository is the settings repository with the URL, when exactly one has it.
	Repository string `json:"repository,omitempty"`
	// State is ok, refused, not-ready or unknown.
	State string `json:"state"`
	// Reason is the source mapping's reason for a state other than ok, when there is one.
	Reason string `json:"reason,omitempty"`
	// Detail says why, in one line.
	Detail string `json:"detail,omitempty"`
}

// checkDelivery makes the delivery report: from the watched namespaces of the analyzer's Deployments, the
// workloads there, and the Flux objects their labels name, each checked by the source mapping's own rules
// (sourcemap.KustomizationSpecRefusal, CheckGitRepository and CompareSource). It adds one check, never a
// fail: a read it may not make or that fails is listed as not checked.
func checkDelivery(ctx context.Context, c client.Reader, flux *fluxLists, d *Diagnosis,
	s *remediationv1alpha1.RemediationSettings, analyzers []appsv1.Deployment, analyzersRead bool) {
	const check = "Delivery sources"
	rep := &Delivery{WatchedNamespaces: []string{}, Namespaces: []NamespaceDelivery{}, Sources: []DeliverySource{}}
	d.Delivery = rep
	if !analyzersRead || len(analyzers) == 0 {
		rep.Unchecked = append(rep.Unchecked, "no analyzer Deployment was read, so the watched namespaces "+
			"are unknown")
		d.add(check, levelWarn, "not checked: the watched namespaces are unknown", anchorDelivery)
		return
	}
	watched, err := watchedNamespaces(analyzers)
	if err != nil {
		rep.Unchecked = append(rep.Unchecked, err.Error())
		d.add(check, levelWarn, "not checked: "+err.Error(), anchorDelivery)
		return
	}
	if len(watched) == 0 {
		d.add(check, levelWarn, "the analyzer watches no namespace (--watch-namespaces is empty), so no workload "+
			"is delivered to it", anchorDelivery)
		return
	}
	if len(watched) > maxWatched {
		rep.Unchecked = append(rep.Unchecked, fmt.Sprintf("the analyzer watches %d namespaces: only the first %d "+
			"were read", len(watched), maxWatched))
		watched = watched[:maxWatched]
	}
	rep.WatchedNamespaces = watched
	byKs := map[string]int{}
	for _, ns := range watched {
		rep.Namespaces = append(rep.Namespaces, countWorkloads(ctx, c, ns, byKs))
	}
	if len(byKs) > 0 {
		ksList := flux.list(ctx, fluxKustomizations)
		srcList := flux.list(ctx, fluxGitRepositories)
		for _, key := range slices.Sorted(maps.Keys(byKs)) {
			src := evalSource(key, ksList, srcList, s)
			src.Workloads = byKs[key]
			rep.Sources = append(rep.Sources, src)
		}
	}
	level, detail := deliveryLevel(rep)
	d.add(check, level, detail, anchorDelivery)
}

// watchedNamespaces reads --watch-namespaces from the containers of the analyzer Deployments, as the analyzer
// parses it (analyzer.ParseNamespaces). The last occurrence in a container wins, as with Go flags; several
// Deployments give the union. The raw value is never printed: it is cluster data.
func watchedNamespaces(analyzers []appsv1.Deployment) ([]string, error) {
	var all []string
	found := false
	for i := range analyzers {
		for _, ctr := range analyzers[i].Spec.Template.Spec.Containers {
			value, ok := flagValue(append(slices.Clone(ctr.Command), ctr.Args...), watchNamespacesFlag)
			if !ok {
				continue
			}
			found = true
			names, err := analyzer.ParseNamespaces(value)
			if err != nil {
				return nil, fmt.Errorf("--%s of the analyzer Deployment %s is not a list of namespace names",
					watchNamespacesFlag, clean(analyzers[i].Name, maxName))
			}
			all = append(all, names...)
		}
	}
	if !found {
		return []string{}, nil
	}
	slices.Sort(all)
	return slices.Compact(all), nil
}

// flagValue returns the value of the last -name or --name flag in args, in the forms -name=value and -name value.
func flagValue(args []string, name string) (string, bool) {
	value, found := "", false
	for i, a := range args {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if trimmed == a {
			continue
		}
		if v, ok := strings.CutPrefix(trimmed, name+"="); ok {
			value, found = v, true
		} else if trimmed == name && i+1 < len(args) {
			value, found = args[i+1], true
		}
	}
	return value, found
}

// countWorkloads reads the metadata of the Deployments and StatefulSets of ns and counts them by their Flux
// labels, in the mapping's order: a HelmRelease label first, then the Kustomization labels. The Kustomizations
// named go into byKs, keyed by namespace/name.
func countWorkloads(ctx context.Context, c client.Reader, ns string, byKs map[string]int) NamespaceDelivery {
	out := NamespaceDelivery{Namespace: ns}
	for _, kind := range workloadKinds {
		items, truncated, err := listMetadata(ctx, c, ns, kind.list)
		if err != nil {
			out.Unchecked = append(out.Unchecked, couldNotList(kind.resource, ns, err))
			continue
		}
		if truncated {
			out.Unchecked = append(out.Unchecked, fmt.Sprintf("list %s in namespace %s: more than %d, only the "+
				"first %d were read", kind.resource, ns, listPageSize*listMaxPages, listPageSize*listMaxPages))
		}
		for i := range items {
			out.Workloads++
			labels := items[i].GetLabels()
			if _, ok := labels[sourcemap.LabelHelmReleaseName]; ok {
				out.HelmRelease++
				continue
			}
			name, namespace := labels[sourcemap.LabelKustomizationName], labels[sourcemap.LabelKustomizationNamespace]
			switch {
			case name == "" && namespace == "":
				out.NoFluxLabels++
			case len(validation.IsDNS1123Subdomain(name)) > 0 || len(validation.IsDNS1123Label(namespace)) > 0:
				out.InvalidFluxLabels++
			default:
				out.Kustomization++
				byKs[namespace+"/"+name]++
			}
		}
	}
	return out
}

// couldNotList names a list that failed: the verb, the resource and the namespace, and why.
func couldNotList(resource, ns string, err error) string {
	what := "list " + resource
	if ns != "" {
		what += " in namespace " + ns
	}
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return what + ": the cluster does not serve it"
	}
	state, msg := readState(err)
	if state == stateForbidden {
		return what + ": not allowed with your permissions"
	}
	return what + ": " + msg
}

// listMetadata lists only the metadata of the objects of an apps/v1 list kind in ns, in pages, up to
// listMaxPages pages: no spec or status is read.
func listMetadata(ctx context.Context, c client.Reader, ns, listKind string) (
	items []metav1.PartialObjectMetadata, truncated bool, err error) {
	token := ""
	for range listMaxPages {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(listKind))
		opts := []client.ListOption{client.InNamespace(ns), client.Limit(listPageSize)}
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

// evalSource checks the Kustomization key (namespace/name) and its GitRepository by the source mapping's rules,
// in its order, and matches the GitRepository's URL to the settings repositories exactly. The rules that need a
// target object (status.inventory) are not checked.
func evalSource(key string, ksList, srcList *listResult, s *remediationv1alpha1.RemediationSettings) DeliverySource {
	out := DeliverySource{Kustomization: cleanKey(key), Settings: settingsUnknown}
	if ksList.err != nil {
		out.State, out.Detail = sourceUnknown, "could not check: "+couldNotList(kustomizationsResource, "", ksList.err)
		return out
	}
	ks := find(ksList.items, key)
	if ks == nil {
		out.State, out.Reason = sourceRefused, string(sourcemap.ReasonKustomizationNotFound)
		out.Detail = "no Kustomization of that name exists"
		if ksList.truncated {
			out.State, out.Reason = sourceUnknown, ""
			out.Detail = "not among the Kustomizations read (the list was cut short)"
		}
		return out
	}
	stop := sourcemap.KustomizationSpecRefusal(ks)
	notReady := ""
	if stop == nil && !fluxstatus.Ready(ks.Object) {
		notReady = string(sourcemap.ReasonKustomizationNotReady)
		if fluxstatus.Reconciling(ks.Object) {
			notReady = string(sourcemap.ReasonKustomizationReconciling)
		}
	}
	ref, _, _ := unstructured.NestedStringMap(ks.Object, "spec", "sourceRef")
	srcNS := ref["namespace"]
	if srcNS == "" {
		srcNS = ks.GetNamespace()
	}
	if ref["kind"] != fluxstatus.KindGitRepository || len(validation.IsDNS1123Subdomain(ref["name"])) > 0 ||
		len(validation.IsDNS1123Label(srcNS)) > 0 {
		return finish(out, stop, notReady, sourceRefused, string(sourcemap.ReasonUnsupportedSource),
			"its source is not a GitRepository")
	}
	out.GitRepository = clean(srcNS, maxName) + "/" + clean(ref["name"], maxName)
	if srcList.err != nil {
		return finish(out, stop, notReady, sourceUnknown, "",
			"could not check: "+couldNotList(gitRepositoriesResource, "", srcList.err))
	}
	src := find(srcList.items, srcNS+"/"+ref["name"])
	if src == nil {
		if srcList.truncated {
			return finish(out, stop, notReady, sourceUnknown, "",
				"its GitRepository is not among the ones read (the list was cut short)")
		}
		return finish(out, stop, notReady, sourceRefused, string(sourcemap.ReasonSourceNotFound),
			"its GitRepository does not exist")
	}
	rawURL, _, _ := unstructured.NestedString(src.Object, "spec", "url")
	out.URL = hostPath(rawURL)
	_, branch, srcStop := sourcemap.CheckGitRepository(src)
	if srcStop == nil {
		out.Branch = clean(branch, maxName)
	}
	target := matchSettings(&out, rawURL, s)
	switch {
	case stop != nil || notReady != "":
		return finish(out, stop, notReady, "", "", "")
	case srcStop != nil:
		return finish(out, srcStop, "", "", "", "")
	case !fluxstatus.Ready(src.Object):
		out.State, out.Reason = sourceNotReady, string(sourcemap.ReasonSourceNotReady)
		out.Detail = "its GitRepository is not Ready for its current generation"
	case sourcemap.CompareSource(ks, src) == providers.SourceBehind:
		out.State, out.Reason = sourceNotReady, string(sourcemap.ReasonRevisionMismatch)
		out.Detail = "it has not applied the revision its GitRepository last fetched"
	case out.Settings == settingsUnknown:
		out.State, out.Detail = sourceUnknown, "the RemediationSettings was not read"
	case out.Settings != settingsNamed:
		out.State, out.Reason = sourceRefused, string(sourcemap.ReasonRepositoryNotConfigured)
		out.Detail = "no single repository of the RemediationSettings has the URL of its GitRepository exactly"
	case branch != target:
		out.State, out.Reason = sourceRefused, string(sourcemap.ReasonBranchMismatch)
		out.Detail = "its GitRepository tracks another branch than the targetBranch " + clean(target, maxName) +
			" of repository " + out.Repository
	default:
		out.State = sourceOK
	}
	return out
}

// The resources of the cluster-wide Flux lists, as a failed read names them.
const (
	kustomizationsResource  = "kustomizations.kustomize.toolkit.fluxcd.io"
	gitRepositoriesResource = "gitrepositories.source.toolkit.fluxcd.io"
)

// finish sets the state of out from the first stop that applies: a Kustomization spec refusal, then a
// Kustomization that is not Ready, then the given state, reason and detail.
func finish(out DeliverySource, stop *sourcemap.Result, notReady, state, reason, detail string) DeliverySource {
	switch {
	case stop != nil:
		out.State, out.Reason, out.Detail = sourceRefused, clean(string(stop.Reason), maxName), clean(stop.Message,
			maxMessage)
	case notReady != "":
		out.State, out.Reason, out.Detail = sourceNotReady, notReady, "the Kustomization is not Ready for its "+
			"current generation"
	default:
		out.State, out.Reason, out.Detail = state, reason, detail
	}
	return out
}

// matchSettings sets the Settings and Repository of out by the settings repositories whose URL equals rawURL
// exactly, the match the mapping makes, and returns the target branch of the one that matches.
func matchSettings(out *DeliverySource, rawURL string, s *remediationv1alpha1.RemediationSettings) string {
	if s == nil {
		out.Settings = settingsUnknown
		return ""
	}
	var found []remediationv1alpha1.GitRepository
	for _, repo := range s.Spec.Repositories {
		if rawURL != "" && repo.URL == rawURL {
			found = append(found, repo)
		}
	}
	switch len(found) {
	case 0:
		out.Settings = settingsNotNamed
		return ""
	case 1:
	default:
		out.Settings = settingsAmbiguous
		return ""
	}
	out.Settings, out.Repository = settingsNamed, clean(string(found[0].Name), maxName)
	if found[0].TargetBranch == "" {
		return "main"
	}
	return string(found[0].TargetBranch)
}

// hostPath reduces a Git URL to its host and path: no scheme, user, password, port, query or fragment, since
// spec.url may carry credentials. A value that is not a URL with a host prints as "(not a URL)".
func hostPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "(not a URL)"
	}
	return clean(u.Hostname()+u.EscapedPath(), maxName)
}

// find returns the object of items with the key namespace/name, or nil.
func find(items []unstructured.Unstructured, key string) *unstructured.Unstructured {
	for i := range items {
		if items[i].GetNamespace()+"/"+items[i].GetName() == key {
			return &items[i]
		}
	}
	return nil
}

// cleanKey cleans each part of a namespace/name key.
func cleanKey(key string) string {
	ns, name, _ := strings.Cut(key, "/")
	return clean(ns, maxName) + "/" + clean(name, maxName)
}

// deliveryLevel is pass when every workload of the watched namespaces comes from a source in state ok and
// every read was made, otherwise warn; never a fail, since a cluster may deliver some workloads by other means
// on purpose.
func deliveryLevel(rep *Delivery) (string, string) {
	var workloads, fromKs, helm, noLabels, unchecked int
	unchecked = len(rep.Unchecked)
	for _, n := range rep.Namespaces {
		workloads += n.Workloads
		fromKs += n.Kustomization
		helm += n.HelmRelease
		noLabels += n.NoFluxLabels + n.InvalidFluxLabels
		unchecked += len(n.Unchecked)
	}
	states := map[string]int{}
	for _, src := range rep.Sources {
		states[src.State]++
	}
	detail := fmt.Sprintf("%d watched namespaces, %d workloads: %d from Kustomizations, %d from a HelmRelease, "+
		"%d without Flux labels; %d sources: %d ok, %d refused, %d not ready, %d unknown",
		len(rep.WatchedNamespaces), workloads, fromKs, helm, noLabels, len(rep.Sources), states[sourceOK],
		states[sourceRefused], states[sourceNotReady], states[sourceUnknown])
	if unchecked > 0 {
		detail += fmt.Sprintf("; %d reads not made", unchecked)
	}
	if unchecked > 0 || helm > 0 || noLabels > 0 || states[sourceOK] != len(rep.Sources) {
		return levelWarn, detail
	}
	return levelPass, detail
}

// renderDeliveryText writes the delivery report under the checks: a line per namespace, then a line per source.
func renderDeliveryText(p *printer, rep *Delivery) {
	if rep == nil || (len(rep.WatchedNamespaces) == 0 && len(rep.Unchecked) == 0) {
		return
	}
	p.line("")
	p.line("Delivery sources of the watched namespaces")
	for _, u := range rep.Unchecked {
		p.line("  NOT CHECKED  %s", u)
	}
	for _, n := range rep.Namespaces {
		p.line("  Namespace %s: %d workloads: %d from Kustomizations, %d from a HelmRelease (%s), %d without Flux "+
			"labels (%s), %d with invalid Flux labels (%s)", n.Namespace, n.Workloads, n.Kustomization,
			n.HelmRelease, sourcemap.ReasonHelmManaged, n.NoFluxLabels, sourcemap.ReasonNoFluxLabels,
			n.InvalidFluxLabels, sourcemap.ReasonInvalidFluxLabels)
		for _, u := range n.Unchecked {
			p.line("    NOT CHECKED  %s", u)
		}
	}
	for _, src := range rep.Sources {
		line := fmt.Sprintf("  %-9s  Kustomization %s (%d workloads)", strings.ToUpper(src.State),
			src.Kustomization, src.Workloads)
		if src.GitRepository != "" {
			line += ", GitRepository " + src.GitRepository
		}
		if src.URL != "" {
			line += " " + src.URL
		}
		if src.Branch != "" {
			line += " branch " + src.Branch
		}
		line += ", settings: " + settingsText(src)
		if src.Reason != "" {
			line += "; " + src.Reason
		}
		if src.Detail != "" {
			line += ": " + src.Detail
		}
		p.line("%s", line)
	}
}

func settingsText(src DeliverySource) string {
	switch src.Settings {
	case settingsNamed:
		return "repository " + src.Repository
	case settingsNotNamed:
		return "not named"
	case settingsAmbiguous:
		return "named by more than one repository"
	}
	return "not checked"
}
