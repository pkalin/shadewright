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

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// testAppsPath is the build directory and Kustomization name the fleet fixtures use.
const testAppsPath = "apps"

const (
	opCommit = "CommitFile"
	opOpen   = "OpenChangeRequest"
)

var _ = Describe("RemediationRequest pull request step", func() {
	ctx := context.Background()
	var (
		seq        int
		deployment *appsv1.Deployment
		policy     *remediationv1alpha1.RemediationPolicy
		settings   *remediationv1alpha1.RemediationSettings
		request    *remediationv1alpha1.RemediationRequest
		git        *fakegit.Git
		clock      time.Time
		build      remediationv1alpha1.GitDirPath
		// faulty, when set, is the provider the reconciler gets instead of git; openErr fails opening it.
		faulty       *faultyGit
		openErr      error
		writeTimeout time.Duration
		// apiReader, when set, replaces the reconciler's APIReader (k8sClient otherwise); incidentReadTimeout its IncidentReadTimeout.
		apiReader           client.Reader
		incidentReadTimeout time.Duration
		// opens counts the repositories opened through the reconciler.
		opens atomic.Int32
		// recorder, when set, records the reconciler's Events.
		recorder *record.FakeRecorder
	)

	manifest := func(limit string) []byte {
		return deploymentManifest(deployment.Name, deployment.Namespace, workerContainer, limit)
	}
	reconciler := func() *RemediationRequestReconciler {
		r := &RemediationRequestReconciler{
			Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Source: sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
				res := resolved()
				res.Source.BaseCommit, _ = git.BranchHead(ctx, mainBranch)
				res.Source.BuildPath = build
				return res, nil
			}),
			OpenRepository: func(_ context.Context, repo remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
				if repo.URL != settings.Spec.Repositories[0].URL {
					return nil, errors.New("unknown repository")
				}
				opens.Add(1)
				if openErr != nil {
					return nil, openErr
				}
				if faulty != nil {
					return faulty, nil
				}
				return git, nil
			},
			Now:                 func() time.Time { return clock },
			WriteTimeout:        writeTimeout,
			IncidentReadTimeout: incidentReadTimeout,
		}
		if apiReader != nil {
			r.APIReader = apiReader
		}
		if recorder != nil {
			r.Recorder = recorder
		}
		return r
	}
	key := func() reconcile.Request {
		return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)}
	}
	// reconcileResult runs one reconcile with a new reconciler, as after a restart.
	reconcileResult := func() (reconcile.Result, error) {
		return reconciler().Reconcile(ctx, key())
	}
	// reconcileOnce is reconcileResult without the result.
	reconcileOnce := func() error {
		_, err := reconciler().Reconcile(ctx, key())
		return err
	}
	fetch := func() *remediationv1alpha1.RemediationRequest {
		got := &remediationv1alpha1.RemediationRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got)).To(Succeed())
		return got
	}
	// drive reconciles until the request reaches phase or stops changing, ignoring retryable errors, as the
	// controller's work queue would.
	drive := func(phase remediationv1alpha1.RemediationPhase) *remediationv1alpha1.RemediationRequest {
		for range 20 {
			_ = reconcileOnce()
			if got := fetch(); got.Status.Phase == phase {
				return got
			}
		}
		return fetch()
	}
	openRequests := func() []providers.ChangeRequest {
		var out []providers.ChangeRequest
		for _, cr := range git.ChangeRequests() {
			if cr.HeadBranch == freshness.BranchName(fetch()) {
				out = append(out, cr)
			}
		}
		return out
	}

	BeforeEach(func() {
		seq++
		clock = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		build = buildPath
		faulty, openErr, writeTimeout, recorder = nil, nil, 0, nil
		opens.Store(0)
		apiReader, incidentReadTimeout = nil, 0
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fx.TargetNamespace}})
		Expect(client.IgnoreAlreadyExists(err)).To(Succeed())

		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: fmt.Sprintf("pr-%d", seq)},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabel: appName}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabel: appName}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: workerContainer, Image: workerImage,
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						}},
					}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		policy = fx.RemediationPolicy(fmt.Sprintf("pr-policy-%d", seq))
		policy.Spec.Actions = []remediationv1alpha1.PolicyAction{fx.MemoryLimitPolicyAction()}
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
		policy.Spec.RequestNamespaces = []remediationv1alpha1.Name{requestNamespace}
		policy.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{appsGlob}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		settings = fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		Expect(k8sClient.Create(ctx, settings)).To(Succeed())
		request = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("pr-rr-%d", seq))
		request.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
		request.Spec.TargetRef.Name = deployment.Name
		request.Spec.TargetRef.UID = remediationv1alpha1.UID(deployment.UID)
		Expect(k8sClient.Create(ctx, request)).To(Succeed())

		git = fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: manifest("256Mi"), buildKustomize: buildFile, "README.yaml": []byte("a: 1\n")})

		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
	})
	AfterEach(func() {
		got := &remediationv1alpha1.RemediationRequest{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got); err == nil &&
			controllerutil.ContainsFinalizer(got, FinalizerName) {
			base := got.DeepCopy()
			controllerutil.RemoveFinalizer(got, FinalizerName)
			Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
		}
		for _, o := range []client.Object{request, policy, settings, deployment} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
	})

	It("turns an allowed request into one branch, one commit and one pull request", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(controllerutil.ContainsFinalizer(got, FinalizerName)).To(BeTrue())
		Expect(got.Status.Deadline).To(BeNil(), "the deadline of the step is cleared once the pull request is open")
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)).To(BeTrue())
		approved := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(approved).NotTo(BeNil())
		Expect(approved.Reason).To(Equal(ReasonWaitingForReview))

		crs := openRequests()
		Expect(crs).To(HaveLen(1))
		cr := crs[0]
		Expect(cr.State).To(Equal(providers.ChangeRequestOpen))
		Expect(cr.BaseBranch).To(Equal(mainBranch))
		Expect(cr.Title).To(HavePrefix("fix: set the memory limit of container worker of "))
		Expect(cr.Body).To(ContainSubstring(fmt.Sprintf("<!-- shadewright request-uid=%s plan-digest=%s -->",
			got.UID, got.Status.PlanDigest)))
		for _, part := range []string{"**Change:**", "**Scope:**", "**Evidence:**", "**Expected effect:**",
			"**Risk:**", "**Rollback:**"} {
			Expect(cr.Body).To(ContainSubstring(part))
		}
		main, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status.PullRequest).To(Equal(&remediationv1alpha1.PullRequestStatus{
			Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA, RenderedTargetHead: main}),
			"the change is rendered once more at the target branch head after the pull request is recorded")

		file, err := git.ReadFile(ctx, cr.HeadSHA, sourcePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Data).To(Equal(manifest("384Mi")), "one value changed, nothing else")
		onMain, err := git.ReadFile(ctx, main, sourcePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(onMain.Data).To(Equal(manifest("256Mi")), "the target branch is never written")

		// More reconciles change nothing.
		before := fetch().ResourceVersion
		for range 3 {
			Expect(reconcileOnce()).To(Succeed())
		}
		Expect(fetch().ResourceVersion).To(Equal(before))
		Expect(openRequests()).To(HaveLen(1))
	})

	// answering replaces the request with one whose incidentRef is ref, and drives it to a pull request.
	answering := func(ref remediationv1alpha1.IncidentRef) string {
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		replaced := fx.MemoryLimitRequest(requestNamespace, request.Name+"-incident")
		replaced.Spec.PolicyRef, replaced.Spec.TargetRef = request.Spec.PolicyRef, request.Spec.TargetRef
		replaced.Spec.IncidentRef = ref
		Expect(k8sClient.Create(ctx, replaced)).To(Succeed())
		request = replaced
		Expect(drive(remediationv1alpha1.PhaseAwaitingApproval).Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		crs := openRequests()
		Expect(crs).To(HaveLen(1))
		return crs[0].Body
	}
	// collectorIncident creates an Incident named as the collector names it, from fingerprint hex.
	collectorIncident := func(hex string) *remediationv1alpha1.Incident {
		inc := &remediationv1alpha1.Incident{
			ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace, Name: "incident-" + hex},
			Spec: remediationv1alpha1.IncidentSpec{
				Fingerprint: "sha256:" + hex, Reason: "OOMKilled",
				Subject: remediationv1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "orders"},
			},
		}
		Expect(k8sClient.Create(ctx, inc)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, inc))).To(Succeed()) })
		return inc
	}

	It("names the Incident it read back verbatim", func() {
		hex := strings.Repeat("9f86d081", 7) + fmt.Sprintf("%08x", seq)
		inc := collectorIncident(hex)
		body := answering(remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)})
		Expect(body).To(ContainSubstring("for incident `" + inc.Name + "`."))
		Expect(body).To(ContainSubstring("incident `" + inc.Name + "`, policy"))
	})

	It("redacts an Incident name of the collector's form that no Incident with that UID has", func() {
		hex := strings.Repeat("3f1c9a7b", 7) + fmt.Sprintf("%08x", seq)
		inc := collectorIncident(hex)
		// A hex secret written as "incident-<secret>": the right form, but no Incident has this UID.
		body := answering(remediationv1alpha1.IncidentRef{Name: inc.Name, UID: "22222222-2222-4222-8222-222222222222"})
		Expect(body).NotTo(ContainSubstring(hex))
		Expect(body).To(ContainSubstring("for incident `[REDACTED:token]`."))
	})

	It("opens the pull request with the name redacted when the Incident read hangs", func() {
		hex := strings.Repeat("5e884898", 7) + fmt.Sprintf("%08x", seq)
		inc := collectorIncident(hex)
		// The read of the Incident never answers, as from a stalled API server; every other read goes through.
		// Its own deadline is far shorter than the step's, so the step still has the time to open the pull
		// request. Without that deadline the read would use up the step's and the open would time out.
		hangs := &atomic.Int32{}
		apiReader = hangingIncidentReader{Reader: k8sClient, hangs: hangs}
		writeTimeout, incidentReadTimeout = 2*time.Second, 100*time.Millisecond
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		replaced := fx.MemoryLimitRequest(requestNamespace, request.Name+"-incident")
		replaced.Spec.PolicyRef, replaced.Spec.TargetRef = request.Spec.PolicyRef, request.Spec.TargetRef
		replaced.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)}
		Expect(k8sClient.Create(ctx, replaced)).To(Succeed())
		request = replaced
		// Unlike drive, any error fails the test: a step that ran out of time is retryable, and a retry would
		// hang the same way. The loop has an overall limit too, so a slow regression fails instead of crawling.
		start := time.Now()
		for range 20 {
			Expect(reconcileOnce()).To(Succeed())
			if fetch().Status.Phase == remediationv1alpha1.PhaseAwaitingApproval {
				break
			}
			Expect(time.Since(start)).To(BeNumerically("<", 20*time.Second), "the steps took too long")
		}
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		// The read did hang: the body is redacted because of the deadline, not because the read was skipped.
		Expect(hangs.Load()).To(BeNumerically(">=", 1))
		crs := openRequests()
		Expect(crs).To(HaveLen(1))
		Expect(crs[0].Body).NotTo(ContainSubstring(hex))
		Expect(crs[0].Body).To(ContainSubstring("for incident `[REDACTED:token]`."))
	})

	It("finds the existing work after a restart between any two steps", func() {
		// Every Git write fails once after it took effect, as if the executor crashed before it could go on.
		crashed := map[string]bool{}
		git.SetAfter(func(op string) error {
			if (op == "CreateBranch" || op == opCommit || op == opOpen) && !crashed[op] {
				crashed[op] = true
				return errors.New("crash after the write")
			}
			return nil
		})
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(crashed).To(HaveLen(3))
		Expect(openRequests()).To(HaveLen(1))
		head, err := git.BranchHead(ctx, freshness.BranchName(got))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status.PullRequest.HeadSHA).To(Equal(head))
	})

	It("adopts a pull request whose status write was lost", func() {
		// Drive up to the open, then lose the status write that records the pull request.
		failStatus := true
		r := reconciler()
		r.Client = &statusFailer{Client: k8sClient, fail: func() bool { return failStatus && len(git.ChangeRequests()) > 0 }}
		for range 10 {
			_, _ = r.Reconcile(ctx, key())
			if len(openRequests()) == 1 {
				break
			}
		}
		Expect(openRequests()).To(HaveLen(1))
		Expect(fetch().Status.PullRequest).To(BeNil())
		failStatus = false
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.PullRequest).NotTo(BeNil())
		Expect(got.Status.PullRequest.Number).To(Equal(openRequests()[0].Number))
		Expect(openRequests()).To(HaveLen(1))
	})

	It("opens one pull request when reconciled concurrently", func() {
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				defer GinkgoRecover()
				for range 15 {
					_ = reconcileOnce()
				}
			})
		}
		wg.Wait()
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(openRequests()).To(HaveLen(1))
		file, err := git.ReadFile(ctx, got.Status.PullRequest.HeadSHA, sourcePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Data).To(Equal(manifest("384Mi")))
	})

	It("stops automation when someone else pushes to the branch", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(cond.Reason).To(Equal(ReasonHeadMoved))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)).
			To(BeTrue(), "the pull request is still open")
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen), "the pull request is left for a person")
	})

	It("stops before the commit when the change renders into another object too", func() {
		// The patched file is a base that a second overlay builds under another name: the change would reach
		// b-<name> as well, which the plan does not cover.
		_, err := git.Push(mainBranch, "apps/kustomization.yaml", []byte(
			"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - orders\n  - copy\n"))
		Expect(err).NotTo(HaveOccurred())
		_, err = git.Push(mainBranch, "apps/copy/kustomization.yaml", []byte(
			"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamePrefix: b-\nresources:\n  - ../orders\n"))
		Expect(err).NotTo(HaveOccurred())
		// The mapping now names the parent directory as the build, as it would for a Kustomization at apps.
		build = testAppsPath
		got := fetch()
		got.Status.Source.BuildPath = build
		// The digest covers the build directory, so the plan is digested again, as the policy check would.
		got.Status.PlanDigest, err = freshness.ComputeDigest(got, got.Status.Source, policy, settings)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		drive(remediationv1alpha1.PhaseNeedsIntervention)
		got = fetch()
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(string(semdiff.ReasonOtherObjectChanged)))
		head, err := git.BranchHead(ctx, freshness.BranchName(got))
		Expect(err).NotTo(HaveOccurred())
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		Expect(head).To(Equal(base), "nothing was committed")
		Expect(openRequests()).To(BeEmpty())
	})

	It("does not commit on a branch that holds something else", func() {
		// The branch exists with another value in the file, as if someone created it by hand.
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		branch := freshness.BranchName(fetch())
		Expect(git.CreateBranch(ctx, branch, base)).To(Succeed())
		_, err = git.Push(branch, sourcePath, manifest("1Gi"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonBranchDiverged))
		Expect(openRequests()).To(BeEmpty())
	})

	It("leaves a pull request without its marker alone", func() {
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		branch := freshness.BranchName(fetch())
		Expect(git.CreateBranch(ctx, branch, base)).To(Succeed())
		_, err = git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: mainBranch, Title: "someone"})
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonForeignChangeRequest))
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
	})

	It("records a merge by a person as Merged, before the freshness check sees the changed file", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Merge(got.Status.PullRequest.Number)
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionApproved)).To(BeTrue())
		Expect(reconcileOnce()).To(Succeed(), "a merged request takes no further step yet")
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
	})

	It("stops when a person closes the pull request without a merge", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(git.CloseChangeRequest(ctx, got.Status.PullRequest.Number)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			To(Equal(ReasonChangeRequestClosed))
		opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(opened.Status).To(Equal(metav1.ConditionTrue))
		Expect(opened.Reason).To(Equal(ReasonChangeRequestClosed))
		Expect(opened.Message).To(Equal(fmt.Sprintf("Pull request #%d was closed without a merge",
			got.Status.PullRequest.Number)), "the condition no longer says the pull request is open")
	})

	It("expires a request whose freshness check keeps failing", func() {
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		Expect(reconcileOnce()).To(Succeed()) // branch
		git.SetFail(func(op string) error {
			if op == opBranchHead { // the freshness check reads it; expiring does not
				return providers.ErrRetryable
			}
			return nil
		})
		Expect(reconcileOnce()).NotTo(Succeed(), "a failing freshness check is retried")
		clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
	})

	It("expires a request that did not open its pull request before the deadline", func() {
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		Expect(reconcileOnce()).To(Succeed()) // branch
		clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
		Expect(reconcileOnce()).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
		Expect(openRequests()).To(BeEmpty())
	})

	It("closes the pull request when the request is deleted, and only then lets it go", func() {
		drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(fetch().DeletionTimestamp).NotTo(BeNil(), "the finalizer holds the request")
		Expect(reconcileOnce()).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(request),
				&remediationv1alpha1.RemediationRequest{}))
		}).Should(BeTrue())
		crs := git.ChangeRequests()
		Expect(crs).To(HaveLen(1))
		Expect(crs[0].State).To(Equal(providers.ChangeRequestClosed))
		got := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), got)).To(Succeed())
		Expect(got.DeletionTimestamp).To(BeNil(), "deleting a request never touches the cluster")
	})

	gone := func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(request),
			&remediationv1alpha1.RemediationRequest{}))
	}

	It("closes its own pull request on deletion after automation stopped", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("never closes a pull request without its marker, also on deletion", func() {
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		branch := freshness.BranchName(fetch())
		Expect(git.CreateBranch(ctx, branch, base)).To(Succeed())
		_, err = git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: mainBranch, Title: "someone"})
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed()) // ForeignChangeRequest
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
	})

	It("waits with the deletion while writes are paused", func() {
		drive(remediationv1alpha1.PhaseAwaitingApproval)
		live := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
		live.Spec.Paused = new(true)
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed(), "a pause is a wait, not an error")
		Consistently(gone, "300ms").Should(BeFalse())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
		live.Spec.Paused = new(false)
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("writes nothing to Git while writes are paused", func() {
		live := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
		live.Spec.Paused = new(true)
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		for range 5 {
			_ = reconcileOnce()
		}
		_, err := git.BranchHead(ctx, freshness.BranchName(fetch()))
		Expect(errors.Is(err, providers.ErrNotFound)).To(BeTrue())
		Expect(openRequests()).To(BeEmpty())
	})

	// Issue #87: the kill switch is read right before every write, not only by the freshness check at the start
	// of the reconcile. pauseOnLookup sets spec.paused in the API server when the pull request step (or the close)
	// looks up the change request, which it does after the freshness check passed and before any write.
	Context("when writes are paused right before a write (kill switch)", func() {
		var (
			armed  bool
			writes []string
		)
		setPaused := func(on bool) {
			live := &remediationv1alpha1.RemediationSettings{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
			live.Spec.Paused = new(on)
			Expect(k8sClient.Update(ctx, live)).To(Succeed())
		}
		pausedCondition := func() *metav1.Condition {
			return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionWritesPaused)
		}
		BeforeEach(func() {
			armed, writes = false, nil
			recorder = record.NewFakeRecorder(20)
			git.SetFail(func(op string) error {
				if op == opFind && armed {
					armed = false
					setPaused(true)
				}
				return nil
			})
			git.SetAfter(func(op string) error {
				if !gitReads[op] {
					writes = append(writes, op)
				}
				return nil
			})
		})

		It("stops before each write of the pull request step, keeps the state and resumes where it stopped", func() {
			Expect(reconcileOnce()).To(Succeed()) // finalizer
			for _, write := range []string{opCreateBranch, opCommit, opOpen} {
				before := len(writes)
				armed = true
				res, err := reconcileResult()
				Expect(err).NotTo(HaveOccurred(), "a pause is a wait, not an error")
				Expect(res.RequeueAfter).To(Equal(freshRecheckDelay))
				Expect(writes[before:]).To(BeEmpty(), "paused after the freshness check: no %s", write)
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "the phase is kept")
				Expect(got.Status.Deadline).NotTo(BeNil(), "the deadline is kept")
				Expect(controllerutil.ContainsFinalizer(got, FinalizerName)).To(BeTrue())
				cond := pausedCondition()
				Expect(cond).NotTo(BeNil())
				Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				Expect(cond.Reason).To(Equal(ReasonWritesPaused))
				Expect(recorder.Events).To(Receive(Equal("Normal " + ReasonWritesPaused + " " + messageWritesPaused)))

				// Still paused: nothing is written, and the Event is not repeated.
				Expect(reconcileOnce()).To(Succeed())
				Expect(writes[before:]).To(BeEmpty())
				Expect(recorder.Events).NotTo(Receive())

				setPaused(false)
				Expect(reconcileOnce()).To(Succeed())
				Expect(writes[before:]).To(Equal([]string{write}), "the step goes on with the write it stopped before")
				Expect(pausedCondition()).To(BeNil())
				Expect(recorder.Events).To(Receive(Equal("Normal " + EventWritesResumed + " " + messageWritesResumed)))
			}
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			Expect(openRequests()).To(HaveLen(1))
		})

		DescribeTable("clears WritesPaused once writes are allowed, also when the next check then fails",
			func(hostErr error, phase remediationv1alpha1.RemediationPhase) {
				Expect(reconcileOnce()).To(Succeed()) // finalizer
				armed = true
				Expect(reconcileOnce()).To(Succeed())
				Expect(pausedCondition()).NotTo(BeNil())
				Expect(recorder.Events).To(Receive(ContainSubstring(ReasonWritesPaused)))

				setPaused(false)
				git.SetFail(func(op string) error {
					if op == opBranchHead { // the freshness check reads it first
						return hostErr
					}
					return nil
				})
				_ = reconcileOnce()
				Expect(writes).To(BeEmpty())
				Expect(fetch().Status.Phase).To(Equal(phase))
				Expect(pausedCondition()).To(BeNil(), "spec.paused is false, so the condition is gone")
				Expect(recorder.Events).To(Receive(Equal("Normal " + EventWritesResumed + " " + messageWritesResumed)))
			},
			Entry("a failure of the host (5xx)", providers.ErrRetryable, remediationv1alpha1.PhasePolicyChecked),
			Entry("a refusal of the host (403)", providers.ErrForbidden, remediationv1alpha1.PhaseNeedsIntervention),
		)

		It("does not close the pull request of a deleted request when paused between the look-up and the close", func() {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			armed = true
			Expect(reconcileOnce()).To(Succeed())
			Expect(writes).NotTo(ContainElement(opClose))
			// Further requeues while paused close nothing and record no second Event.
			Expect(recorder.Events).To(Receive(ContainSubstring(ReasonWritesPaused)))
			for range 3 {
				res, err := reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(freshRecheckDelay))
			}
			Expect(writes).NotTo(ContainElement(opClose))
			Expect(recorder.Events).NotTo(Receive())
			Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
			Expect(fetch().DeletionTimestamp).NotTo(BeNil(), "the finalizer holds the request")
			Expect(pausedCondition()).NotTo(BeNil())
			Expect(fetch().Status.PullRequest).To(Equal(got.Status.PullRequest))

			setPaused(false)
			Expect(reconcileOnce()).To(Succeed())
			Eventually(gone).Should(BeTrue())
			Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
		})

		DescribeTable("blocks the write when the settings cannot be read right before it (fails closed)",
			func(readErr func() error) {
				Expect(reconcileOnce()).To(Succeed()) // finalizer
				broken := false
				git.SetFail(func(op string) error {
					if op == opFind {
						broken = true
					}
					return nil
				})
				r := reconciler()
				r.APIReader = readerFunc(func(ctx context.Context, key client.ObjectKey, obj client.Object) error {
					if _, ok := obj.(*remediationv1alpha1.RemediationSettings); ok && broken {
						return readErr()
					}
					return k8sClient.Get(ctx, key, obj)
				})
				_, err := r.Reconcile(ctx, key())
				Expect(err).To(MatchError(freshness.ErrWriteBlocked), "returned for a retry with backoff")
				Expect(writes).To(BeEmpty())
				_, err = git.BranchHead(ctx, freshness.BranchName(fetch()))
				Expect(errors.Is(err, providers.ErrNotFound)).To(BeTrue(), "no branch was created")
				Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))

				git.SetFail(nil)
				broken = false
				Expect(reconcileOnce()).To(Succeed())
				Expect(writes).To(Equal([]string{opCreateBranch}))
			},
			Entry("the API server does not answer", func() error { return apierrors.NewServiceUnavailable("down") }),
			Entry("the settings object is gone", func() error {
				return apierrors.NewNotFound(remediationv1alpha1.GroupVersion.WithResource("remediationsettings").GroupResource(),
					remediationv1alpha1.SettingsName)
			}),
		)
	})

	// Issue #181: the target branch can move under the change twice, between cutting the branch and the commit,
	// and while the pull request waits. Both are covered by one rule: the change is rendered at the head of the
	// target branch, and while the pull request waits, again whenever that head moves.
	Context("when the target branch moves under the change", func() {
		BeforeEach(func() {
			faulty = newFaultyGit(git)
			// Build from apps, the parent of the target's directory, as for a Flux Kustomization at apps that
			// lists overlays. The digest covers the build directory, so the plan is digested again, as the
			// policy check would.
			_, err := git.Push(mainBranch, "apps/kustomization.yaml", []byte(
				"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - orders\n"))
			Expect(err).NotTo(HaveOccurred())
			build = testAppsPath
			got := fetch()
			got.Status.Source.BuildPath = build
			got.Status.PlanDigest, err = freshness.ComputeDigest(got, got.Status.Source, policy, settings)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		})

		// addOverlay merges, as someone else would, a second overlay that reuses the patched base under another
		// name: once the pull request merges, its change reaches b-<name> as well. It returns the new head.
		addOverlay := func() string {
			GinkgoHelper()
			_, err := git.Push(mainBranch, "apps/copy/kustomization.yaml", []byte(
				"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamePrefix: b-\nresources:\n  - ../orders\n"))
			Expect(err).NotTo(HaveOccurred())
			head, err := git.Push(mainBranch, "apps/kustomization.yaml", []byte(
				"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - orders\n  - copy\n"))
			Expect(err).NotTo(HaveOccurred())
			return head
		}
		// stoppedWaiting asserts that the request stopped with reason after its pull request was recorded, and
		// that the pull request was closed with nothing pushed to it.
		stoppedWaiting := func(reason string) {
			GinkgoHelper()
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reason))
			for _, text := range []string{"b-", "copy", "namePrefix", "320Mi", "memory"} {
				Expect(cond.Message).NotTo(ContainSubstring(text), "no file content in the status")
			}
			Expect(cond.Message).To(HaveSuffix("the pull request was closed"))
			Expect(cond.Message).NotTo(ContainSubstring("branch was cut"), "the text names the window it was refused in")
			opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
			Expect(opened).NotTo(BeNil())
			Expect(opened.Status).To(Equal(metav1.ConditionTrue), "the pull request was opened")
			Expect(opened.Reason).To(Equal(ReasonPullRequestClosed))
			Expect(opened.Message).To(Equal(fmt.Sprintf("Pull request #%d is closed: the change no longer renders "+
				"as planned (%s)", got.Status.PullRequest.Number, reason)))
			crs := openRequests()
			Expect(crs).To(HaveLen(1), "no other pull request is opened")
			Expect(crs[0].State).To(Equal(providers.ChangeRequestClosed), "a refused pull request is closed")
			Expect(crs[0].HeadSHA).To(Equal(got.Status.PullRequest.HeadSHA), "nothing is pushed or rebuilt")
		}

		It("stops a waiting pull request when a merge adds an overlay that reuses the patched base", func() {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			rendered := got.Status.PullRequest.RenderedTargetHead
			Expect(rendered).NotTo(BeEmpty())

			addOverlay()
			Expect(reconcileOnce()).To(Succeed())
			stoppedWaiting(string(semdiff.ReasonOtherObjectChanged))
			Expect(fetch().Status.PullRequest.RenderedTargetHead).To(Equal(rendered), "a refused head is not recorded")
			Expect(reconcileOnce()).To(Succeed(), "a stopped request takes no further step")
			stoppedWaiting(string(semdiff.ReasonOtherObjectChanged))
		})

		It("stops a waiting pull request when a merge changes another field of the target file", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			// The freshness check still sees the previous limit, but merging would combine this edit with the
			// change, and no render saw that file.
			_, err := git.Push(mainBranch, sourcePath, withMemoryRequest(manifest("256Mi"), "320Mi"))
			Expect(err).NotTo(HaveOccurred())
			Expect(reconcileOnce()).To(Succeed())
			stoppedWaiting(ReasonTargetFileDiverged)
		})

		It("renders at the target branch head when it moved between cutting the branch and the commit", func() {
			for faulty.count(opCreateBranch) == 0 {
				Expect(reconcileOnce()).To(Succeed())
			}
			branch := freshness.BranchName(fetch())
			cut, err := git.BranchHead(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			// The branch was cut before the overlay was merged: its own tree still renders as planned.
			addOverlay()
			got := drive(remediationv1alpha1.PhaseNeedsIntervention)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
			Expect(cond.Reason).To(Equal(string(semdiff.ReasonOtherObjectChanged)))
			Expect(cond.Message).To(ContainSubstring("nothing was committed"))
			head, err := git.BranchHead(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			Expect(head).To(Equal(cut), "nothing was committed")
			Expect(faulty.count(opCommit)).To(BeZero())
			Expect(openRequests()).To(BeEmpty())
		})

		It("reads nothing more from Git while the target branch head stays where it was rendered", func() {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			version := got.ResourceVersion
			for range 3 {
				reads, heads := faulty.count(opReadFile), faulty.count(opBranchHead)
				builds := faulty.count(opReadFile + " " + buildKustomize)
				res, err := reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(freshRecheckInterval))
				// The freshness check's own reads only: the head before and after the target file, and the file.
				Expect(faulty.count(opReadFile)-reads).To(Equal(1), "only the freshness check reads a file")
				Expect(faulty.count(opBranchHead)-heads).To(Equal(2), "only the freshness check reads the head")
				Expect(faulty.count(opReadFile + " " + buildKustomize)).To(Equal(builds))
			}
			Expect(fetch().ResourceVersion).To(Equal(version), "nothing is written either")

			// A merge elsewhere moves the head: the change is rendered once at the new head, which is recorded.
			moved, err := git.Push(mainBranch, "README.yaml", []byte("a: 2\n"))
			Expect(err).NotTo(HaveOccurred())
			builds := faulty.count(opReadFile + " " + buildKustomize)
			Expect(reconcileOnce()).To(Succeed())
			Expect(faulty.count(opReadFile + " " + buildKustomize)).To(BeNumerically(">", builds))
			got = fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			Expect(got.Status.PullRequest.RenderedTargetHead).To(Equal(moved))
		})

		It("retries a failed read of the render, as long as GitHub asks, without stopping the request", func() {
			rendered := drive(remediationv1alpha1.PhaseAwaitingApproval).Status.PullRequest.RenderedTargetHead
			moved, err := git.Push(mainBranch, "README.yaml", []byte("a: 2\n"))
			Expect(err).NotTo(HaveOccurred())
			faulty.failOn(opReadFile+" "+buildKustomize, &providers.RetryableError{
				RetryAfter: 42 * time.Second, Err: errors.New("secondary rate limit")})
			res, err := reconcileResult()
			Expect(err).NotTo(HaveOccurred(), "a delay the host asked for is a wait, not an error")
			Expect(res.RequeueAfter).To(Equal(42 * time.Second))
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			Expect(got.Status.PullRequest.RenderedTargetHead).To(Equal(rendered), "a head not rendered is not recorded")

			faulty.failOn("", nil)
			Expect(reconcileOnce()).To(Succeed())
			Expect(fetch().Status.PullRequest.RenderedTargetHead).To(Equal(moved))
		})

		// cutBranch reconciles until the branch exists and returns the commit it was cut at.
		cutBranch := func() (string, string) {
			GinkgoHelper()
			for faulty.count(opCreateBranch) == 0 {
				Expect(reconcileOnce()).To(Succeed())
			}
			branch := freshness.BranchName(fetch())
			cut, err := git.BranchHead(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			return branch, cut
		}
		// stoppedBeforeCommit asserts that the request stopped with reason before any commit or pull request.
		stoppedBeforeCommit := func(branch, cut, reason string) {
			GinkgoHelper()
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reason))
			Expect(cond.Message).To(HaveSuffix("nothing was committed"))
			for _, text := range []string{"320Mi", "268435456", "memory"} {
				Expect(cond.Message).NotTo(ContainSubstring(text), "no file content in the status")
			}
			head, err := git.BranchHead(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			Expect(head).To(Equal(cut), "nothing was committed")
			Expect(faulty.count(opCommit)).To(BeZero())
			Expect(openRequests()).To(BeEmpty())
		}

		It("commits and opens the pull request when an unrelated merge moved the target branch after the cut", func() {
			branch, cut := cutBranch()
			moved, err := git.Push(mainBranch, "README.yaml", []byte("a: 2\n"))
			Expect(err).NotTo(HaveOccurred())
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "nothing is refused")
			Expect(faulty.count(opCommit)).To(Equal(1))
			crs := openRequests()
			Expect(crs).To(HaveLen(1))
			Expect(crs[0].State).To(Equal(providers.ChangeRequestOpen))
			file, err := git.ReadFile(ctx, crs[0].HeadSHA, sourcePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(file.Data).To(Equal(manifest("384Mi")))
			Expect(cut).NotTo(Equal(moved), "the branch was cut before the merge")
			Expect(crs[0].HeadBranch).To(Equal(branch))
		})

		It("stops before the commit when another field of the target file was edited after the cut", func() {
			branch, cut := cutBranch()
			// The freshness check still sees the previous limit, and the edit alone renders fine; merging would
			// combine it with the change, which no render saw.
			_, err := git.Push(mainBranch, sourcePath, withMemoryRequest(manifest("256Mi"), "320Mi"))
			Expect(err).NotTo(HaveOccurred())
			Expect(reconcileOnce()).To(Succeed())
			stoppedBeforeCommit(branch, cut, ReasonTargetFileDiverged)
		})

		It("stops before the commit when the previous value is spelled otherwise after the cut", func() {
			// The patcher reads 268435456 as 256Mi, so the planned change of the edited file is byte for byte the
			// commit; only the file the branch was cut from tells the two apart.
			branch, cut := cutBranch()
			_, err := git.Push(mainBranch, sourcePath, manifest("268435456"))
			Expect(err).NotTo(HaveOccurred())
			Expect(reconcileOnce()).To(Succeed())
			stoppedBeforeCommit(branch, cut, ReasonTargetFileDiverged)
		})

		It("retries, without stopping, when the file on the branch cannot be read after the target file was edited", func() {
			branch, cut := cutBranch()
			_, err := git.Push(mainBranch, sourcePath, withMemoryRequest(manifest("256Mi"), "320Mi"))
			Expect(err).NotTo(HaveOccurred())
			// The read of the step, which follows its read of the branch head, not the freshness check's read.
			faulty.failOnAfter(opReadFile+" "+sourcePath, opBranchHead+" "+branch,
				&providers.RetryableError{Err: errors.New("server error")})
			err = reconcileOnce()
			Expect(err).To(HaveOccurred())
			Expect(retryable(err)).To(BeTrue())
			Expect(faulty.faults()).To(Equal(1))
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "a failed read decides nothing")
			Expect(faulty.count(opCommit)).To(BeZero())

			faulty.failOn("", nil)
			Expect(reconcileOnce()).To(Succeed())
			stoppedBeforeCommit(branch, cut, ReasonTargetFileDiverged)
		})

		It("cuts the branch at the head the freshness check found the plan fresh at", func() {
			checked, err := git.BranchHead(ctx, mainBranch)
			Expect(err).NotTo(HaveOccurred())
			// A merge after the freshness check, right before the step looks for the pull request: the step
			// does not read the head again, so the branch starts where the file was checked.
			for !controllerutil.ContainsFinalizer(fetch(), FinalizerName) {
				Expect(reconcileOnce()).To(Succeed())
			}
			faulty.beforeOp(opFind, func() {
				defer GinkgoRecover()
				_, err := git.Push(mainBranch, sourcePath, withMemoryRequest(manifest("256Mi"), "320Mi"))
				Expect(err).NotTo(HaveOccurred())
			})
			heads := faulty.count(opBranchHead + " " + mainBranch)
			Expect(reconcileOnce()).To(Succeed())
			Expect(faulty.count(opCreateBranch)).To(Equal(1))
			cut, err := git.BranchHead(ctx, freshness.BranchName(fetch()))
			Expect(err).NotTo(HaveOccurred())
			Expect(cut).To(Equal(checked))
			Expect(faulty.count(opBranchHead+" "+mainBranch)-heads).To(Equal(2), "only the freshness check reads the head")
		})

		It("fails closed when the freshness check returns no head for a waiting pull request", func() {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			got.Status.PullRequest.RenderedTargetHead = ""
			_, err := reconciler().renderWaiting(ctx, got, freshness.Result{})
			Expect(err).To(MatchError(errNoHead), "no head is never a pass")
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		})

		It("closes a waiting pull request whose target file the patcher now refuses, as PatchRefused", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			// An anchor on the path: the freshness check still reads the previous limit, the patcher refuses.
			_, err := git.Push(mainBranch, sourcePath, bytes.Replace(manifest("256Mi"),
				[]byte("limits:\n"), []byte("limits: &l\n"), 1))
			Expect(err).NotTo(HaveOccurred())
			Expect(reconcileOnce()).To(Succeed())
			stoppedWaiting(ReasonPatchRefused)
		})

		It("leaves a refused waiting pull request open, and says so, when the settings no longer reach its repository", func() {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			addOverlay()
			// The freshness check reads the target file first; the repository leaves the settings right before
			// the step reads the file of the pull request, after the check passed and before the close.
			faulty.beforeOp(opReadFile, func() {
				faulty.beforeOp(opReadFile, func() {
					defer GinkgoRecover()
					live := &remediationv1alpha1.RemediationSettings{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
					live.Spec.Repositories = nil
					Expect(k8sClient.Update(ctx, live)).To(Succeed())
				})
			})
			Expect(reconcileOnce()).To(Succeed())
			after := fetch()
			Expect(after.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			approved := meta.FindStatusCondition(after.Status.Conditions, remediationv1alpha1.ConditionApproved)
			Expect(approved.Reason).To(Equal(string(semdiff.ReasonOtherObjectChanged)))
			Expect(approved.Message).To(HaveSuffix("so the pull request was not closed"))
			Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
			opened := meta.FindStatusCondition(after.Status.Conditions, remediationv1alpha1.ConditionPROpened)
			Expect(opened.Status).To(Equal(metav1.ConditionTrue))
			Expect(opened.Reason).To(Equal(ReasonPullRequestOpened), "the pull request was not closed")
			Expect(opened.Message).To(Equal(fmt.Sprintf("Pull request #%d is open", got.Status.PullRequest.Number)))
		})

		It("keeps a waiting pull request open and retries when closing it after a refusal fails", func() {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			addOverlay()
			faulty.failOn(opClose, &providers.RetryableError{Err: errors.New("server error")})
			err := reconcileOnce()
			Expect(err).To(HaveOccurred())
			Expect(retryable(err)).To(BeTrue())
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "a failed close is retried")
			Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen))

			faulty.failOn("", nil)
			Expect(reconcileOnce()).To(Succeed())
			stoppedWaiting(string(semdiff.ReasonOtherObjectChanged))
			Expect(fetch().Status.PullRequest.HeadSHA).To(Equal(got.Status.PullRequest.HeadSHA))
		})

		It("opens the repository once for the freshness check and the render of a waiting pull request", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			_, err := git.Push(mainBranch, "README.yaml", []byte("a: 2\n"))
			Expect(err).NotTo(HaveOccurred())
			opens.Store(0)
			Expect(reconcileOnce()).To(Succeed())
			// One for the look at the pull request, one for the freshness check, which the render reuses.
			Expect(opens.Load()).To(Equal(int32(2)))
		})
	})

	// The failure cases of design section 14 for the pull request step: a crash between an external call and
	// the status write, Git host outages, and a target file that changes while the request waits.
	Context("when something fails on the way", func() {
		BeforeEach(func() {
			faulty = newFaultyGit(git)
		})

		// waiting reports that the request is still where the step left it before the failure: in
		// PolicyChecked, with no pull request recorded and no condition saying why none was opened.
		waiting := func() {
			got := fetch()
			ExpectWithOffset(1, got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
			ExpectWithOffset(1, got.Status.PullRequest).To(BeNil())
			ExpectWithOffset(1, meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)).
				To(BeNil(), "a failure worth retrying writes no outcome")
		}
		// loseStatusWrites reconciles with a client whose status writes fail from the moment a pull request
		// exists, until one does: the pull request is open and the request does not know it.
		loseStatusWrites := func() {
			r := reconciler()
			r.Client = &statusFailer{Client: k8sClient, fail: func() bool { return len(git.ChangeRequests()) > 0 }}
			for range 10 {
				_, _ = r.Reconcile(ctx, key())
				if len(openRequests()) == 1 {
					break
				}
			}
			ExpectWithOffset(1, openRequests()).To(HaveLen(1))
			waiting()
		}
		// hitAfter makes op fail with fault where it directly follows a call starting with after ("" for any),
		// and reconciles until a reconcile got that fault (with a nil fault: called op), returning its result.
		hitAfter := func(op, after string, fault error) (reconcile.Result, error) {
			faulty.failOnAfter(op, after, fault)
			for range 10 {
				calls, faults := faulty.count(op), faulty.faults()
				res, err := reconcileResult()
				if (fault == nil && faulty.count(op) > calls) || (fault != nil && faulty.faults() > faults) {
					return res, err
				}
			}
			Fail("the step never called " + op)
			return reconcile.Result{}, nil
		}
		hit := func(op string, fault error) (reconcile.Result, error) { return hitAfter(op, "", fault) }

		Context("between opening the pull request and writing status", func() {
			It("finds the pull request by its branch and marker after a crash, and opens no second one", func() {
				faulty.crashAfter(opOpen)
				for range 10 {
					if err := reconcileOnce(); err != nil {
						break
					}
				}
				Expect(faulty.count(opOpen)).To(Equal(1))
				Expect(openRequests()).To(HaveLen(1), "the pull request exists although the executor never saw it")
				waiting()

				got := drive(remediationv1alpha1.PhaseAwaitingApproval)
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				Expect(faulty.count(opOpen)).To(Equal(1), "the restart adopted the pull request instead of asking for one")
				crs := openRequests()
				Expect(crs).To(HaveLen(1))
				Expect(hasMarker(crs[0].Body, got)).To(BeTrue())
				main, err := git.BranchHead(ctx, mainBranch)
				Expect(err).NotTo(HaveOccurred())
				Expect(got.Status.PullRequest).To(Equal(&remediationv1alpha1.PullRequestStatus{
					Number: crs[0].Number, URL: crs[0].URL, HeadSHA: crs[0].HeadSHA, RenderedTargetHead: main}))
			})

			It("adopts the pull request after the status write that records it failed, and opens no second one", func() {
				loseStatusWrites()
				got := drive(remediationv1alpha1.PhaseAwaitingApproval)
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				Expect(faulty.count(opOpen)).To(Equal(1))
				Expect(got.Status.PullRequest.Number).To(Equal(openRequests()[0].Number))
			})

			It("closes the pull request it finds by its marker when the deadline passes first", func() {
				loseStatusWrites()
				clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
				Expect(reconcileOnce()).To(Succeed())
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
					To(Equal(ReasonPullRequestDeadline))
				Expect(openRequests()).To(HaveLen(1))
				Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestClosed), "no open pull request is left behind")
				Expect(faulty.count(opOpen)).To(Equal(1))
			})

			DescribeTable("expires with a clear message when the close after the deadline is refused",
				func(op string, class error, reason string) {
					loseStatusWrites()
					clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
					faulty.failOn(op, fmt.Errorf("status 403 from https://github.com/example/fleet: %w", class))
					res, err := reconcileResult()
					Expect(err).NotTo(HaveOccurred(), "a final refusal is not handed back for a retry")
					Expect(res).To(Equal(reconcile.Result{}))
					got := fetch()
					Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
					c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
					Expect(c.Reason).To(Equal(ReasonPullRequestDeadline))
					Expect(c.Message).To(ContainSubstring(reason))
					Expect(c.Message).To(ContainSubstring("close it by hand"))
					Expect(c.Message).NotTo(ContainSubstring("https://"))
					Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen), "the refused close left it open")
				},
				Entry("finding it (403)", opFind, providers.ErrForbidden, ReasonGitForbidden),
				Entry("closing it (403)", opClose, providers.ErrForbidden, ReasonGitForbidden),
				Entry("closing it (422)", opClose, providers.ErrRejected, ReasonGitRejected),
			)

			It("closes the pull request it finds by its marker when the target file changes first", func() {
				loseStatusWrites()
				_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
				Expect(err).NotTo(HaveOccurred())
				Expect(reconcileOnce()).To(Succeed())
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh).Reason).
					To(Equal(freshness.ReasonTargetFileChanged))
				Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
				for range 3 {
					Expect(reconcileOnce()).To(Succeed())
				}
				Expect(openRequests()).To(HaveLen(1))
				Expect(faulty.count(opOpen)).To(Equal(1))
			})
		})

		Context("when GitHub fails", func() {
			// Each call to the Git host, in the freshness check and in the step. The freshness check reads the
			// head and the target file of the target branch first; the step's own reads of the pull request
			// branch are hit by the call they follow.
			steps := []TableEntry{
				Entry("reading the target branch head in the freshness check", opBranchHead, ""),
				Entry("reading the target file in the freshness check", opReadFile+" "+sourcePath, ""),
				Entry("finding the pull request", opFind, ""),
				Entry("reading the head of the pull request branch", opBranchHead, opFind),
				Entry("creating the branch", opCreateBranch, ""),
				Entry("reading the target file on the pull request branch", opReadFile+" "+sourcePath,
					opBranchHead+" shadewright/"),
				Entry("reading the files of the render check", opReadFile+" "+buildKustomize, ""),
				Entry("committing", opCommit, ""),
				Entry("opening the pull request", opOpen, ""),
			}

			DescribeTable("retries a server error with backoff, writes nothing and then opens one pull request",
				func(op, after string) {
					// The adapter's error text is never trusted: it could hold a URL with a token.
					leak := "status 502 from https://github.com/example/fleet with token ghs_leaked"
					_, err := hitAfter(op, after, &providers.RetryableError{Err: errors.New(leak)})
					Expect(err).To(HaveOccurred(), "an error goes to the work queue's backoff")
					Expect(retryable(err)).To(BeTrue(), "a server error is classed as retryable: %v", err)
					Expect(err.Error()).NotTo(ContainSubstring("ghs_leaked"))
					Expect(err.Error()).NotTo(ContainSubstring("https://"))
					waiting()

					faulty.failOn("", nil)
					got := drive(remediationv1alpha1.PhaseAwaitingApproval)
					Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
					Expect(openRequests()).To(HaveLen(1))
				}, steps)

			DescribeTable("waits as long as GitHub asks (429 or a secondary rate limit with Retry-After)",
				func(op, after string) {
					limited := &providers.RetryableError{RetryAfter: 42 * time.Second, Err: errors.New("secondary rate limit")}
					res, err := hitAfter(op, after, limited)
					Expect(err).NotTo(HaveOccurred(), "a delay the host asked for is a wait, not an error")
					Expect(res.RequeueAfter).To(Equal(42*time.Second), "neither sooner nor later than asked")
					waiting()
				}, steps)

			// A refusal GitHub gives for good (a 403 that is no rate limit, a 422 the adapter cannot resolve) stops
			// the request at once, with a fixed reason, long before its deadline. The render check's reads are not
			// in the table: semdiff reports any failed read as transient.
			refusals := make([]TableEntry, 0, 16)
			for _, c := range []struct{ name, op, after string }{
				{"reading the target branch head in the freshness check", opBranchHead, ""},
				{"reading the target file in the freshness check", opReadFile + " " + sourcePath, ""},
				{"finding the pull request", opFind, ""},
				{"reading the head of the pull request branch", opBranchHead, opFind},
				{"creating the branch", opCreateBranch, ""},
				{"reading the target file on the pull request branch", opReadFile + " " + sourcePath,
					opBranchHead + " shadewright/"},
				{"committing", opCommit, ""},
				{"opening the pull request", opOpen, ""},
			} {
				refusals = append(refusals,
					Entry(c.name+" (403)", c.op, c.after, providers.ErrForbidden, ReasonGitForbidden),
					Entry(c.name+" (422)", c.op, c.after, providers.ErrRejected, ReasonGitRejected))
			}
			DescribeTable("stops in NeedsIntervention at once when GitHub refuses for good (403 or 422)",
				func(op, after string, class error, reason string) {
					leak := fmt.Errorf("status 403 from https://github.com/example/fleet with token ghs_leaked: %w", class)
					res, err := hitAfter(op, after, leak)
					Expect(err).NotTo(HaveOccurred(), "a final refusal is not handed back for a retry")
					Expect(res).To(Equal(reconcile.Result{}), "and not requeued")
					got := fetch()
					Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
					Expect(got.Status.Deadline.Time).To(BeTemporally(">", clock), "the deadline has not passed")
					cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
					Expect(cond).NotTo(BeNil())
					Expect(cond.Status).To(Equal(metav1.ConditionFalse))
					Expect(cond.Reason).To(Equal(reason))
					Expect(cond.Message).NotTo(ContainSubstring("ghs_leaked"))
					Expect(cond.Message).NotTo(ContainSubstring("https://"))
					Expect(cond.Message).NotTo(ContainSubstring("403"))

					before := faulty.creations()
					faulty.failOn("", nil)
					for range 3 {
						res, err := reconcileResult()
						Expect(err).NotTo(HaveOccurred())
						Expect(res).To(Equal(reconcile.Result{}))
					}
					Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention), "the stop is final")
					Expect(faulty.creations()).To(Equal(before), "nothing more is written to Git")
					Expect(openRequests()).To(BeEmpty())
				}, refusals)

			It("does not record a pull request someone else opened on the branch while it was opening its own", func() {
				// GitHub answers the create with 422 (one open pull request per head and base) and the adapter
				// returns the open one: it is recorded only when it carries this request's marker.
				faulty.beforeOp(opOpen, func() {
					_, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(fetch()),
						BaseBranch: mainBranch, Title: "someone else's change", Body: "no marker"})
					Expect(err).NotTo(HaveOccurred())
				})
				_, err := hit(opOpen, nil)
				Expect(err).NotTo(HaveOccurred())
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
				Expect(got.Status.PullRequest).To(BeNil())
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
					To(Equal(ReasonForeignChangeRequest))
				crs := openRequests()
				Expect(crs).To(HaveLen(1))
				Expect(crs[0].State).To(Equal(providers.ChangeRequestOpen), "someone else's pull request is not touched")
			})

			It("treats a response that never arrives as a timeout, then adopts the pull request it opened", func() {
				writeTimeout = 200 * time.Millisecond
				faulty.blockAfter(opOpen)
				var err error
				for range 10 {
					if err = reconcileOnce(); err != nil {
						break
					}
				}
				Expect(errors.Is(err, ErrTimeout)).To(BeTrue(), "the step's own deadline is a retryable timeout: %v", err)
				Expect(openRequests()).To(HaveLen(1))
				waiting()

				faulty.blockAfter("")
				writeTimeout = 0 // the later steps get the default deadline again
				got := drive(remediationv1alpha1.PhaseAwaitingApproval)
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				Expect(faulty.count(opOpen)).To(Equal(1))
			})

			It("retries a conflict on the commit and lets the branch decide the outcome", func() {
				// Someone pushes another value to the branch just before the commit, which then fails on the
				// expected head: a conflict is not final by itself, the next reconcile reads the branch again.
				faulty.beforeOp(opCommit, func() {
					_, err := git.Push(freshness.BranchName(fetch()), sourcePath, manifest("1Gi"))
					Expect(err).NotTo(HaveOccurred())
				})
				_, err := hit(opCommit, nil)
				Expect(errors.Is(err, providers.ErrConflict)).To(BeTrue(), "%v", err)
				waiting()

				Expect(reconcileOnce()).To(Succeed())
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
					To(Equal(ReasonBranchDiverged))
				Expect(openRequests()).To(BeEmpty())
			})

			It("ends the request without a retry when the repository settings can never open a provider", func() {
				openErr = fmt.Errorf("client ID: %w", providers.ErrInvalidConfig)
				res, err := reconcileResult()
				Expect(err).NotTo(HaveOccurred(), "a permanent refusal is not retried")
				Expect(res).To(Equal(reconcile.Result{}))
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh).Reason).
					To(Equal(reasonRepositoryUnusable))
			})

			DescribeTable("gives up with Expired at the deadline while GitHub keeps failing",
				func(op string, fault error, wait time.Duration) {
					res, err := hit(op, fault)
					if wait > 0 {
						Expect(err).NotTo(HaveOccurred())
						// The clock has not moved since the decision: the whole step's time is left, and a longer
						// delay is cut to it, so the request expires on time.
						Expect(res.RequeueAfter).To(Equal(min(wait, DefaultPullRequestTimeout)))
					} else {
						Expect(retryable(err)).To(BeTrue())
					}
					waiting()
					Expect(fetch().Status.Deadline).NotTo(BeNil(), "the deadline is stored with the decision")
					clock = clock.Add(DefaultPullRequestTimeout + time.Second)
					before := faulty.creations()
					res, err = reconcileResult()
					Expect(err).NotTo(HaveOccurred())
					Expect(res).To(Equal(reconcile.Result{}), "an expired request is not retried")
					got := fetch()
					Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
					cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
					Expect(cond.Status).To(Equal(metav1.ConditionFalse))
					Expect(cond.Reason).To(Equal(ReasonPullRequestDeadline))
					Expect(faulty.creations()).To(Equal(before), "no branch, commit or pull request after the deadline")
					Expect(openRequests()).To(BeEmpty())
				},
				Entry("server errors on the open", opOpen, &providers.RetryableError{Err: errors.New("status 503")},
					time.Duration(0)),
				Entry("a rate limit with a long Retry-After on the open", opOpen, &providers.RetryableError{
					RetryAfter: time.Hour, Err: errors.New("rate limit")}, time.Hour),
				// The freshness check fails from the first reconcile after the policy check, before any Git write.
				Entry("server errors in every freshness check", opBranchHead,
					&providers.RetryableError{Err: errors.New("status 502")}, time.Duration(0)),
			)

			It("stores the deadline in the status write that decides the request", func() {
				got := fetch()
				Expect(got.Status.Deadline).NotTo(BeNil())
				Expect(got.Status.Deadline.Time).To(BeTemporally("==", clock.Add(DefaultPullRequestTimeout)))
			})

			It("supersedes a plan that is stale right after the decision on the next reconcile", func() {
				_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
				Expect(err).NotTo(HaveOccurred())
				res, err := reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				Expect(res).To(Equal(reconcile.Result{}))
				Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded), "no other write comes first")
			})

			It("does not start the deadline while writes are paused, so a long pause expires nothing", func() {
				pause := func(on bool) {
					live := &remediationv1alpha1.RemediationSettings{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
					live.Spec.Paused = new(on)
					Expect(k8sClient.Update(ctx, live)).To(Succeed())
				}
				// Decide the request again, this time while writes are paused.
				pause(true)
				got := fetch()
				got.Status = remediationv1alpha1.RemediationRequestStatus{}
				Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
				Expect(reconcileOnce()).To(Succeed())
				got = fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
				Expect(got.Status.Deadline).To(BeNil(), "no deadline is started during a pause")
				for range 3 {
					res, err := reconcileResult()
					Expect(err).NotTo(HaveOccurred(), "a pause is a wait, not an error")
					Expect(res.RequeueAfter).To(Equal(freshRecheckDelay))
				}
				Expect(fetch().Status.Deadline).To(BeNil())
				clock = clock.Add(2 * DefaultPullRequestTimeout)
				pause(false)
				Expect(reconcileOnce()).To(Succeed())
				Expect(fetch().Status.Deadline.Time).To(BeTemporally("==", clock.Add(DefaultPullRequestTimeout)),
					"the deadline starts once the pause has ended")
				got = drive(remediationv1alpha1.PhaseAwaitingApproval)
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				Expect(openRequests()).To(HaveLen(1))
			})

			It("writes nothing to Git when the deadline passes while the freshness check runs", func() {
				Expect(reconcileOnce()).To(Succeed()) // finalizer
				// The freshness check is slow: the deadline passes before it returns a fresh verdict.
				faulty.beforeOp(opBranchHead, func() { clock = clock.Add(DefaultPullRequestTimeout + time.Minute) })
				Expect(reconcileOnce()).To(Succeed())
				Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
				Expect(faulty.creations()).To(BeZero())
			})
		})

		Context("when the target file changes while the request waits", func() {
			It("supersedes the request during review, closes its pull request and opens no other", func() {
				got := drive(remediationv1alpha1.PhaseAwaitingApproval)
				branch := freshness.BranchName(got)
				head, err := git.BranchHead(ctx, branch)
				Expect(err).NotTo(HaveOccurred())
				_, err = git.Push(mainBranch, sourcePath, manifest("512Mi"))
				Expect(err).NotTo(HaveOccurred())
				Expect(reconcileOnce()).To(Succeed())
				got = fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh).Reason).
					To(Equal(freshness.ReasonTargetFileChanged))
				Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
				opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
				Expect(opened.Status).To(Equal(metav1.ConditionTrue), "the pull request was opened")
				Expect(opened.Reason).To(Equal(ReasonPullRequestClosed))
				Expect(opened.Message).To(Equal(fmt.Sprintf("Pull request #%d is closed: the plan is stale (%s)",
					got.Status.PullRequest.Number, freshness.ReasonTargetFileChanged)),
					"the condition no longer says the pull request is open")
				for range 3 {
					Expect(reconcileOnce()).To(Succeed())
				}
				Expect(openRequests()).To(HaveLen(1))
				Expect(faulty.count(opOpen)).To(Equal(1))
				after, err := git.BranchHead(ctx, branch)
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(head), "the branch is never rebuilt")
			})

			It("supersedes the request before the commit when the file changes after the branch was cut", func() {
				for faulty.count(opCreateBranch) == 0 {
					Expect(reconcileOnce()).To(Succeed())
				}
				branch := freshness.BranchName(fetch())
				cut, err := git.BranchHead(ctx, branch)
				Expect(err).NotTo(HaveOccurred())
				_, err = git.Push(mainBranch, sourcePath, manifest("512Mi"))
				Expect(err).NotTo(HaveOccurred())
				Expect(reconcileOnce()).To(Succeed())
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh).Reason).
					To(Equal(freshness.ReasonTargetFileChanged))
				head, err := git.BranchHead(ctx, branch)
				Expect(err).NotTo(HaveOccurred())
				Expect(head).To(Equal(cut), "nothing was committed")
				Expect(faulty.count(opCommit)).To(BeZero())
				Expect(openRequests()).To(BeEmpty())
			})

			It("stops before the commit when the file still holds the previous value but the change no longer renders as planned", func() {
				// A memory request above the planned limit: the freshness check still sees the previous limit,
				// the render check refuses the changed Deployment.
				_, err := git.Push(mainBranch, sourcePath, withMemoryRequest(manifest("256Mi"), "512Mi"))
				Expect(err).NotTo(HaveOccurred())
				got := drive(remediationv1alpha1.PhaseNeedsIntervention)
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
				cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal(string(semdiff.ReasonSchemaInvalid)))
				Expect(cond.Message).NotTo(ContainSubstring("512Mi"), "no file content in the status")
				Expect(faulty.count(opCommit)).To(BeZero())
				Expect(openRequests()).To(BeEmpty())
			})

			It("opens the pull request when the same edit keeps the request below the planned limit", func() {
				// The control of the spec above: the edited file itself is fine, only the request above the
				// planned limit is refused.
				_, err := git.Push(mainBranch, sourcePath, withMemoryRequest(manifest("256Mi"), "320Mi"))
				Expect(err).NotTo(HaveOccurred())
				got := drive(remediationv1alpha1.PhaseAwaitingApproval)
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				file, err := git.ReadFile(ctx, got.Status.PullRequest.HeadSHA, sourcePath)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(file.Data)).To(ContainSubstring("memory: 384Mi"))
				Expect(string(file.Data)).To(ContainSubstring("memory: 320Mi"))
			})
		})
	})
})

