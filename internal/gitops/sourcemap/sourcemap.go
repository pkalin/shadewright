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
package sourcemap

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/pathmatch"
	"github.com/pkalin/shadewright/internal/providers"
)

// defaultTargetBranch is the default of GitRepository.targetBranch in RemediationSettings.
const defaultTargetBranch = "main"

// RepositoryOpener returns a GitProvider bound to one repository from RemediationSettings. It is called only
// after the repository has been matched, so the mapping never opens a repository that settings do not name.
type RepositoryOpener func(ctx context.Context, repo v1alpha1.GitRepository) (providers.GitProvider, error)

// Resolver maps objects to their source. It only reads: Get on the cluster and ReadFile and BranchHead on
// the repository.
type Resolver struct {
	// Client reads the target, the Flux Kustomization and the GitRepository.
	Client client.Reader
	// OpenRepository opens the repository the source URL matched.
	OpenRepository RepositoryOpener
}

// Input is what one mapping needs. Repositories and GitOps come from admin-owned configuration; Target and
// Field come from the request.
type Input struct {
	// Target is the request's targetRef. A non-empty UID must equal the live object's.
	Target v1alpha1.ObjectRef
	// Action is the request's action. It fixes the kinds the target may have and the field compared.
	Action v1alpha1.Action
	// Repositories is RemediationSettings spec.repositories.
	Repositories []v1alpha1.GitRepository
	// GitOps is the policy's spec.gitOps.
	GitOps v1alpha1.PolicyGitOps
}

// Resolve finds the repository, branch, file and base commit that declare in.Target, or returns why not.
// The error is non-nil only for failures that may succeed on retry, such as an API or Git host error; it is
// then a [*TransientError] matching [ErrTransient] and holds no text from the cluster or the provider. Every
// stop condition of ADR-0012 is a Result with Outcome NeedsIntervention or Denied.
func (r *Resolver) Resolve(ctx context.Context, in Input) (Result, error) {
	fld, err := FieldFor(in.Action, in.Target)
	if err != nil {
		return needsIntervention(ReasonInvalidInput, "the action does not fit the target: %v", err), nil
	}
	id, live, stop, err := r.target(ctx, in)
	if stop != nil || err != nil {
		return deref(stop), err
	}
	d, stop, err := r.delivery(ctx, in, id, live)
	if stop != nil || err != nil {
		return deref(stop), err
	}
	res, err := r.declaration(ctx, fld, in.GitOps, id, live, d)
	if err != nil || res.Outcome != Resolved {
		return res, err
	}
	// Scanning Git can take long. The Kustomization and GitRepository chose the repository, branch and path that
	// were scanned; if either changed meanwhile, the result describes a delivery that no longer exists. A
	// transient error makes the caller read everything again.
	for _, o := range d.seen {
		now, stop, err := r.get(ctx, o.gvk, o.namespace, o.name, ReasonSourceNotFound)
		if err != nil {
			return Result{}, err
		}
		if stop != nil || string(now.GetUID()) != o.uid || now.GetResourceVersion() != o.resourceVersion {
			return Result{}, transient(fmt.Sprintf("%s %s/%s changed while Git was scanned", o.gvk.Kind, o.namespace, o.name),
				errors.New("changed"))
		}
	}
	for _, o := range d.seen {
		res.Observed = append(res.Observed, Observed{GVK: o.gvk, Namespace: o.namespace, Name: o.name,
			UID: o.uid, ResourceVersion: o.resourceVersion})
	}
	return res, nil
}

// target validates the request's target and reads the live object.
func (r *Resolver) target(ctx context.Context, in Input) (identity, *unstructured.Unstructured, *Result, error) {
	gv, err := schema.ParseGroupVersion(in.Target.APIVersion)
	if err != nil || in.Target.Kind == "" || in.Target.Name == "" || in.Target.Namespace == "" || in.Target.UID == "" {
		return identity{}, nil, new(needsIntervention(ReasonInvalidInput,
			"the target must be a namespaced object pinned by UID")), nil
	}
	id := identity{gvk: gv.WithKind(in.Target.Kind), namespace: in.Target.Namespace, name: in.Target.Name}
	live, stop, err := r.get(ctx, id.gvk, id.namespace, id.name, ReasonTargetNotFound)
	if stop != nil || err != nil {
		return identity{}, nil, stop, err
	}
	if live.GetUID() != types.UID(in.Target.UID) {
		return identity{}, nil, new(needsIntervention(ReasonTargetChanged,
			"%s has another UID than the request names", id)), nil
	}
	return id, live, nil, nil
}