// statusFailer fails status writes while fail returns true, as a lost write after a successful Git call.
type statusFailer struct {
	client.Client
	fail func() bool
}

func (s *statusFailer) Status() client.SubResourceWriter {
	return &failingStatus{SubResourceWriter: s.Client.Status(), fail: s.fail}
}

type failingStatus struct {
	client.SubResourceWriter
	fail func() bool
}

func (f *failingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if f.fail() {
		return errors.New("status write lost")
	}
	return f.SubResourceWriter.Update(ctx, obj, opts...)
}

// Operation names of faultyGit. A ReadFile fault or count may name the path too: "ReadFile <path>".
const (
	opFind         = "FindChangeRequest"
	opCreateBranch = "CreateBranch"
	opReadFile     = "ReadFile"
)

// faultyGit wraps the fake provider. It counts every call and injects faults the fake's hook cannot express:
// a fault for one path or for the call that follows another, a crash or a response that never arrives after a
// call took effect, and a change made by someone else right before a call.
type faultyGit struct {
	*fakegit.Git

	mu     sync.Mutex
	calls  map[string]int
	failOp string
	after  string
	fault  error
	hits   int
	last   string
	crash  string
	block  string
	before map[string]func()
}

func newFaultyGit(g *fakegit.Git) *faultyGit {
	return &faultyGit{Git: g, calls: map[string]int{}, before: map[string]func(){}}
}

// failOn makes op ("ReadFile <path>" for one file) fail with fault before it runs; a nil fault fails nothing.
func (f *faultyGit) failOn(op string, fault error) { f.failOnAfter(op, "", fault) }

// failOnAfter is failOn for the calls of op that directly follow a call whose "op arg" starts with after, so a
// fault can hit the pull request step's own read and not the freshness check's read of the same file.
func (f *faultyGit) failOnAfter(op, after string, fault error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOp, f.after, f.fault = op, after, fault
}

// faults returns how many injected faults were returned.
func (f *faultyGit) faults() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// crashAfter makes the next successful call of op return an error, as a crash after the call took effect.
func (f *faultyGit) crashAfter(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.crash = op
}

// blockAfter makes every successful call of op wait for the end of its context and then report a timeout,
// as a response that never arrives although the host did the work; "" stops it.
func (f *faultyGit) blockAfter(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = op
}

// beforeOp runs do once, right before the next call of op.
func (f *faultyGit) beforeOp(op string, do func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before[op] = do
}

// count returns how often op ("ReadFile <path>" for one file) was called, failed calls included.
func (f *faultyGit) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// creations returns how many branch, commit and pull request writes were attempted. Closing a change request
// is not counted: expiry may close an unrecorded pull request of its own.
func (f *faultyGit) creations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[opCreateBranch] + f.calls[opCommit] + f.calls[opOpen]
}