// delivery is what ADR-0012 steps 1 and 2 find: the repository and branch Flux delivers the target from, and
// the Kustomization's path in it.
type delivery struct {
	repo   v1alpha1.GitRepository
	branch string
	ks     string
	path   string
	// seen are the Flux objects as read, to tell whether they changed while Git was scanned.
	seen []seenObject
}

// seenObject is an object as it was read: its identity and version.
type seenObject struct {
	gvk                  schema.GroupVersionKind
	namespace, name      string
	uid, resourceVersion string
}

func seen(gvk schema.GroupVersionKind, namespace, name string, o *unstructured.Unstructured) seenObject {
	return seenObject{gvk: gvk, namespace: namespace, name: name, uid: string(o.GetUID()),
		resourceVersion: o.GetResourceVersion()}
}

// delivery follows the Flux labels to the Kustomization and its GitRepository (ADR-0012 steps 1 and 2).
func (r *Resolver) delivery(ctx context.Context, in Input, id identity, live *unstructured.Unstructured) (
	delivery, *Result, error) {
	labels := live.GetLabels()
	if _, ok := labels[LabelHelmReleaseName]; ok {
		return delivery{}, new(needsIntervention(ReasonHelmManaged,
			"%s is installed by a HelmRelease; Helm values are not supported", id)), nil
	}
	ksName, ksNamespace := labels[LabelKustomizationName], labels[LabelKustomizationNamespace]
	if ksName == "" && ksNamespace == "" {
		return delivery{}, new(needsIntervention(ReasonNoFluxLabels, "%s has no Flux Kustomization labels", id)), nil
	}
	if len(validation.IsDNS1123Subdomain(ksName)) > 0 || len(validation.IsDNS1123Label(ksNamespace)) > 0 {
		return delivery{}, new(needsIntervention(ReasonInvalidFluxLabels,
			"the Flux labels on %s are not a valid object reference", id)), nil
	}
	ks, stop, err := r.get(ctx, KustomizationGVK, ksNamespace, ksName, ReasonKustomizationNotFound)
	if stop != nil || err != nil {
		return delivery{}, stop, err
	}
	srcNamespace, srcName, ksPath, stop := checkKustomization(ks, id.gvk, id.namespace, id.name)
	if stop != nil {
		return delivery{}, stop, nil
	}
	src, stop, err := r.get(ctx, GitRepositoryGVK, srcNamespace, srcName, ReasonSourceNotFound)
	if stop != nil || err != nil {
		return delivery{}, stop, err
	}
	url, branch, stop := checkGitRepository(src)
	if stop != nil {
		return delivery{}, stop, nil
	}
	if stop := checkRevision(ks, src, branch); stop != nil {
		return delivery{}, stop, nil
	}
	repo, stop := matchRepository(in, url, branch, srcNamespace, srcName)
	if stop != nil {
		return delivery{}, stop, nil
	}
	return delivery{repo: repo, branch: branch, ks: ksNamespace + "/" + ksName, path: ksPath, seen: []seenObject{
		seen(KustomizationGVK, ksNamespace, ksName, ks), seen(GitRepositoryGVK, srcNamespace, srcName, src)}}, nil, nil
}