func (f *faultyGit) enter(op, arg string) error {
	f.mu.Lock()
	f.calls[op]++
	f.calls[op+" "+arg]++
	do := f.before[op]
	delete(f.before, op)
	fault := f.fault
	if (f.failOp != op && f.failOp != op+" "+arg) || !strings.HasPrefix(f.last, f.after) {
		fault = nil
	}
	if fault != nil {
		f.hits++
	}
	f.last = op + " " + arg
	f.mu.Unlock()
	if do != nil {
		do()
	}
	return fault
}

func (f *faultyGit) leave(ctx context.Context, op string, err error) error {
	if err != nil {
		return err
	}
	f.mu.Lock()
	crash, block := f.crash == op, f.block == op
	if crash {
		f.crash = ""
	}
	f.mu.Unlock()
	switch {
	case crash:
		return errors.New("connection lost after the call")
	case block:
		<-ctx.Done()
		// What the adapter returns when the caller's deadline ends the call.
		return &providers.RetryableError{Err: fmt.Errorf("%s: %w", op, context.DeadlineExceeded)}
	}
	return nil
}

func (f *faultyGit) ReadFile(ctx context.Context, sha, path string) (providers.File, error) {
	if err := f.enter(opReadFile, path); err != nil {
		return providers.File{}, err
	}
	file, err := f.Git.ReadFile(ctx, sha, path)
	return file, f.leave(ctx, opReadFile, err)
}

func (f *faultyGit) BranchHead(ctx context.Context, branch string) (string, error) {
	if err := f.enter(opBranchHead, branch); err != nil {
		return "", err
	}
	sha, err := f.Git.BranchHead(ctx, branch)
	return sha, f.leave(ctx, opBranchHead, err)
}

func (f *faultyGit) CreateBranch(ctx context.Context, branch, from string) error {
	if err := f.enter(opCreateBranch, branch); err != nil {
		return err
	}
	return f.leave(ctx, opCreateBranch, f.Git.CreateBranch(ctx, branch, from))
}

func (f *faultyGit) CommitFile(ctx context.Context, branch, expectedHead string, ch providers.FileChange) (string, error) {
	if err := f.enter(opCommit, branch); err != nil {
		return "", err
	}
	sha, err := f.Git.CommitFile(ctx, branch, expectedHead, ch)
	return sha, f.leave(ctx, opCommit, err)
}

func (f *faultyGit) OpenChangeRequest(ctx context.Context, req providers.NewChangeRequest) (providers.ChangeRequest, error) {
	if err := f.enter(opOpen, req.HeadBranch); err != nil {
		return providers.ChangeRequest{}, err
	}
	cr, err := f.Git.OpenChangeRequest(ctx, req)
	if err = f.leave(ctx, opOpen, err); err != nil {
		return providers.ChangeRequest{}, err
	}
	return cr, nil
}