// declaration finds the one document in the Kustomization's path that declares the target and holds the live
// value, and checks the file against the policy (ADR-0012 step 3).
func (r *Resolver) declaration(ctx context.Context, fld Field, gitOps v1alpha1.PolicyGitOps, id identity,
	live *unstructured.Unstructured, d delivery) (Result, error) {
	git, err := r.OpenRepository(ctx, d.repo)
	if providers.IsInvalidConfig(err) {
		// Permanent: retrying cannot help, an administrator has to fix the settings. The text of err is
		// dropped, it is not needed to say what to look at.
		return needsIntervention(ReasonRepositoryUnusable,
			"the settings of repository %s cannot be used to open it", d.repo.Name), nil
	}
	if err != nil {
		return Result{}, transient("open repository "+string(d.repo.Name), err)
	}
	head, err := git.BranchHead(ctx, d.branch)
	if providers.IsNotFound(err) {
		return needsIntervention(ReasonBranchNotFound,
			"branch %s does not exist in repository %s", d.branch, d.repo.Name), nil
	}
	if err != nil {
		return Result{}, transient("read head of branch "+d.branch+" in repository "+string(d.repo.Name), err)
	}
	dir := "."
	if d.path != "" {
		var ok bool
		if dir, ok = joinRel(".", d.path); !ok {
			return needsIntervention(ReasonInvalidPath,
				"spec.path of Kustomization %s is not a relative path inside the repository", d.ks), nil
		}
	}
	w := newWalker(git, head)
	if stop, err := w.walk(ctx, dir, 0); stop != nil || err != nil {
		return deref(stop), err
	}
	decl, stop, err := w.findDeclaration(ctx, id)
	if stop != nil || err != nil {
		return deref(stop), err
	}
	inGit, okGit := fld.Get(decl.doc)
	inCluster, okLive := fld.Get(live.Object)
	if !okGit || !okLive {
		return needsIntervention(ReasonFieldNotFound, "%s is not set in both %s and the live %s", fld, decl.path, id), nil
	}
	if !fld.equal(inGit, inCluster) {
		return needsIntervention(ReasonValueMismatch,
			"%s in %s differs from the live value; something such as a patch changes it", fld, decl.path), nil
	}
	file := v1alpha1.GitFilePath(decl.path)
	if !pathAllowed(gitOps.AllowedPaths, file) {
		return denied(ReasonPathNotAllowed, "%s matches none of the policy's allowedPaths", decl.path), nil
	}
	return Result{
		Outcome: Resolved,
		Reason:  ReasonResolved,
		Message: fmt.Sprintf("%s is declared in %s on branch %s of repository %s", id, decl.path, d.branch, d.repo.Name),
		Source: &v1alpha1.ResolvedSource{
			RepositoryRef: d.repo.Name,
			TargetBranch:  v1alpha1.GitBranch(d.branch),
			Path:          file,
			BaseCommit:    head,
		},
	}, nil
}

// get reads one object as unstructured. A missing object, or a kind the API server does not serve, is a stop
// with notFound; other errors are returned for retry.
func (r *Resolver) get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string, notFound Reason) (
	*unstructured.Unstructured, *Result, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, new(needsIntervention(notFound, "%s %s/%s does not exist", gvk.Kind, namespace, name)), nil
	}
	if err != nil {
		return nil, nil, transient(fmt.Sprintf("get %s %s/%s", gvk.Kind, namespace, name), err)
	}
	return obj, nil, nil
}

// matchRepository finds the one settings repository whose URL equals the source URL exactly, and checks it
// against the policy and its target branch. The URL is never put in a message: it may carry credentials.
func matchRepository(in Input, url, branch, srcNamespace, srcName string) (v1alpha1.GitRepository, *Result) {
	src := fmt.Sprintf("GitRepository %s/%s", srcNamespace, srcName)
	var found []v1alpha1.GitRepository
	for _, repo := range in.Repositories {
		if url != "" && repo.URL == url {
			found = append(found, repo)
		}
	}
	if len(found) != 1 {
		return v1alpha1.GitRepository{}, new(denied(ReasonRepositoryNotConfigured,
			"the URL of %s equals %d repositories in RemediationSettings, not exactly one", src, len(found)))
	}
	repo := found[0]
	if repo.Name != in.GitOps.RepositoryRef {
		return v1alpha1.GitRepository{}, new(denied(ReasonRepositoryNotInPolicy,
			"%s delivers from repository %s, but the policy allows only %s", src, repo.Name, in.GitOps.RepositoryRef))
	}
	target := string(repo.TargetBranch)
	if target == "" {
		target = defaultTargetBranch
	}
	if branch != target {
		return v1alpha1.GitRepository{}, new(denied(ReasonBranchMismatch,
			"%s tracks another branch than the targetBranch %s of repository %s", src, target, repo.Name))
	}
	return repo, nil
}

func pathAllowed(patterns []v1alpha1.GitPathPattern, file v1alpha1.GitFilePath) bool {
	for _, p := range patterns {
		if pathmatch.Match(p, file) {
			return true
		}
	}
	return false
}

// deref returns the stop result, or the zero Result when only an error is set.
func deref(stop *Result) Result {
	if stop == nil {
		return Result{}
	}
	return *stop
}