func (f *faultyGit) FindChangeRequest(ctx context.Context, branch string) (providers.ChangeRequest, error) {
	if err := f.enter(opFind, branch); err != nil {
		return providers.ChangeRequest{}, err
	}
	cr, err := f.Git.FindChangeRequest(ctx, branch)
	if err = f.leave(ctx, opFind, err); err != nil {
		return providers.ChangeRequest{}, err
	}
	return cr, nil
}

func (f *faultyGit) CloseChangeRequest(ctx context.Context, n int64) error {
	if err := f.enter(opClose, ""); err != nil {
		return err
	}
	return f.leave(ctx, opClose, f.Git.CloseChangeRequest(ctx, n))
}

// retryable reports whether err is one the reconciler hands back for a retry: a Git host failure worth
// retrying, a transient read of the render check, or the step's own deadline.
func retryable(err error) bool {
	return errors.Is(err, providers.ErrRetryable) || errors.Is(err, sourcemap.ErrTransient) || errors.Is(err, ErrTimeout)
}

// withMemoryRequest sets a memory request on the worker container of a deploymentManifest, through the YAML
// tree rather than by text.
func withMemoryRequest(manifest []byte, request string) []byte {
	GinkgoHelper()
	doc, err := kyaml.Parse(string(manifest))
	Expect(err).NotTo(HaveOccurred())
	_, err = doc.Pipe(
		kyaml.LookupCreate(kyaml.MappingNode, "spec", "template", "spec", "containers", "[name="+workerContainer+"]",
			"resources", "requests"),
		kyaml.SetField("memory", kyaml.NewStringRNode(request)))
	Expect(err).NotTo(HaveOccurred())
	out, err := doc.String()
	Expect(err).NotTo(HaveOccurred())
	return []byte(out)
}

var _ = Describe("Pull request body", func() {
	It("carries no secret and keeps the marker exact", func() {
		rr := bodyRequest()
		// Names are validated by the schema; the body is redacted anyway, so a token that slipped through a
		// future field never reaches the Git host.
		rr.Spec.IncidentRef.Name = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
		body := pullRequestBody(rr, "")
		Expect(body).NotTo(ContainSubstring("eyJhbGciOiJIUzI1NiJ9"))
		Expect(body).To(ContainSubstring("[REDACTED"))
		Expect(hasMarker(body, rr)).To(BeTrue())
		Expect(body).NotTo(ContainSubstring("https://"), "no repository URL in the body")

		other := rr.DeepCopy()
		other.Status.PlanDigest = "sha256:" + strings.Repeat("b", 64)
		Expect(hasMarker(body, other)).To(BeFalse(), "a marker for another plan is not this plan's")
	})

	It("names an Incident called after its fingerprint verbatim and still redacts free text", func() {
		rr := bodyRequest()
		// The collector names an Incident "incident-" plus the 64 hex digits of its fingerprint, a run the
		// redactor would otherwise take for a token.
		name := "incident-9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		rr.Spec.IncidentRef.Name = name
		hexToken := "3f1c9a7be2d84f06a5b1c3e9d7f2a4b6c8e0d1f3"
		rr.Status.Source.Path = remediationv1alpha1.GitFilePath("apps/" + hexToken + "/deployment.yaml")
		body := pullRequestBody(rr, name)
		Expect(body).To(ContainSubstring("for incident `" + name + "`."))
		Expect(body).To(ContainSubstring("incident `" + name + "`, policy `p/1`"))
		Expect(body).NotTo(ContainSubstring(hexToken), "a hex token in other text is still redacted")
		Expect(body).To(ContainSubstring("[REDACTED:token]"))
		Expect(hasMarker(body, rr)).To(BeTrue())
	})

	It("redacts a hex token used as the Incident name when it was not verified", func() {
		hex40 := strings.Repeat("3f1c9a7b", 5)
		for _, token := range []string{
			hex40,                         // a 40-digit hex token, a valid object name
			strings.Repeat("9f86d081", 8), // 64 hex digits without the prefix
			"incident-" + hex40,           // the prefix and 40 hex digits
			"incident-" + strings.Repeat("9f86d081", 8), // the collector's form, not read back
		} {
			rr := bodyRequest()
			rr.Spec.IncidentRef.Name = token
			body := pullRequestBody(rr, "")
			Expect(body).NotTo(ContainSubstring(token), token)
			Expect(body).To(ContainSubstring("for incident `[REDACTED:token]`."), token)
			Expect(hasMarker(body, rr)).To(BeTrue())
		}
	})
})

// bodyRequest is a request with the status fields the pull request body reads.
func bodyRequest() *remediationv1alpha1.RemediationRequest {
	rr := fx.MemoryLimitRequest(requestNamespace, "body")
	rr.UID = "11111111-1111-4111-8111-111111111111"
	rr.Status.PlanDigest = "sha256:" + strings.Repeat("a", 64)
	rr.Status.PolicyRevision = "p/1"
	rr.Status.Source = &remediationv1alpha1.ResolvedSource{Path: sourcePath, TargetBranch: mainBranch}
	return rr
}

// hangingIncidentReader is a reader whose get of an Incident blocks until its context ends, as a stalled API
// server would, and counts those gets in hangs; reads of other kinds go to Reader.
type hangingIncidentReader struct {
	client.Reader
	hangs *atomic.Int32
}

func (h hangingIncidentReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.Incident); ok {
		h.hangs.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}
	return h.Reader.Get(ctx, key, obj, opts...)
}
