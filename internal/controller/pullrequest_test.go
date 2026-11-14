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
	"maps"
	"slices"
	"strconv"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// readmePath is a file of the fake repository besides the target file.
	readmePath = "README.yaml"
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
		// process is the memory of first looks at a missing pull request (goneLooks) that every reconciler of a
		// spec shares, as one executor process would; the other memory of a reconciler starts empty.
		process *goneLooks
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
		r.goneLooks = process
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

	// seenGone records that this process saw pull request n missing confirmWait ago (goneLooks), so the next look
	// that finds it missing again takes it for gone (#688).
	seenGone := func(n int64) {
		process.note(fetch(), n, clock.Add(-confirmWait))
	}
	// firstLook returns the number and the time of the first look at a missing pull request that this process
	// keeps for the request (0 without one).
	firstLook := func() (int64, time.Time) {
		process.mu.Lock()
		defer process.mu.Unlock()
		s, ok := process.keys[client.ObjectKeyFromObject(request)]
		if !ok || s.look == nil {
			return 0, time.Time{}
		}
		return s.look.number, s.look.at
	}
	// lookedAt is the number of firstLook.
	lookedAt := func() int64 {
		n, _ := firstLook()
		return n
	}
	// lockHolders is the number of changeOf calls that hold or wait for the lock of the request (goneLooks.lock).
	lockHolders := func() int {
		process.mu.Lock()
		defer process.mu.Unlock()
		if s, ok := process.keys[client.ObjectKeyFromObject(request)]; ok {
			return s.holders
		}
		return 0
	}

	BeforeEach(func() {
		seq++
		clock = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		build = buildPath
		faulty, openErr, writeTimeout, recorder = nil, nil, 0, nil
		process = &goneLooks{}
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

		file, err := git.ReadFile(ctx, cr.HeadSHA, sourcePath, providers.MaxFileBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Data).To(Equal(manifest("384Mi")), "one value changed, nothing else")
		onMain, err := git.ReadFile(ctx, main, sourcePath, providers.MaxFileBytes)
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

	// revertingRequest turns the spec's request into one that ended Failed with Verified False (OOMKilled) after
	// its pull request #71, and makes its revert the request the spec drives (#720). The live limit and the file
	// on the target branch hold the failed request's value, and the policy allows the revert's decrease.
	revertingRequest := func() *remediationv1alpha1.RemediationRequest {
		failed := fetch()
		failed.Status.Phase = remediationv1alpha1.PhaseFailed
		failed.Status.HoldsTarget = new(false) // its pull request merged, so the revert may take the target
		failed.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 71,
			URL: "https://github.com/example/fleet/pull/71", HeadSHA: strings.Repeat("c", 40)}
		meta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
			Status: metav1.ConditionFalse, Reason: ReasonOOMKilled, Message: "OOM killed under the limit of the failed request"})
		Expect(k8sClient.Status().Update(ctx, failed)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, failed))).To(Succeed()) })

		live := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), live)).To(Succeed())
		live.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("384Mi")
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		pol := &remediationv1alpha1.RemediationPolicy{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), pol)).To(Succeed())
		pol.Spec.Actions[0].SetContainerMemoryLimit.MaxDecreasePercent = new(int32(90))
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
		git = fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: manifest("384Mi"), buildKustomize: buildFile,
			readmePath: []byte("a: 1\n")})

		revert := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace,
			Name: string(remediationv1alpha1.RevertName(remediationv1alpha1.UID(failed.UID)))}}
		revert.Spec = *failed.Spec.DeepCopy()
		m := revert.Spec.Action.SetContainerMemoryLimit
		m.Previous, m.Desired = m.Desired, m.Previous
		revert.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: failed.Name, UID: remediationv1alpha1.UID(failed.UID)}
		revert.Spec.ApprovalRequired = new(true)
		Expect(k8sClient.Create(ctx, revert)).To(Succeed())
		request = revert
		return failed
	}
	revertTitle := func() string {
		return "revert: set the memory limit of container worker of " + fx.TargetNamespace + "/" + deployment.Name +
			" back to 256Mi"
	}

	It("opens a revert's pull request that names what it reverts and shows the revert's name (#720)", func() {
		failed := revertingRequest()
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "conditions %+v", got.Status.Conditions)
		crs := openRequests()
		Expect(crs).To(HaveLen(1))
		Expect(crs[0].Title).To(Equal(revertTitle()))
		Expect(crs[0].Body).To(ContainSubstring("**Reverts:** RemediationRequest `" + requestNamespace + "/" +
			failed.Name + "` (UID `" + string(failed.UID) + "`), pull request #71. " +
			"It ended `Failed` with the reason `OOMKilled`."))
		Expect(crs[0].Body).To(ContainSubstring("**Evidence:** RemediationRequest `" + requestNamespace + "/" +
			got.Name + "` (UID"))
		Expect(crs[0].Body).NotTo(ContainSubstring("[REDACTED"))
		Expect(crs[0].Body).NotTo(ContainSubstring("revert this pull request;"))
		file, err := git.ReadFile(ctx, crs[0].HeadSHA, sourcePath, providers.MaxFileBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Data).To(Equal(manifest("256Mi")))
	})

	It("finds a revert's pull request opened under the older title after a restart, and opens no second one (#720)", func() {
		revertingRequest()
		// The executor crashes right after the open, before it records the pull request; an older version had
		// opened it under the title of a first fix.
		crashed := false
		git.SetAfter(func(op string) error {
			if op == opOpen && !crashed {
				crashed = true
				return errors.New("crash after the open")
			}
			return nil
		})
		for range 10 {
			_ = reconcileOnce()
			if crashed {
				break
			}
		}
		crs := openRequests()
		Expect(crs).To(HaveLen(1))
		Expect(fetch().Status.PullRequest).To(BeNil())
		oldTitle := "fix: set the memory limit of container worker of " + fx.TargetNamespace + "/" + deployment.Name +
			" to 256Mi"
		Expect(git.UpdateChangeRequest(ctx, crs[0].Number, oldTitle, crs[0].Body)).To(Succeed())

		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(openRequests()).To(HaveLen(1))
		Expect(got.Status.PullRequest.Number).To(Equal(crs[0].Number))
		Expect(crs[0].HeadBranch).To(Equal("shadewright/" + string(got.UID)))
		Expect(hasMarker(openRequests()[0].Body, got)).To(BeTrue())
	})

	It("retries the open of a revert's pull request while the request it reverts cannot be read (#720)", func() {
		failed := revertingRequest()
		// The read fails from the commit on, after the link check has passed: no pull request is opened with a
		// body that leaves out what it reverts, and the request is not ended.
		fail := &atomic.Bool{}
		apiReader = switchedRequestReader{Reader: k8sClient, name: failed.Name, fail: fail}
		git.SetAfter(func(op string) error {
			if op == opCommit {
				fail.Store(true)
			}
			return nil
		})
		var lastErr error
		for range 10 {
			if err := reconcileOnce(); err != nil {
				lastErr = err
			}
		}
		Expect(lastErr).To(MatchError(ContainSubstring("pull request text of a revert")))
		Expect(git.ChangeRequests()).To(BeEmpty())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))

		fail.Store(false)
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(openRequests()).To(HaveLen(1))
		Expect(openRequests()[0].Body).To(ContainSubstring("pull request #71"))
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
		file, err := git.ReadFile(ctx, got.Status.PullRequest.HeadSHA, sourcePath, providers.MaxFileBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Data).To(Equal(manifest("384Mi")))
	})

	It("hands the pull request to a person when someone else pushes to the branch (#524)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(cond.Reason).To(Equal(ReasonBranchDiverged))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)).
			To(BeTrue(), "the pull request is still open")
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen), "the pull request is left for a person")
		Expect(got.Status.HoldsTarget).To(Equal(new(true)), "an open pull request keeps the target")
		comments := git.Comments(got.Status.PullRequest.Number)
		Expect(comments).To(HaveLen(1))
		Expect(comments[0]).To(ContainSubstring("no longer holds only"))
		Expect(reconcileOnce()).To(Succeed())
		Expect(git.Comments(got.Status.PullRequest.Number)).To(HaveLen(1), "one comment only")
	})

	It("posts one comment when the status write after it is lost (#524)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		failStatus := true
		r := reconciler()
		r.Client = &statusFailer{Client: k8sClient, fail: func() bool { return failStatus && len(git.Comments(n)) > 0 }}
		_, _ = r.Reconcile(ctx, key())
		Expect(git.Comments(n)).To(HaveLen(1))
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "the stop was lost")
		failStatus = false
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(git.Comments(n)).To(HaveLen(1), "the comment is not posted again")
	})

	It("ends with MergedWithForeignChange when a pull request with a foreign push is merged (#524)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		_, err = git.Merge(got.Status.PullRequest.Number)
		Expect(err).NotTo(HaveOccurred())
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(cond.Reason).To(Equal(ReasonMergedWithForeignChange))
		Expect(cond.Message).To(HavePrefix(fmt.Sprintf("Pull request #%d was merged", got.Status.PullRequest.Number)))
		Expect(got.Status.HoldsTarget).NotTo(Equal(new(true)), "a merged pull request releases the target")
	})

	It("stops with HeadMoved when the head moves to the same planned change (#524)", func() {
		parent, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(git.CreateBranch(ctx, "copy", parent)).To(Succeed())
		same, err := git.CommitFile(ctx, "copy", parent, providers.FileChange{Path: sourcePath,
			Content: manifest("384Mi"), Message: "the same change"})
		Expect(err).NotTo(HaveOccurred())
		Expect(git.ForcePush(freshness.BranchName(got), same)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			To(Equal(ReasonHeadMoved))
		Expect(git.Comments(got.Status.PullRequest.Number)).To(BeEmpty())
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

	It("refuses a build over the render budget at the policy check, before any plan or approval", func() {
		// Five fillers of 900 KiB: each fits the 1 MiB file limit and the build fits the mapping's 16 MiB, but
		// together they are over the 4 MiB the render check takes (#482).
		res := make([]string, 0, 6)
		res = append(res, "deployment.yaml")
		for i := range 5 {
			name := fmt.Sprintf("fill%d.yaml", i)
			res = append(res, name)
			cm := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fill%d\n  namespace: %s\n", i, fx.TargetNamespace)
			_, err := git.Push(mainBranch, buildPath+"/"+name, []byte("# "+strings.Repeat("x", 900<<10)+"\n"+cm))
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := git.Push(mainBranch, buildKustomize, []byte(
			"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - "+strings.Join(res, "\n  - ")+"\n"))
		Expect(err).NotTo(HaveOccurred())
		// BeforeEach decided the request on the small build; clear its status so it is decided again on this one.
		got := fetch()
		got.Status = remediationv1alpha1.RemediationRequestStatus{}
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		got = drive(remediationv1alpha1.PhaseNeedsIntervention)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(string(semdiff.ReasonTooLarge)))
		Expect(cond.Message).To(ContainSubstring(fmt.Sprintf("at most %d bytes", semdiff.MaxBuildBytes)))
		Expect(got.Status.PlanDigest).To(BeEmpty(), "no plan was made")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)).To(BeNil())
		_, err = git.BranchHead(ctx, freshness.BranchName(got))
		Expect(providers.IsNotFound(err)).To(BeTrue(), "no branch was cut")
	})

	It("retries, without stopping, when the target changes while the build over the render budget is read", func() {
		res := append(make([]string, 0, 6), "deployment.yaml")
		for i := range 5 {
			name := fmt.Sprintf("fill%d.yaml", i)
			res = append(res, name)
			cm := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fill%d\n  namespace: %s\n", i, fx.TargetNamespace)
			_, err := git.Push(mainBranch, buildPath+"/"+name, []byte("# "+strings.Repeat("x", 900<<10)+"\n"+cm))
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := git.Push(mainBranch, buildKustomize, []byte(
			"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - "+strings.Join(res, "\n  - ")+"\n"))
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		got.Status = remediationv1alpha1.RemediationRequestStatus{}
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		// Flux applies a new revision of the target while the build is read: its resourceVersion moves.
		faulty = newFaultyGit(git)
		faulty.beforeOp(opReadFile, func() {
			defer GinkgoRecover()
			live := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), live)).To(Succeed())
			live.Annotations = map[string]string{"flux.example/revision": "2"}
			Expect(k8sClient.Update(ctx, live)).To(Succeed())
		})
		var errs []error
		for range 5 {
			if faulty.count(opReadFile) > 0 {
				break
			}
			errs = append(errs, reconcileOnce())
		}
		Expect(faulty.count(opReadFile)).To(BeNumerically(">", 0), "the build was read")
		Expect(errs[len(errs)-1]).To(MatchError(ContainSubstring("changed during the policy check")),
			"the reconcile that read the build retries")
		Expect(fetch().Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseNeedsIntervention),
			"a build read for an object that changed does not end the request")
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

	// stoppedDiverged asserts that the request stopped in NeedsIntervention with BranchDiverged, without a
	// recorded pull request, and that the branch is still at head.
	stoppedDiverged := func(head string) *remediationv1alpha1.RemediationRequest {
		GinkgoHelper()
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonBranchDiverged))
		Expect(got.Status.PullRequest).To(BeNil(), "no pull request is recorded")
		now, err := git.BranchHead(ctx, freshness.BranchName(got))
		Expect(err).NotTo(HaveOccurred())
		Expect(now).To(Equal(head), "nothing is pushed to the branch")
		return got
	}

	// closedWith asserts that the request stopped in NeedsIntervention with reason after Shadewright
	// closed the branch's one pull request, which it recorded, and that the branch is still at head.
	closedWith := func(head, reason string) {
		GinkgoHelper()
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		crs := openRequests()
		Expect(crs).To(HaveLen(1))
		Expect(crs[0].State).To(Equal(providers.ChangeRequestClosed), "the pull request is closed")
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded")
		Expect(got.Status.PullRequest.Number).To(Equal(crs[0].Number))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(reason))
		Expect(cond.Message).To(HaveSuffix("the pull request was closed"))
		opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(opened).NotTo(BeNil())
		Expect(opened.Reason).To(Equal(ReasonPullRequestClosed))
		Expect(got.Status.HoldsTarget).NotTo(Equal(new(true)), "a closed pull request releases the target")
		now, err := git.BranchHead(ctx, freshness.BranchName(got))
		Expect(err).NotTo(HaveOccurred())
		Expect(now).To(Equal(head), "nothing is pushed to the branch")
	}

	It("does not commit on a branch that carries someone else's commit (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCreateBranch) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		// Someone pushes to the branch before Shadewright commits; the target file still holds the previous
		// value, so the file alone does not tell.
		foreign, err := git.Push(branch, "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got := stoppedDiverged(foreign)
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Message).
			To(HaveSuffix("nothing was committed"))
		Expect(faulty.count(opCommit)).To(BeZero())
		Expect(faulty.count(opOpen)).To(BeZero())
		Expect(openRequests()).To(BeEmpty())
	})

	It("does not open a pull request for a branch that carries someone else's commit after its own (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		// Someone pushes on top of Shadewright's commit before the pull request is opened; the target file
		// still holds the planned value.
		foreign, err := git.Push(branch, "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		stoppedDiverged(foreign)
		Expect(faulty.count(opOpen)).To(BeZero())
		Expect(openRequests()).To(BeEmpty())
	})

	It("does not open a pull request for a branch whose planned value someone else committed (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCreateBranch) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		// Someone else commits another file and then the planned value: the branch looks done.
		_, err := git.Push(branch, "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		foreign, err := git.Push(branch, sourcePath, manifest("384Mi"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		stoppedDiverged(foreign)
		Expect(faulty.count(opCommit)).To(BeZero())
		Expect(faulty.count(opOpen)).To(BeZero())
		Expect(openRequests()).To(BeEmpty())
	})

	It("does not record a pull request whose branch got someone else's commit before the status write (#524)", func() {
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
		foreign, err := git.Push(freshness.BranchName(fetch()), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		failStatus = false
		Expect(reconcileOnce()).To(Succeed())
		closedWith(foreign, ReasonBranchDiverged)
	})

	It("does not open a pull request for a branch whose one commit also edits another field of the file (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCreateBranch) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		// One commit on the cut sets the planned limit and, in the same file, a memory request: the file holds
		// the planned value and the commit changes only the target file, yet it is not the planned change.
		edited := withMemoryRequest(manifest("384Mi"), "320Mi")
		foreign, err := git.Push(branch, sourcePath, edited)
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		stoppedDiverged(foreign)
		Expect(faulty.count(opCommit)).To(BeZero())
		Expect(faulty.count(opOpen)).To(BeZero())
		Expect(openRequests()).To(BeEmpty())
	})

	It("closes and records a pull request whose branch moved between the check and the open (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		var foreign string
		faulty.beforeOp(opOpen, func() {
			defer GinkgoRecover()
			var err error
			foreign, err = git.Push(branch, "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
		})
		Expect(reconcileOnce()).To(Succeed())
		closedWith(foreign, ReasonBranchDiverged)
		// A restart opens nothing again.
		Expect(reconcileOnce()).To(Succeed())
		Expect(faulty.count(opOpen)).To(Equal(1))
		Expect(openRequests()).To(HaveLen(1))
	})

	It("closes and records a pull request found after a lost open whose branch now holds another value (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		// The open takes effect but its response is lost; before the restart someone pushes another value.
		faulty.crashAfter(opOpen)
		Expect(reconcileOnce()).NotTo(Succeed())
		Expect(openRequests()).To(HaveLen(1))
		foreign, err := git.Push(branch, sourcePath, manifest("1Gi"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		closedWith(foreign, ReasonHeadMoved)
	})

	It("records a refused pull request it could not close as open and keeps the target locked (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		Expect(fetch().Status.HoldsTarget).To(Equal(new(true)))
		branch := freshness.BranchName(fetch())
		faulty.beforeOp(opOpen, func() {
			defer GinkgoRecover()
			_, err := git.Push(branch, "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
		})
		// A close that fails for a reason worth a retry, or one the kill switch holds back, decides nothing.
		faulty.failOn(opClose, &providers.RetryableError{Err: errors.New("server error")})
		err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(retryable(err)).To(BeTrue())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "a failed close is retried")
		faulty.failOn(opClose, fmt.Errorf("close: %w", freshness.ErrPaused))
		_ = reconcileOnce()
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "a paused close waits")
		Expect(meta.IsStatusConditionTrue(fetch().Status.Conditions, remediationv1alpha1.ConditionWritesPaused)).To(BeTrue())
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		// Only a final refusal stops the request.
		faulty.failOn(opClose, fmt.Errorf("status 403: %w", providers.ErrForbidden))
		Expect(reconcileOnce()).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		crs := openRequests()
		Expect(crs).To(HaveLen(1))
		Expect(crs[0].State).To(Equal(providers.ChangeRequestOpen))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "an open pull request is never left unrecorded")
		Expect(got.Status.PullRequest.Number).To(Equal(crs[0].Number))
		opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(opened.Reason).To(Equal(ReasonPullRequestOpened), "not recorded as closed without a confirmed close")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved).Message).
			To(HaveSuffix("close it by hand"))
		Expect(got.Status.HoldsTarget).To(Equal(new(true)), "the target stays locked while it may be open")
	})

	It("retries, without recording a merge, when closing the refused pull request is a conflict but it is open (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		var foreign string
		faulty.beforeOp(opOpen, func() {
			defer GinkgoRecover()
			var err error
			foreign, err = git.Push(branch, "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
		})
		// GitHub answers 409 for reasons other than a merge; the adapter maps it to ErrConflict.
		faulty.failOn(opClose, fmt.Errorf("status 409: %w", providers.ErrConflict))
		Expect(reconcileOnce()).NotTo(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "a conflict is not a merge")
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		faulty.failOn("", nil)
		Expect(reconcileOnce()).To(Succeed())
		closedWith(foreign, ReasonBranchDiverged)
	})

	// refusedAfterPerson drives to the commit, then lets someone push to the branch right before the open and a
	// person act on the pull request right after it, before Shadewright's close. It returns the stopped request.
	refusedAfterPerson := func(act func(n int64) error) *remediationv1alpha1.RemediationRequest {
		GinkgoHelper()
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		faulty.beforeOp(opOpen, func() {
			defer GinkgoRecover()
			_, err := git.Push(branch, "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
		})
		git.SetAfter(func(op string) error {
			if op == opOpen {
				return act(1)
			}
			return nil
		})
		Expect(reconcileOnce()).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded")
		Expect(got.Status.PullRequest.Number).To(Equal(int64(1)))
		Expect(got.Status.HoldsTarget).NotTo(Equal(new(true)), "a pull request that is not open releases the target")
		return got
	}

	It("ends with MergedWithForeignChange when a person merges the refused pull request before the close (#524)", func() {
		got := refusedAfterPerson(func(n int64) error {
			_, err := git.Merge(n)
			return err
		})
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(cond.Reason).To(Equal(ReasonMergedWithForeignChange))
		Expect(cond.Message).To(HavePrefix("Pull request #1 was merged"))
		Expect(cond.Message).To(ContainSubstring(ReasonBranchDiverged))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonChangeRequestMerged))
		Expect(faulty.count(opClose)).To(BeZero(), "a merged pull request is not closed")
	})

	It("ends as closed by a person when a person closes the refused pull request before the close (#524)", func() {
		got := refusedAfterPerson(func(n int64) error { return git.CloseChangeRequest(ctx, n) })
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			To(Equal(ReasonChangeRequestClosed))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonChangeRequestClosed))
		Expect(faulty.count(opClose)).To(BeZero(), "Shadewright does not claim the close")
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	// mergedAfterLostOpen opens the pull request with its response lost, lets push run on the branch, and has a
	// person merge it before the restart. It returns the request after the restart.
	// lostOpen drives the request up to the open of its pull request and loses the answer of the open: the pull
	// request is open with this request's marker and unrecorded.
	lostOpen := func() {
		GinkgoHelper()
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		faulty.crashAfter(opOpen)
		Expect(reconcileOnce()).NotTo(Succeed())
		Expect(openRequests()).To(HaveLen(1))
	}

	mergedAfterLostOpen := func(push func(branch string), merge ...func(int64) (string, error)) *remediationv1alpha1.RemediationRequest {
		GinkgoHelper()
		lostOpen()
		push(freshness.BranchName(fetch()))
		mergeWith := git.Merge
		if len(merge) > 0 {
			mergeWith = merge[0]
		}
		_, err := mergeWith(openRequests()[0].Number)
		Expect(err).NotTo(HaveOccurred())
		_ = reconcileOnce()
		return fetch()
	}

	It("records a merged pull request found after a lost open as Merged when it held the planned change (#524)", func() {
		got := mergedAfterLostOpen(func(string) {})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
	})

	It("ends Merged on an older merged pull request of its own that a newer closed one of the branch hides (#688)", func() {
		lostOpen()
		merged := openRequests()[0]
		_, err := git.Merge(merged.Number)
		Expect(err).NotTo(HaveOccurred())
		// The merge is reverted on the target branch, so the plan is fresh again and the step looks for its pull
		// request rather than the stale plan's cleanup.
		_, err = git.Push(mainBranch, sourcePath, manifest("256Mi"))
		Expect(err).NotTo(HaveOccurred())
		newer, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: merged.HeadBranch,
			BaseBranch: mainBranch, Title: "opened after the merge, then closed", Body: merged.Body})
		Expect(err).NotTo(HaveOccurred())
		Expect(git.CloseChangeRequest(ctx, newer.Number)).To(Succeed())
		_ = reconcileOnce()
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged), "the merge is not hidden by the newer closed one")
		Expect(got.Status.PullRequest).NotTo(BeNil())
		Expect(got.Status.PullRequest.Number).To(Equal(merged.Number))
	})

	It("ends Merged, not Expired and not in a loop, when its unrecorded pull request was merged before the deadline cleanup (#688)", func() {
		lostOpen()
		_, err := git.Merge(openRequests()[0].Number)
		Expect(err).NotTo(HaveOccurred())
		clock = fetch().Status.Deadline.Add(time.Minute)
		_, err = reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the merged pull request is recorded")
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()), "a merge releases the lock")
	})

	It("records the pull request and its merge commit when it was merged after a lost open (#561)", func() {
		var sha string
		got := mergedAfterLostOpen(func(string) {}, func(n int64) (string, error) {
			var err error
			sha, err = git.Merge(n)
			return sha, err
		})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded")
		Expect(got.Status.PullRequest.Number).To(Equal(openRequests()[0].Number))
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha), "delivery is checked against this commit")
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(ReasonChangeRequestMerged))
		approved := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(approved).NotTo(BeNil())
		Expect(approved.Status).To(Equal(metav1.ConditionTrue))
		Expect(approved.Reason).To(Equal(ReasonChangeRequestMerged))
	})

	It("does not record a merge commit when the merged pull request found after a lost open held more (#561)", func() {
		got := mergedAfterLostOpen(func(branch string) {
			_, err := git.Push(branch, "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
		})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(ReasonMergedWithForeignChange))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded")
		Expect(got.Status.PullRequest.MergeCommitSHA).To(BeEmpty(), "a foreign change is not checked for delivery")
	})

	It("records a clean pull request merged with a merge commit after a lost open as Merged (#524)", func() {
		// The merge commit takes the head into the target branch, so the head is an ancestor of the target now; it
		// is judged against its own parent, not the current target.
		got := mergedAfterLostOpen(func(string) {}, git.MergeWithCommit)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
	})

	// ADR-0030 section 1 (#678): the merge receipt, written in the status write that sets Merged.
	expectReceipt := func(got *remediationv1alpha1.RemediationRequest, number int64, head, merge string) {
		GinkgoHelper()
		m := got.Status.MergeReceipt
		Expect(m).NotTo(BeNil(), "no merge receipt")
		Expect(m.Repository).To(Equal(*got.Status.PlanRepository))
		Expect(m.TargetBranch).To(Equal(got.Status.Source.TargetBranch))
		Expect(m.PullRequest).To(Equal(number))
		Expect(m.HeadSHA).To(Equal(head))
		Expect(m.MergeCommitSHA).To(Equal(merge))
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(merge))
		Expect(got.Status.PullRequest.MergedAt).NotTo(BeNil())
		Expect(m.MergedAt.Equal(got.Status.PullRequest.MergedAt)).To(BeTrue(), "the time of the write that set Merged")
	}
	// mergedWrite returns the first status write that set Merged.
	mergedWrite := func(rec *statusRecorder) *remediationv1alpha1.RemediationRequestStatus {
		GinkgoHelper()
		for _, st := range rec.written() {
			if st.Phase == remediationv1alpha1.PhaseMerged {
				return &st
			}
		}
		Fail("no status write set Merged")
		return nil
	}

	It("writes the merge receipt of the recorded pull request in the status write that sets Merged (#678)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n, head := got.Status.PullRequest.Number, got.Status.PullRequest.HeadSHA
		sha, err := git.Merge(n)
		Expect(err).NotTo(HaveOccurred())
		rec := &statusRecorder{Client: k8sClient}
		r := reconciler()
		r.Client = rec
		_, _ = r.Reconcile(ctx, key())
		Expect(mergedWrite(rec).MergeReceipt).NotTo(BeNil(), "the receipt is in the write that sets Merged")
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		expectReceipt(got, n, head, sha)
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified)).To(BeNil(),
			"a merge with a receipt leaves Verified to the steps after the merge")
		receipt := got.Status.MergeReceipt.DeepCopy()
		clock = clock.Add(time.Hour)
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.MergeReceipt).To(Equal(receipt), "a later reconcile keeps the receipt")
	})

	It("repeats a status write lost after a merge with the same receipt values (#678)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n, head := got.Status.PullRequest.Number, got.Status.PullRequest.HeadSHA
		sha, err := git.Merge(n)
		Expect(err).NotTo(HaveOccurred())
		lost := 0
		r := reconciler()
		r.Client = &statusRecorder{Client: k8sClient, fail: func(rr *remediationv1alpha1.RemediationRequest) bool {
			if rr.Status.Phase == remediationv1alpha1.PhaseMerged {
				lost++
				return true
			}
			return false
		}}
		_, err = r.Reconcile(ctx, key())
		Expect(err).To(HaveOccurred(), "the lost write is returned for a retry")
		Expect(lost).To(Equal(1))
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "the write that set Merged was lost")
		Expect(got.Status.MergeReceipt).To(BeNil())
		clock = clock.Add(time.Minute)
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		expectReceipt(got, n, head, sha)
	})

	It("records the merge of the recorded pull request when its branch carries a newer pull request (#678)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n, head := got.Status.PullRequest.Number, got.Status.PullRequest.HeadSHA
		sha, err := git.Merge(n)
		Expect(err).NotTo(HaveOccurred())
		// A newer pull request on the same branch, as a supersede chain can open after the first merged.
		_, err = git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 2\n"))
		Expect(err).NotTo(HaveOccurred())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: "next", Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		Expect(next.Number).NotTo(Equal(n))
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged), "the newer pull request does not hide the merge")
		expectReceipt(got, n, head, sha)
	})

	It("stops with ChangeRequestGone when the recorded number is a pull request from another branch (#678)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		head, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		Expect(git.CreateBranch(ctx, "shadewright/someone-else", head)).To(Succeed())
		other, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: "shadewright/someone-else",
			BaseBranch: mainBranch, Title: "newer"})
		Expect(err).NotTo(HaveOccurred())
		got.Status.PullRequest.Number = other.Number
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		approved := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(approved.Reason).To(Equal(ReasonChangeRequestGone))
		Expect(approved.Message).To(ContainSubstring("from another branch"), "the cause is kept")
		Expect(got.Status.MergeReceipt).To(BeNil())
	})

	It("retries the watch when the Git host answers the look-up by number with another pull request, and never stops on it (#688)", func() {
		drive(remediationv1alpha1.PhaseAwaitingApproval)
		git.SetFail(func(op string) error {
			if op == opGet {
				return fmt.Errorf("another pull request: %w", providers.ErrUntrustedAnswer)
			}
			return nil
		})
		res, err := reconcileResult()
		// A look that fails goes on to the full recheck (prUnchecked), whose freshness check passes: the pull
		// request is read again at the next poll, and nothing is handed to the error backoff.
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval), "read again at the next poll")
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval),
			"an answer about another pull request is a failed read: nothing is ended on it")
		git.SetFail(nil)
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
	})

	It("retries a look-up by number that fails, deciding nothing (#678)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Merge(got.Status.PullRequest.Number)
		Expect(err).NotTo(HaveOccurred())
		git.SetFail(func(op string) error {
			if op == opGet {
				return &providers.RetryableError{Err: errors.New("server error")}
			}
			return nil
		})
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "a failed read is not an answer")
		Expect(got.Status.MergeReceipt).To(BeNil())
		git.SetFail(nil)
		_ = reconcileOnce()
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
	})

	It("writes the merge receipt on the adopt path, for a merged pull request found after a lost open (#678)", func() {
		var sha string
		got := mergedAfterLostOpen(func(string) {}, func(n int64) (string, error) {
			var err error
			sha, err = git.Merge(n)
			return sha, err
		})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		cr := openRequests()[0]
		expectReceipt(got, cr.Number, cr.HeadSHA, sha)
	})

	It("waits for the merge commit on the adopt path, then checks the merge with it (#678)", func() {
		var sha string
		var n int64
		got := mergedAfterLostOpen(func(string) {}, func(num int64) (string, error) {
			var err error
			n = num
			if sha, err = git.Merge(num); err != nil {
				return "", err
			}
			return sha, git.SetMergeCommit(num, "")
		})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "waits for the commit")
		Expect(got.Status.PullRequest).To(BeNil(), "nothing recorded while it waits")
		Expect(got.Status.MergeCommitAwaitedSince).NotTo(BeNil())
		clock = clock.Add(time.Minute)
		Expect(git.SetMergeCommit(n, sha)).To(Succeed())
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		expectReceipt(got, n, openRequests()[0].HeadSHA, sha)
	})

	It("ends a merge found on the adopt path without a merge commit after the wait as unchecked (#678)", func() {
		got := mergedAfterLostOpen(func(string) {}, func(num int64) (string, error) {
			sha, err := git.Merge(num)
			if err != nil {
				return "", err
			}
			return sha, git.SetMergeCommit(num, "")
		})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		clock = got.Status.MergeCommitAwaitedSince.Add(mergeCommitWait)
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		approved := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(approved.Reason).To(Equal(ReasonMergedWithForeignChange))
		Expect(approved.Message).To(ContainSubstring("no full merge commit ID"))
		Expect(got.Status.MergeReceipt).To(BeNil())
	})

	It("writes no merge receipt for a merge with a foreign change (#678)", func() {
		got := mergedAfterLostOpen(func(branch string) {
			_, err := git.Push(branch, "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
		})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(got.Status.MergeReceipt).To(BeNil())
	})

	DescribeTable("ends a merge without a full merge commit ID Inconclusive with NoMergeReceipt (#678)",
		func(commit string) {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			n := got.Status.PullRequest.Number
			_, err := git.Merge(n)
			Expect(err).NotTo(HaveOccurred())
			Expect(git.SetMergeCommit(n, commit)).To(Succeed())
			// The executor waits for the commit, from the time it first saw the merge without one, on its clock.
			start := clock
			_ = reconcileOnce()
			got = fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "waits for the commit")
			Expect(got.Status.MergeCommitAwaitedSince).NotTo(BeNil())
			Expect(got.Status.MergeCommitAwaitedSince.Time.Equal(start.Truncate(time.Second))).To(BeTrue())
			Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "the lock is kept while it waits")
			clock = clock.Add(mergeCommitWait - time.Second)
			_ = reconcileOnce()
			got = fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "still within the wait")
			Expect(got.Status.MergeCommitAwaitedSince.Time.Equal(start.Truncate(time.Second))).To(BeTrue(), "never moved")

			clock = start.Truncate(time.Second).Add(mergeCommitWait)
			_ = reconcileOnce()
			got = fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
			Expect(got.Status.MergeReceipt).To(BeNil())
			Expect(got.Status.PullRequest.MergeCommitSHA).To(BeEmpty())
			verified := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified)
			Expect(verified).NotTo(BeNil(), "set in the write that records the merge")
			Expect(verified.Status).To(Equal(metav1.ConditionUnknown))
			Expect(verified.Reason).To(Equal(ReasonNoMergeReceipt))

			clock = clock.Add(time.Minute)
			_ = reconcileOnce()
			got = fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseInconclusive))
			verified = meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified)
			Expect(verified.Status).To(Equal(metav1.ConditionUnknown))
			Expect(verified.Reason).To(Equal(ReasonNoMergeReceipt))
			approved := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
			Expect(approved.Status).To(Equal(metav1.ConditionTrue), "the merge stays recorded")
			Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
			Expect(got.Status.BreakerCounts).To(HaveValue(BeFalse()), "neither a success nor a failure")
			Expect(got.Status.LastPhaseTransitionTime.Time.Equal(clock)).To(BeTrue())

			_ = reconcileOnce()
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseInconclusive), "it is final")
		},
		Entry("none", ""),
		Entry("abbreviated", "abcdef1"),
		Entry("upper case", strings.Repeat("AB", 20)),
		Entry("SHA-256", strings.Repeat("ab", 32)),
	)

	It("records the merge with its receipt when the Git host reports the merge commit during the wait (#678)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		sha, err := git.Merge(n)
		Expect(err).NotTo(HaveOccurred())
		Expect(git.SetMergeCommit(n, "")).To(Succeed())
		_ = reconcileOnce()
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		clock = clock.Add(time.Minute)
		Expect(git.SetMergeCommit(n, sha)).To(Succeed())
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		expectReceipt(got, n, got.Status.PullRequest.HeadSHA, sha)
	})

	It("marks a request merged before merge receipts existed once, and leaves its breaker reading as it was (#678)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		sha, err := git.Merge(got.Status.PullRequest.Number)
		Expect(err).NotTo(HaveOccurred())
		// What an executor before receipts wrote for the merge: Merged with the merge commit, nothing else.
		merged := metav1.NewMicroTime(clock)
		got.Status.Phase = remediationv1alpha1.PhaseMerged
		got.Status.LastPhaseTransitionTime = &merged
		got.Status.PullRequest.MergeCommitSHA = sha
		got.Status.PullRequest.MergedAt = &merged
		got.Status.HoldsTarget = new(false)
		got.Status.BreakerCounts = new(true)
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		before := fetch()
		Expect(breakerOutcome(before)).To(Equal(outcomeSuccess))

		clock = clock.Add(time.Minute)
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.MergeReceipt).To(BeNil())
		verified := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified)
		Expect(verified).NotTo(BeNil())
		Expect(verified.Status).To(Equal(metav1.ConditionUnknown))
		Expect(verified.Reason).To(Equal(ReasonMergedBeforeVerification))
		Expect(got.Status.LastPhaseTransitionTime).To(Equal(before.Status.LastPhaseTransitionTime))
		Expect(got.Status.BreakerCounts).To(Equal(before.Status.BreakerCounts))
		Expect(breakerOutcome(got)).To(Equal(outcomeSuccess), "the breaker reads the same history")

		clock = clock.Add(time.Hour)
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		again := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified)
		Expect(*again).To(Equal(*verified), "written once")
	})

	It("ends a merged request without a receipt and with an unknown Verified reason Inconclusive (#678)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Merge(got.Status.PullRequest.Number)
		Expect(err).NotTo(HaveOccurred())
		merged := metav1.NewMicroTime(clock)
		got.Status.Phase = remediationv1alpha1.PhaseMerged
		got.Status.LastPhaseTransitionTime = &merged
		got.Status.PullRequest.MergedAt = &merged
		got.Status.HoldsTarget = new(false)
		got.Status.BreakerCounts = new(true)
		setVerifiedUnknown(got, "SomethingElse", "not written by this executor")
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		clock = clock.Add(time.Minute)
		_ = reconcileOnce()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseInconclusive), "never passes as merged")
		verified := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified)
		Expect(verified.Reason).To(Equal(ReasonNoMergeReceipt))
		Expect(verified.Message).To(ContainSubstring(`"SomethingElse"`))
	})

	It("retries the check of a merged pull request found after a lost open without closing it again (#524)", func() {
		got := mergedAfterLostOpen(func(string) {
			faulty.failOn(opCheckMerged, &providers.RetryableError{Err: errors.New("server error")})
		})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "a failed read decides nothing")
		err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(retryable(err)).To(BeTrue(), "the read failure is the cause handed back")
		Expect(faulty.count(opClose)).To(BeZero(), "a merged pull request is not closed again")
		faulty.failOn("", nil)
		_ = reconcileOnce()
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
	})

	It("ends with MergedWithForeignChange when the merged pull request found after a lost open held more (#524)", func() {
		got := mergedAfterLostOpen(func(branch string) {
			_, err := git.Push(branch, "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
		})
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(ReasonMergedWithForeignChange))
		Expect(cond.Message).To(HavePrefix("Pull request #1 was merged"))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded")
		Expect(got.Status.HoldsTarget).NotTo(Equal(new(true)), "a merged pull request releases the target")
	})

	It("closes the pull request it opened by its number when its marker is gone before the close (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		branch := freshness.BranchName(fetch())
		var foreign string
		faulty.beforeOp(opOpen, func() {
			defer GinkgoRecover()
			var err error
			foreign, err = git.Push(branch, "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
		})
		// Right after the open answered (with the marker), someone removes the marker from the description: a
		// look-up by marker no longer finds it, so the close must go by the number the open returned.
		git.SetAfter(func(op string) error {
			if op == opOpen {
				Expect(git.SetBody(1, "edited")).To(Succeed())
			}
			return nil
		})
		Expect(reconcileOnce()).To(Succeed())
		closedWith(foreign, ReasonBranchDiverged)
	})

	It("retries, without stopping, when what the branch holds cannot be read (#524)", func() {
		faulty = newFaultyGit(git)
		for faulty.count(opCommit) == 0 {
			Expect(reconcileOnce()).To(Succeed())
		}
		faulty.failOn(opCheckBranch, &providers.RetryableError{Err: errors.New("server error")})
		err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(retryable(err)).To(BeTrue())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "a failed read decides nothing")
		Expect(faulty.count(opOpen)).To(BeZero())

		faulty.failOn("", nil)
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(openRequests()).To(HaveLen(1))
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

	// conflictSeen returns the recorded first report of a merge conflict, or nil.
	conflictSeen := func(rr *remediationv1alpha1.RemediationRequest) *remediationv1alpha1.ConflictSeen {
		return rr.Status.PullRequest.ConflictSeen
	}

	It("stops with ChangeRequestConflict, not as a rejection, when the merge conflict lasts past the wait (#680)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.SetConflicted(n, true)).To(Succeed())

		// The first report is recorded, on the executor's clock, and decides nothing. Each reconcile runs on a new
		// reconciler, as after a restart, so the wait lives in status only.
		start := clock.Truncate(time.Second)
		res, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		seen := conflictSeen(got)
		Expect(seen).NotTo(BeNil())
		Expect(seen.At.Time.Equal(start)).To(BeTrue())
		Expect(seen.HeadSHA).To(Equal(got.Status.PullRequest.HeadSHA))
		mainHead, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		Expect(seen.TargetHead).To(Equal(mainHead))

		clock = start.Add(confirmWait - time.Second)
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "still inside the wait")
		Expect(conflictSeen(got).At.Time.Equal(start)).To(BeTrue(), "the first report is not moved")

		clock = start.Add(confirmWait)
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		approved := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(approved.Reason).To(Equal(ReasonChangeRequestConflict))
		opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(opened.Reason).To(Equal(ReasonPullRequestOpened), "the pull request is left open")
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen), "Shadewright does not close it")
		Expect(got.Status.HoldsTarget).To(Equal(new(true)), "an open pull request keeps the lock on the target")
		Expect(breakerOutcome(got)).To(Equal(outcomeFailure), "the circuit breaker counts it as NeedsIntervention")

		// A person who closes it afterwards does not turn it into a rejection: the request has ended.
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			To(Equal(ReasonChangeRequestConflict))
	})

	It("does not end the request on a stale merge conflict that the next read no longer reports (#680)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.SetConflicted(n, true)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Expect(conflictSeen(fetch())).NotTo(BeNil())

		// The host computes the answer again: no conflict. The report is dropped.
		Expect(git.SetConflicted(n, false)).To(Succeed())
		clock = clock.Add(confirmWait + time.Minute)
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(conflictSeen(got)).To(BeNil())

		// A conflict reported again starts a new wait: the earlier report does not count.
		Expect(git.SetConflicted(n, true)).To(Succeed())
		clock = clock.Add(time.Second)
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(conflictSeen(got).At.Time.Equal(clock.Truncate(time.Second))).To(BeTrue())
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			NotTo(Equal(ReasonChangeRequestConflict))
	})

	It("keeps the wait for a merge conflict when the target branch head moves (#680)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(git.SetConflicted(got.Status.PullRequest.Number, true)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		first := conflictSeen(fetch())
		Expect(first).NotTo(BeNil())

		// A commit that leaves the target file alone keeps the plan fresh. It does not start the wait again: a
		// busy target branch would otherwise postpone the end for as long as it keeps moving.
		_, err := git.Push(mainBranch, "apps/other/notes.yaml", []byte("a: b\n"))
		Expect(err).NotTo(HaveOccurred())
		clock = first.At.Add(confirmWait)
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
	})

	It("starts the wait for a merge conflict again when the clock went back (#680)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(git.SetConflicted(got.Status.PullRequest.Number, true)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Expect(conflictSeen(fetch())).NotTo(BeNil())

		clock = clock.Add(-time.Hour)
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "a clock set back ends no wait")
		Expect(conflictSeen(got).At.Time.Equal(clock)).To(BeTrue())
	})

	It("supersedes a stale plan whose pull request has a merge conflict, as without one (#680)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
		Expect(err).NotTo(HaveOccurred())
		Expect(git.SetConflicted(got.Status.PullRequest.Number, true)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("decides nothing on a merge conflict while the freshness check cannot finish (#680)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(git.SetConflicted(got.Status.PullRequest.Number, true)).To(Succeed())
		git.SetFail(func(op string) error {
			if op == "ReadFile" || op == "BranchHead" {
				return errors.New("git host down")
			}
			return nil
		})
		_, _ = reconcileResult()
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(conflictSeen(fetch())).To(BeNil(), "a failed check records no conflict")
		git.SetFail(nil)
		Expect(reconcileOnce()).To(Succeed())
		Expect(conflictSeen(fetch())).NotTo(BeNil())
		clock = clock.Add(confirmWait)
		Expect(reconcileOnce()).To(Succeed())
		Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			To(Equal(ReasonChangeRequestConflict))
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

	// second creates another request on the same target as request, and reconciles it with a new reconciler
	// each time, as after a restart. It is deleted after the spec.
	// another is a request named request.Name plus suffix for the same target and field as request.
	another := func(suffix string) (*remediationv1alpha1.RemediationRequest, func() *remediationv1alpha1.RemediationRequest) {
		rr := fx.MemoryLimitRequest(requestNamespace, request.Name+suffix)
		rr.Spec.PolicyRef = request.Spec.PolicyRef
		rr.Spec.TargetRef = request.Spec.TargetRef
		Expect(k8sClient.Create(ctx, rr)).To(Succeed())
		DeferCleanup(func() {
			got := &remediationv1alpha1.RemediationRequest{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rr), got); err == nil {
				base := got.DeepCopy()
				controllerutil.RemoveFinalizer(got, FinalizerName)
				Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, rr))).To(Succeed())
		})
		return rr, func() *remediationv1alpha1.RemediationRequest {
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
			Expect(err).NotTo(HaveOccurred())
			got := &remediationv1alpha1.RemediationRequest{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rr), got)).To(Succeed())
			return got
		}
	}
	second := func() (*remediationv1alpha1.RemediationRequest, func() *remediationv1alpha1.RemediationRequest) {
		return another("-second")
	}
	// expectClosedElsewhere checks that a request that waited behind the closed pull request ended (#643), and
	// that a request for the same field created after the lock was released goes through: the target is free.
	expectClosedElsewhere := func(waited *remediationv1alpha1.RemediationRequest) {
		GinkgoHelper()
		Expect(waited.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(meta.FindStatusCondition(waited.Status.Conditions, remediationv1alpha1.ConditionPlanFresh).Reason).
			To(Equal(remediationv1alpha1.ReasonAnotherRequestClosed))
		_, reconcileThird := another("-third")
		Expect(reconcileThird().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
	}
	expectLocked := func(got *remediationv1alpha1.RemediationRequest) {
		GinkgoHelper()
		Expect(got.Status.Phase).To(BeEmpty())
		queued := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued)
		Expect(queued).NotTo(BeNil())
		Expect(queued.Reason).To(Equal(ReasonTargetLocked))
	}

	It("keeps the target locked while a stopped request's pull request is open, across restarts, until it is deleted", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))

		_, reconcileSecond := second()
		for range 3 {
			expectLocked(reconcileSecond())
		}
		Expect(git.ChangeRequests()).To(HaveLen(1), "no second pull request while the first may be open")

		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		expectLocked(reconcileSecond()) // the finalizer has not closed the pull request yet
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
		next := reconcileSecond()
		Expect(next.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		Expect(meta.FindStatusCondition(next.Status.Conditions, remediationv1alpha1.ConditionQueued)).To(BeNil())
	})

	// stopHeadMoved drives the request to AwaitingApproval and stops it with HeadMoved, its pull request open.
	stopHeadMoved := func() *remediationv1alpha1.RemediationRequest {
		GinkgoHelper()
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
		return got
	}

	It("looks the pull request of a stopped holder up slowly and frees the target once a person closes it", func() {
		got := stopHeadMoved()
		_, reconcileSecond := second()
		expectLocked(reconcileSecond())
		res, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(heldRecheckInterval), "an open pull request is looked at again slowly")
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()))
		Expect(git.CloseChangeRequest(ctx, got.Status.PullRequest.Number)).To(Succeed())
		res, err = reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}), "a released lock is not looked at again")
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonChangeRequestClosed))
		expectClosedElsewhere(reconcileSecond())
	})

	It("keeps the target of a stopped holder whose recorded pull request is closed while another one is open on its branch (#688)", func() {
		got := stopHeadMoved()
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		// A newer pull request on the same branch, with this request's own marker: it may carry its commit.
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: "opened after the close", Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		Expect(next.Number).NotTo(Equal(n))
		res, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(heldRecheckInterval))
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "another pull request is open on the branch")
		Expect(got.Status.PullRequest.Number).To(Equal(n))
		held := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)
		Expect(held).NotTo(BeNil(), "the status says why the lock is kept")
		Expect(held.Reason).To(Equal(ReasonOpenOnBranch))
		Expect(held.Message).To(ContainSubstring(fmt.Sprintf("#%d", next.Number)))
		before := got.ResourceVersion
		_, err = reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().ResourceVersion).To(Equal(before), "the reason is written once, not on every look")

		// Once the branch has no open pull request, the closed recorded one releases the lock.
		Expect(git.CloseChangeRequest(ctx, next.Number)).To(Succeed())
		res, err = reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}), "a released lock is not looked at again")
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
		Expect(got.Status.PullRequest.Number).To(Equal(n))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonChangeRequestClosed))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)).To(BeNil())
	})

	It("keeps the target of a stopped holder when the read by its branch fails (#688)", func() {
		got := stopHeadMoved()
		Expect(git.CloseChangeRequest(ctx, got.Status.PullRequest.Number)).To(Succeed())
		git.SetFail(func(op string) error {
			if op == opList {
				return &providers.RetryableError{Err: errors.New("connection reset")}
			}
			return nil
		})
		res, err := reconcileResult()
		git.SetFail(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(heldRecheckInterval))
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()), "a failed read keeps the lock")
	})

	// reopenBehindNewer leaves the branch of got with its recorded pull request closed, #2 open with body, and #3
	// closed: #2 is closed, #3 opened and closed, then #2 reopened, so the newest pull request of the branch is
	// closed and hides the open one from a look-up of the newest. It returns #2.
	reopenBehindNewer := func(got *remediationv1alpha1.RemediationRequest, body string) providers.ChangeRequest {
		GinkgoHelper()
		Expect(git.CloseChangeRequest(ctx, got.Status.PullRequest.Number)).To(Succeed())
		open := func() providers.ChangeRequest {
			cr, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
				BaseBranch: mainBranch, Title: "opened on the branch", Body: body})
			Expect(err).NotTo(HaveOccurred())
			return cr
		}
		two := open()
		Expect(git.CloseChangeRequest(ctx, two.Number)).To(Succeed())
		three := open()
		Expect(git.CloseChangeRequest(ctx, three.Number)).To(Succeed())
		Expect(git.Reopen(two.Number)).To(Succeed())
		newest, err := git.FindChangeRequest(ctx, freshness.BranchName(got))
		Expect(err).NotTo(HaveOccurred())
		Expect(newest.Number).To(Equal(three.Number))
		Expect(newest.State).To(Equal(providers.ChangeRequestClosed), "the newest hides the open one")
		return two
	}

	It("keeps the target of a stopped holder while an older pull request of its branch is open behind a closed newer one (#688)", func() {
		got := stopHeadMoved()
		reopened := reopenBehindNewer(got, "a body without a marker")
		_, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "#2 is open on the branch")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld).Message).
			To(ContainSubstring(fmt.Sprintf("#%d", reopened.Number)))
		Expect(git.CloseChangeRequest(ctx, reopened.Number)).To(Succeed())
		_, err = reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeFalse()))
	})

	It("keeps the target when a person closes the pull request while another one is open on its branch (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: "opened on the branch", Body: "a body without a marker"})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			To(Equal(ReasonChangeRequestClosed))
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "the watch's stop does not release the lock on its own")
		held := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)
		Expect(held).NotTo(BeNil())
		Expect(held.Message).To(ContainSubstring(fmt.Sprintf("#%d", next.Number)))
		_, reconcileSecond := second()
		expectLocked(reconcileSecond())

		Expect(git.CloseChangeRequest(ctx, next.Number)).To(Succeed())
		res, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}))
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)).To(BeNil())
	})

	It("stops the watch with ChangeRequestGone only on a 404 its branch confirms (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		git.SetFail(func(op string) error {
			if op == opGet {
				return fmt.Errorf("pull request: %w", providers.ErrNotFound)
			}
			return nil
		})
		_ = reconcileOnce()
		git.SetFail(nil)
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval),
			"the branch still lists the number: the 404 is a failed read")

		lost := func() (reconcile.Result, error) {
			r := reconciler()
			r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
				return &lostChangeRequest{Git: git, n: n}, nil
			}
			return r.Reconcile(ctx, key())
		}
		// The first look that finds it missing from the host and the branch is only kept in memory, and waits.
		start := clock
		res, err := lost()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the look is asked again")
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "one look is not enough")
		seen, at := firstLook()
		Expect(seen).To(Equal(n))
		Expect(at).To(BeTemporally("==", start))
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
		// Within the wait it still waits, and the first look is not moved.
		clock = start.Add(confirmWait - time.Second)
		_, _ = lost()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		_, at = firstLook()
		Expect(at).To(BeTemporally("==", start))
		// Once the wait is over, the same answer is gone.
		clock = start.Add(confirmWait)
		_, _ = lost()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			To(Equal(ReasonChangeRequestGone))
	})

	It("drops the first look at a missing pull request when a later read shows it open (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		lost := reconciler()
		lost.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return &lostChangeRequest{Git: git, n: n}, nil
		}
		_, _ = lost.Reconcile(ctx, key())
		Expect(lookedAt()).To(Equal(n))
		// The host answers again with the pull request open: the first look goes, so a later 404 waits again.
		clock = clock.Add(time.Minute)
		_, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(lookedAt()).To(BeZero(), "a read that shows the pull request drops the first look")
		clock = clock.Add(confirmWait)
		_, _ = lost.Reconcile(ctx, key())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval),
			"a 404 after a read that showed the pull request starts the wait again")
		_, at := firstLook()
		Expect(at).To(BeTemporally("==", clock))
	})

	It("drops the first look when the open listing shows the missing number, and the next 404 waits again (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		seenGone(n)
		// The read by number answers 404 and the listing in all states misses it, but the open listing has it.
		v := reconciler().changeOf(ctx, &openListedOnly{Git: git, n: n}, fetch(),
			changeQuery{marker: true, openOnBranch: true})
		Expect(v.state).To(Equal(changeUnknown))
		Expect(v.err).To(MatchError(errConflictingReads))
		Expect(lookedAt()).To(BeZero(), "the open listing showed the number: the first look is dropped")
		// A single 404 with nothing listed now waits again, never answers gone at once.
		v = reconciler().changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{marker: true})
		Expect(v.state).To(Equal(changeUnknown))
		Expect(v.err).To(MatchError(errGoneUnconfirmed))
	})

	It("waits again after a restart, after goneLookMaxAge and when the clock went back (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		lostIn := func(r *RemediationRequestReconciler) changeVerdict {
			return r.changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{marker: true})
		}
		before := reconciler()
		Expect(lostIn(before).err).To(MatchError(errGoneUnconfirmed))
		// A new executor process (a restart or a change of leader) has no first look: it waits two minutes again.
		clock = clock.Add(confirmWait)
		restarted := reconciler()
		restarted.goneLooks = &goneLooks{}
		v := lostIn(restarted)
		Expect(v.state).To(Equal(changeUnknown), "the first look of the earlier process is not this one's")
		Expect(v.err).To(MatchError(errGoneUnconfirmed))
		clock = clock.Add(confirmWait - time.Second)
		Expect(lostIn(restarted).err).To(MatchError(errGoneUnconfirmed), "still within its own wait")
		clock = clock.Add(time.Second)
		Expect(lostIn(restarted).state).To(Equal(changeGone), "its own first look, confirmWait old, confirms")
		// A first look older than goneLookMaxAge is no evidence any more: the wait starts again.
		process.note(fetch(), n, clock.Add(-goneLookMaxAge))
		Expect(lostIn(reconciler()).err).To(MatchError(errGoneUnconfirmed))
		_, at := firstLook()
		Expect(at).To(BeTemporally("==", clock), "the wait starts again from this look")
		// A first look ahead of the clock (it went back) is never confirmed.
		process.note(fetch(), n, clock.Add(time.Hour))
		Expect(lostIn(reconciler()).err).To(MatchError(errGoneUnconfirmed))
		_, at = firstLook()
		Expect(at).To(BeTemporally("==", clock))
	})

	It("never keeps a look at a missing pull request that a later read showing it follows (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		// Call A reads 404 and, before it settles, call B (another worker) asks to read the pull request: B waits
		// for A, then reads it open.
		var reads atomic.Int32
		var b <-chan changeVerdict
		a := reconciler().changeOf(ctx, &pausedLost{lostChangeRequest: lostChangeRequest{Git: git, n: n},
			pause: func() {
				b = startOtherWorker(func() changeVerdict {
					return reconciler().changeOf(ctx, &countedReads{GitProvider: git, reads: &reads}, fetch(), changeQuery{})
				})
				Eventually(lockHolders).Should(Equal(2), "B waits for the lock A holds")
				Expect(reads.Load()).To(BeZero(), "B reads nothing while A has not settled")
			}}, fetch(), changeQuery{})
		Expect(a.state).To(Equal(changeUnknown))
		Expect(a.err).To(MatchError(errGoneUnconfirmed), "A settles on its own reads")
		Expect((<-b).state).To(Equal(changeOpen))
		Expect(lookedAt()).To(BeZero(), "B's read, after A's, drops the first look A kept")
		// So one 404 two minutes later only starts the wait, never answers gone.
		clock = clock.Add(confirmWait)
		v := reconciler().changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{})
		Expect(v.state).To(Equal(changeUnknown))
		Expect(v.err).To(MatchError(errGoneUnconfirmed))
	})

	It("never confirms a missing pull request from a 404 that waited behind a read showing it (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		seenGone(n)
		// Call B reads the pull request open and, before it settles, call A (a 404) and then call C (another 404)
		// ask to read it. Both wait for B, then for each other.
		var reads atomic.Int32
		var a, c <-chan changeVerdict
		b := reconciler().changeOf(ctx, &pausedOpen{Git: git, pause: func() {
			a = startOtherWorker(func() changeVerdict {
				return reconciler().changeOf(ctx, &countedReads{GitProvider: &lostChangeRequest{Git: git, n: n},
					reads: &reads}, fetch(), changeQuery{})
			})
			Eventually(lockHolders).Should(Equal(2), "A waits for the lock B holds")
			Expect(reads.Load()).To(BeZero(), "A reads nothing while B has not settled")
		}}, fetch(), changeQuery{openOnBranch: true})
		Expect(b.state).To(Equal(changeOpen))
		va := <-a
		Expect(va.state).To(Equal(changeUnknown), "B's read dropped the first look before A read")
		Expect(va.err).To(MatchError(errGoneUnconfirmed))
		c = startOtherWorker(func() changeVerdict {
			return reconciler().changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{})
		})
		Expect((<-c).err).To(MatchError(errGoneUnconfirmed), "C finds A's first look younger than confirmWait")
		_, at := firstLook()
		Expect(at).To(BeTemporally("==", clock), "A's first look is kept")
		clock = clock.Add(confirmWait)
		Expect(reconciler().changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{}).state).
			To(Equal(changeGone), "a look confirmWait after A's confirms")
	})

	It("never answers gone while another read of the pull request is between a 404 and its listing (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		seenGone(n)
		// Call B reads the number open and pauses in its open listing. Call A, asked to read meanwhile, would read
		// 404 and an empty listing: it waits for B, which drops the first look, so A only starts the wait.
		var reads atomic.Int32
		var a <-chan changeVerdict
		b := reconciler().changeOf(ctx, &pausedOpen{Git: git, pause: func() {
			a = startOtherWorker(func() changeVerdict {
				return reconciler().changeOf(ctx, &countedReads{GitProvider: &lostChangeRequest{Git: git, n: n},
					reads: &reads}, fetch(), changeQuery{})
			})
			Eventually(lockHolders).Should(Equal(2))
			Expect(reads.Load()).To(BeZero(), "A neither reads by number nor lists while B has not settled")
		}}, fetch(), changeQuery{openOnBranch: true})
		Expect(b.state).To(Equal(changeOpen))
		va := <-a
		Expect(va.state).NotTo(Equal(changeGone), "a 404 never outlives a read that showed the pull request")
		Expect(va.err).To(MatchError(errGoneUnconfirmed))
		Expect(lockHolders()).To(BeZero())
	})

	It("drops the lock of a request with the request, also when a read held it as the request ended (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		// A read that finds the pull request missing holds the lock while the request ends.
		v := reconciler().changeOf(ctx, &pausedLost{lostChangeRequest: lostChangeRequest{Git: git, n: n},
			pause: func() {
				Expect(reconciler().stop(ctx, fetch(), remediationv1alpha1.PhaseNeedsIntervention, ReasonHeadMoved,
					"The head moved")).To(MatchError(errStopped))
				Expect(lockHolders()).To(Equal(1), "the entry stays while the read holds the lock")
			}}, fetch(), changeQuery{})
		Expect(v.err).To(MatchError(errGoneUnconfirmed))
		Expect(lookedAt()).To(Equal(n), "the read settled after the request ended")
		// The request goes: its entry, the lock with it, is dropped.
		Expect(k8sClient.Delete(ctx, fetch())).To(Succeed())
		base := fetch()
		patched := base.DeepCopy()
		controllerutil.RemoveFinalizer(patched, FinalizerName)
		Expect(k8sClient.Patch(ctx, patched, client.MergeFrom(base))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), &remediationv1alpha1.RemediationRequest{}))
		}).Should(BeTrue())
		Expect(reconcileOnce()).To(Succeed())
		process.mu.Lock()
		defer process.mu.Unlock()
		Expect(process.keys).NotTo(HaveKey(client.ObjectKeyFromObject(request)), "nothing is left of the request")
	})

	It("gives up the wait for the lock when the context ends, and reads nothing (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		key := client.ObjectKeyFromObject(got)
		Expect(process.lock(ctx, key)).To(Succeed())
		waiting, cancel := context.WithCancel(ctx)
		cancel()
		var reads atomic.Int32
		v := reconciler().changeOf(waiting, &countedReads{GitProvider: git, reads: &reads}, fetch(), changeQuery{})
		Expect(v.state).To(Equal(changeUnknown))
		Expect(v.err).To(MatchError(context.Canceled))
		Expect(reads.Load()).To(BeZero())
		Expect(lockHolders()).To(Equal(1), "the call that gave up holds nothing")
		process.unlock(key)
		process.mu.Lock()
		defer process.mu.Unlock()
		Expect(process.keys).NotTo(HaveKey(key), "the last unlock drops an entry that keeps no first look")
	})

	It("reads nothing when the context has ended and the lock is free (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		key := client.ObjectKeyFromObject(got)
		ended, cancel := context.WithCancel(ctx)
		cancel()
		var reads atomic.Int32
		// With the lock free both cases of the wait are ready, so the outcome of one try proves nothing.
		for range 64 {
			v := reconciler().changeOf(ended, &countedReads{GitProvider: git, reads: &reads}, fetch(), changeQuery{})
			Expect(v.state).To(Equal(changeUnknown))
			Expect(v.err).To(MatchError(context.Canceled))
		}
		Expect(reads.Load()).To(BeZero())
		process.mu.Lock()
		defer process.mu.Unlock()
		Expect(process.keys).NotTo(HaveKey(key), "a call that gave the lock back holds nothing")
	})

	It("does not confirm on a first look of an earlier request with the same name and number (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		earlier := got.DeepCopy()
		earlier.UID = "uid-of-an-earlier-request"
		process.note(earlier, n, clock.Add(-confirmWait))
		v := reconciler().changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{})
		Expect(v.state).To(Equal(changeUnknown))
		Expect(v.err).To(MatchError(errGoneUnconfirmed))
		at, ok := process.first(fetch(), n)
		Expect(ok).To(BeTrue(), "a fresh first look of this request is kept")
		Expect(at).To(BeTemporally("==", clock))
	})

	It("sweeps the expired first looks of every request when it keeps one (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(reconciler().changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{}).err).
			To(MatchError(errGoneUnconfirmed))
		Expect(lookedAt()).To(Equal(n))
		// The request is never read again; an hour later another request keeps a first look.
		clock = clock.Add(time.Hour)
		other := got.DeepCopy()
		other.Name, other.UID = other.Name+"-other", "uid-of-another-request"
		r := reconciler()
		v := r.settleGone(ctx, other, 7, true, changeVerdict{state: changeGone, gone: "missing"})
		Expect(v.err).To(MatchError(errGoneUnconfirmed))
		Expect(lookedAt()).To(BeZero(), "the expired first look of the first request is swept")
		process.mu.Lock()
		defer process.mu.Unlock()
		Expect(process.keys).To(HaveLen(1))
	})

	It("drops the first look at a missing pull request when the branch shows it and the adoption records it (#688)", func() {
		// The open's status write is lost, so the pull request is on the branch but not recorded.
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
		failStatus = false
		n := openRequests()[0].Number
		// An old look saw that number missing (a lagging replica), long enough ago to confirm.
		seenGone(n)
		// The adoption alone: no read by number has looked at the pull request since.
		got := drive(remediationv1alpha1.PhasePRCreated)
		Expect(got.Status.PullRequest.Number).To(Equal(n))
		Expect(lookedAt()).To(BeZero(), "the look that found the pull request dropped the first look")
		// One lagging 404 now only keeps a first look anew and waits.
		lost := reconciler()
		lost.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return &lostChangeRequest{Git: git, n: n}, nil
		}
		_, _ = lost.Reconcile(ctx, key())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated), "one 404 is not enough")
		_, at := firstLook()
		Expect(at).To(BeTemporally("==", clock))
	})

	It("says while the Git host refuses the read of the pull request, once, and clears it when a read works (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		recorder = record.NewFakeRecorder(20)
		git.SetFail(func(op string) error {
			if op == opGet {
				return fmt.Errorf("status 403: secret body: %w", providers.ErrForbidden)
			}
			return nil
		})
		unreadable := func() *metav1.Condition {
			return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
		}
		for range 3 {
			res, err := reconcileResult()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the read is retried as before")
			clock = clock.Add(time.Minute)
		}
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "a refused read decides nothing")
		c := unreadable()
		Expect(c).NotTo(BeNil(), "a refused read is not silent")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(ReasonGitForbidden))
		Expect(c.Message).To(ContainSubstring(fmt.Sprintf("#%d", n)))
		Expect(c.Message).NotTo(ContainSubstring("secret body"), "no answer of the host in the status")
		Expect(recorder.Events).To(HaveLen(1), "one Event per transition, not per retry")
		event := <-recorder.Events
		Expect(event).To(HavePrefix(corev1.EventTypeWarning + " " + EventPullRequestUnreadable))
		Expect(event).NotTo(ContainSubstring("secret body"))
		git.SetFail(nil)
		_, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(unreadable()).To(BeNil(), "the next read that works removes it")
		Expect(recorder.Events).To(HaveLen(1))
		Expect(<-recorder.Events).To(HavePrefix(corev1.EventTypeNormal + " " + EventPullRequestReadable))
	})

	It("drops the first look at a missing pull request when the request ends (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		seenGone(got.Status.PullRequest.Number)
		Expect(reconciler().stop(ctx, fetch(), remediationv1alpha1.PhaseNeedsIntervention, ReasonHeadMoved,
			"The head moved")).To(MatchError(errStopped))
		Expect(lookedAt()).To(BeZero(), "a later read of the ended request, by the lock or the finalizer, waits again")
	})

	It("drops a first look for another number, and never confirms one for another number (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		// A first look for another number, old enough to confirm that one.
		seenGone(n + 100)
		_, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(lookedAt()).To(BeZero(), "the read of the recorded number found the pull request")
		// Such a first look, kept again, is replaced for the missing number, never confirmed.
		seenGone(n + 100)
		lost := reconciler()
		lost.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return &lostChangeRequest{Git: git, n: n}, nil
		}
		res, err := lost.Reconcile(ctx, key())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "the old look names another number")
		seen, at := firstLook()
		Expect(seen).To(Equal(n))
		Expect(at).To(BeTemporally("==", clock))
	})

	It("never takes a recorded pull request for gone while its branch lists another of this request (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		seenGone(n)
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: openedLater, Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		Expect(next.Number).NotTo(Equal(n))
		lost := reconciler()
		lost.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return &lostChangeRequest{Git: git, n: n}, nil
		}
		_, _ = lost.Reconcile(ctx, key())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval),
			"the listing that checks the 404 shows a pull request of this request")
		Expect(lookedAt()).To(BeZero(), "that listing dropped the old first look")
	})

	It("shows a recorded pull request that is missing while its branch lists another of this request, once, until a read answers (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: openedLater, Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		recorder = record.NewFakeRecorder(20)
		lost := reconciler()
		lost.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return &lostChangeRequest{Git: git, n: n}, nil
		}
		for range 3 {
			_, _ = lost.Reconcile(ctx, key())
			clock = clock.Add(confirmWait)
		}
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "nothing is decided on it")
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
		Expect(c).NotTo(BeNil(), "a request in the known limit is shown to a person")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(ReasonRecordedMissingOtherListed))
		Expect(c.Message).To(ContainSubstring(fmt.Sprintf("#%d", n)))
		Expect(c.Message).To(ContainSubstring(fmt.Sprintf("#%d", next.Number)))
		Expect(c.Message).To(ContainSubstring("Delete the request"))
		Expect(recorder.Events).To(HaveLen(1), "one Event per transition, not per retry")
		Expect(<-recorder.Events).To(HavePrefix(corev1.EventTypeWarning + " " + EventPullRequestUnreadable))
		// The next read that answers (the Git host has the recorded number again) removes it.
		_, _ = reconcileResult()
		Expect(meta.FindStatusCondition(fetch().Status.Conditions,
			remediationv1alpha1.ConditionPullRequestUnreadable)).To(BeNil())
		var events []string
		for len(recorder.Events) > 0 {
			events = append(events, <-recorder.Events)
		}
		Expect(events).To(ContainElement(HavePrefix(corev1.EventTypeNormal + " " + EventPullRequestReadable)))
	})

	It("never answers gone while the open listing of the branch shows a pull request of this request (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: openedLater, Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		seenGone(n)
		// The listing in all states misses the newer one; the open listing has it.
		g := &missedInAll{lostChangeRequest: lostChangeRequest{Git: git, n: n}, missed: next.Number}
		v := reconciler().changeOf(ctx, g, fetch(), changeQuery{marker: true, openOnBranch: true})
		Expect(v.state).To(Equal(changeUnknown), "a read in the call showed a pull request of this request")
		Expect(v.err).To(MatchError(errOwnListed))
		Expect(lookedAt()).To(BeZero(), "that read drops the first look")
	})

	It("settles the first look at a missing pull request without reading the API server (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		reads := &countingReader{Reader: k8sClient}
		r := reconciler()
		r.APIReader = reads
		seenGone(n + 100)
		Expect(r.changeOf(ctx, git, fetch(), changeQuery{marker: true}).state).To(Equal(changeOpen))
		Expect(lookedAt()).To(BeZero())
		v := r.changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{})
		Expect(v.err).To(MatchError(errGoneUnconfirmed))
		clock = clock.Add(confirmWait)
		Expect(r.changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{}).state).To(Equal(changeGone))
		Expect(reads.requests.Load()).To(BeZero(), "the first look is this process's memory, not stored state")
	})

	It("drops the first look when a refused opened pull request is read, although the close is retried (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		cr, err := git.GetChangeRequest(ctx, n)
		Expect(err).NotTo(HaveOccurred())
		seenGone(n)
		rr := fetch()
		git.SetFail(func(op string) error {
			if op == opClose {
				return errors.New("status 502: bad gateway")
			}
			return nil
		})
		err = reconciler().refuseOpened(ctx, git, rr, cr, &renderRefusal{reason: ReasonHeadMoved, message: "The head moved"})
		git.SetFail(nil)
		Expect(err).To(HaveOccurred(), "the close is retried")
		Expect(lookedAt()).To(BeZero(), "the read that found the pull request dropped the first look")
		// One 404 for that number now only keeps a first look anew and waits.
		v := reconciler().changeOf(ctx, &lostChangeRequest{Git: git, n: n}, fetch(), changeQuery{})
		Expect(v.state).To(Equal(changeUnknown))
		Expect(v.err).To(MatchError(errGoneUnconfirmed))
	})

	It("keeps PullRequestUnreadable that a refused opened pull request wrote when its close waits for the kill switch (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		cr, err := git.GetChangeRequest(ctx, n)
		Expect(err).NotTo(HaveOccurred())
		rr := fetch()
		git.SetFail(func(op string) error {
			switch op {
			case opGet:
				return fmt.Errorf("status 403: %w", providers.ErrForbidden)
			case opClose:
				return freshness.ErrPaused
			}
			return nil
		})
		err = reconciler().refuseOpened(ctx, git, rr, cr, &renderRefusal{reason: ReasonHeadMoved, message: "The head moved"})
		git.SetFail(nil)
		Expect(err).To(MatchError(freshness.ErrPaused))
		_, err = reconciler().waitWhilePaused(ctx, rr)
		Expect(err).NotTo(HaveOccurred())
		got = fetch()
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionWritesPaused)).To(BeTrue())
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
		Expect(c).NotTo(BeNil(), "the status write of the pause keeps what the refused read wrote")
		Expect(c.Reason).To(Equal(ReasonGitForbidden))
	})

	It("closes a request's pull request by its recorded number, never a newer one of the branch (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: openedLater, Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		Expect(next.Number).NotTo(Equal(n))
		Expect(reconciler().closeChangeRequest(ctx, fetch())).To(Succeed(),
			"the recorded pull request is this request's, and closing a closed one succeeds")
		crs := git.ChangeRequests()
		Expect(crs[n-1].State).To(Equal(providers.ChangeRequestClosed))
		Expect(crs[next.Number-1].State).To(Equal(providers.ChangeRequestOpen), "the newer pull request is not touched")
	})

	It("never takes a 404 its branch does not confirm, or an answer about another pull request, as the end of a close by number (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		for name, fault := range map[string]error{
			"404":                  fmt.Errorf("pull request: %w", providers.ErrNotFound),
			"another pull request": fmt.Errorf("pull request: %w", providers.ErrUntrustedAnswer),
		} {
			git.SetFail(func(op string) error {
				if op == opGet {
					return fault
				}
				return nil
			})
			err := reconciler().closeChangeRequest(ctx, fetch())
			git.SetFail(nil)
			Expect(err).To(HaveOccurred(), name)
			Expect(errors.Is(err, errChangeRequestNotOurs)).To(BeFalse(), name+": a failed read is retried, never an answer")
			Expect(git.ChangeRequests()[n-1].State).To(Equal(providers.ChangeRequestOpen), name)
		}
		r := reconciler()
		r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return &lostChangeRequest{Git: git, n: n}, nil
		}
		err := r.closeChangeRequest(ctx, fetch())
		Expect(errors.Is(err, errGoneUnconfirmed)).To(BeTrue(), "a first 404 the branch confirms waits: %v", err)
		clock = clock.Add(confirmWait)
		Expect(errors.Is(r.closeChangeRequest(ctx, fetch()), errChangeRequestNotOurs)).To(BeTrue(),
			"a 404 the branch confirms on two looks is an answer")
		Expect(reconciler().closeChangeRequest(ctx, fetch())).To(Succeed())
		Expect(git.ChangeRequests()[n-1].State).To(Equal(providers.ChangeRequestClosed))
	})

	// forgetRecorded stands for a request whose status write after the open was lost: no pull request recorded.
	forgetRecorded := func() {
		GinkgoHelper()
		got := fetch()
		got.Status.PullRequest = nil
		meta.RemoveStatusCondition(&got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
	}

	It("closes the open pull request with its marker behind a closed newer one when none is recorded (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		reopened := reopenBehindNewer(got, pullRequestBody(got, "", nil))
		forgetRecorded()
		Expect(reconciler().closeChangeRequest(ctx, fetch())).To(Succeed())
		Expect(git.ChangeRequests()[reopened.Number-1].State).To(Equal(providers.ChangeRequestClosed),
			"every pull request of the branch with the marker is looked at, not only the newest")
	})

	It("closes on deletion the open pull request with its marker behind a closed newer one when none is recorded (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		reopened := reopenBehindNewer(got, pullRequestBody(got, "", nil))
		forgetRecorded()
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[reopened.Number-1].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("keeps the finalizer when the listing of the branch answers 404 and none is recorded (#688)", func() {
		drive(remediationv1alpha1.PhaseAwaitingApproval)
		forgetRecorded()
		git.SetFail(func(op string) error {
			if op == opList {
				return fmt.Errorf("pull requests: %w", providers.ErrNotFound)
			}
			return nil
		})
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		_ = reconcileOnce()
		Consistently(gone, "300ms").Should(BeFalse(), "a 404 never means none was opened")
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		git.SetFail(nil)
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("keeps the target of a stopped holder with none recorded while its open pull request hides behind a closed newer one (#688)", func() {
		got := stopHeadMoved()
		reopened := reopenBehindNewer(got, pullRequestBody(got, "", nil))
		forgetRecorded()
		_, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
		Expect(got.Status.PullRequest).NotTo(BeNil())
		Expect(got.Status.PullRequest.Number).To(Equal(reopened.Number), "the open one is recorded, not the newest")
	})

	It("keeps the lock when its own pull request, read closed by its number, is listed open on the branch (#688)", func() {
		got := stopHeadMoved()
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		before := fetch().ResourceVersion
		// The listing says open: the pull request was reopened between the read by number and the listing, or the
		// read lags. Either way the two reads conflict, and the lock is kept until they agree.
		r := reconciler()
		r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return &laggingList{Git: git, n: n}, nil
		}
		res, err := r.Reconcile(ctx, key())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(heldRecheckInterval), "held and looked at again")
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "the listing says it is open")
		Expect(got.ResourceVersion).To(Equal(before), "conflicting reads write nothing")
		// The reads agree again: closed, and nothing else open on the branch.
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeFalse()))
	})

	It("clears TargetHeld once the pull request that held the lock closes and the recorded one is reopened (#688)", func() {
		got := stopHeadMoved()
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: "opened after the close", Body: "a body without a marker"})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionTargetHeld)).NotTo(BeNil())
		Expect(git.CloseChangeRequest(ctx, next.Number)).To(Succeed())
		Expect(git.Reopen(n)).To(Succeed())
		_, err = reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "the recorded pull request is open again")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)).To(BeNil(),
			"TargetHeld no longer names a pull request that holds the lock")
	})

	It("frees the target of a stopped holder whose recorded pull request is confirmed gone and none is open (#688)", func() {
		got := stopHeadMoved()
		Expect(git.CloseChangeRequest(ctx, got.Status.PullRequest.Number)).To(Succeed())
		// The recorded number names no pull request of the host any more, and the branch lists none with it, nor
		// one with this request's marker (a person edited it out of the closed one).
		Expect(git.SetBody(got.Status.PullRequest.Number, unmarkedBody)).To(Succeed())
		got.Status.PullRequest.Number = 9999
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		seenGone(9999)
		res, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}), "a released lock is not looked at again")
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeFalse()))
	})

	It("keeps the lock of a stopped holder whose recorded pull request is gone while another is open on its branch (#688)", func() {
		got := stopHeadMoved()
		// The pull request of the request stays open on the branch without its marker (a person edited it out).
		Expect(git.SetBody(got.Status.PullRequest.Number, unmarkedBody)).To(Succeed())
		got.Status.PullRequest.Number = 9999
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		seenGone(9999)
		res, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(heldRecheckInterval))
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "the open pull request of the branch may still deliver")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)).NotTo(BeNil())
	})

	It("frees the target once a person merges the pull request of a stopped holder", func() {
		got := stopHeadMoved()
		_, err := git.Merge(got.Status.PullRequest.Number)
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonChangeRequestMerged))
	})

	It("keeps the lock while the pull request of a stopped holder cannot be looked up", func() {
		stopHeadMoved()
		openErr = errors.New("the Git host is down")
		res, err := reconcileResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(heldRecheckInterval))
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()))
		// The second request's policy check reads its build from Git (the render budget, #482), as the source
		// mapping would, so it is decided with the host back.
		openErr = nil
		_, reconcileSecond := second()
		expectLocked(reconcileSecond())
	})

	It("closes the pull request of a deleted request that holds the lock, whatever its phase", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		// A phase deletion did not close for before (Failed is set after a merge only once that is built).
		got.Status.Phase = remediationv1alpha1.PhaseFailed
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("frees the target once the executor sees the pull request closed", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(git.CloseChangeRequest(ctx, got.Status.PullRequest.Number)).To(Succeed())
		_, reconcileSecond := second()
		expectLocked(reconcileSecond()) // the first request still waits for review in its status
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		expectClosedElsewhere(reconcileSecond())
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
		Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)).
			To(BeNil(), "a pause is no failure and starts no bound")
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
		live.Spec.Paused = new(false)
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	// A step of the cleanup the Git host refuses for good on deletion (#288): the token, the read of the pull request,
	// the listing of the branch or the close. The finalizer holds the request and tries again for closeRefusedGrace
	// after the first refusal since the deletion began, then lets it go and says so.
	Context("when the Git host refuses a step of the cleanup on deletion for good (#288)", func() {
		var closeErr error // the error of every close; nil lets it through
		BeforeEach(func() {
			recorder = record.NewFakeRecorder(20)
			closeErr = nil
		})
		failOp := func(failing string, err error) {
			closeErr = err
			git.SetFail(func(op string) error {
				if op == failing {
					return closeErr
				}
				return nil
			})
		}
		failClose := func(err error) { failOp(opClose, err) }
		refused403 := fmt.Errorf("status 403 from https://github.com/example/fleet with token ghs_leaked: %w",
			providers.ErrForbidden)
		// warnings returns the reasons of the Warning Events recorded since the last call, and their messages.
		warnings := func() (reasons, messages []string) {
			for len(recorder.Events) > 0 {
				e := <-recorder.Events
				if rest, ok := strings.CutPrefix(e, corev1.EventTypeWarning+" "); ok {
					reason, message, _ := strings.Cut(rest, " ")
					reasons, messages = append(reasons, reason), append(messages, message)
				}
			}
			return reasons, messages
		}
		// refusedOf returns condition CleanupRefused of the request.
		refusedOf := func() *metav1.Condition {
			return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionCleanupRefused)
		}
		// failingOf returns condition CleanupFailing of the request.
		failingOf := func() *metav1.Condition {
			return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)
		}

		// leftOpenOf returns the messages of the PullRequestLeftOpen Events recorded since the last call.
		leftOpenOf := func() []string {
			reasons, messages := warnings()
			var out []string
			for i, r := range reasons {
				if r == EventPullRequestLeftOpen {
					out = append(out, messages[i])
				}
			}
			return out
		}

		// refuse makes step fail for good with class: "token" fails opening the repository, "read" the read of the
		// pull request by its number, "close" the close.
		refuse := func(step string, class error) {
			err := fmt.Errorf("status 403 from https://github.com/example/fleet with token ghs_leaked: %w", class)
			switch step {
			case "token":
				openErr = err
			case "read":
				failOp(opGet, err)
			default:
				failClose(err)
			}
		}

		DescribeTable("holds the finalizer and lets the request go only after the grace",
			func(step string, class error, reason, says, leftSays string) {
				pr := drive(remediationv1alpha1.PhaseAwaitingApproval).Status.PullRequest
				name := fmt.Sprintf("#%d (%s)", pr.Number, pr.URL)
				refuse(step, class)
				Expect(k8sClient.Delete(ctx, request)).To(Succeed())
				deleted := clock

				res, err := reconcileResult()
				Expect(err).NotTo(HaveOccurred(), "a final refusal is not handed to the error backoff")
				Expect(res.RequeueAfter).To(Equal(closeRefusedRetry))
				Expect(gone()).To(BeFalse(), "the finalizer holds the request")
				c := refusedOf()
				Expect(c).NotTo(BeNil())
				Expect(c.Status).To(Equal(metav1.ConditionTrue))
				Expect(c.Reason).To(Equal(reason))
				Expect(c.Message).To(ContainSubstring(says), "the step")
				Expect(c.Message).To(ContainSubstring(name), "the recorded pull request")
				Expect(c.Message).NotTo(ContainSubstring("ghs_leaked"))
				Expect(c.Message).NotTo(ContainSubstring("status 403"))
				Expect(c.LastTransitionTime.Time).To(BeTemporally("==", deleted), "on the executor's clock")
				Expect(meta.FindStatusCondition(fetch().Status.Conditions,
					remediationv1alpha1.ConditionPullRequestNotClosed)).To(BeNil(), "that one is the stale plan's")
				reasons, _ := warnings()
				Expect(reasons).To(ContainElement(EventCleanupRefused))
				Expect(reasons).NotTo(ContainElement(EventPullRequestLeftOpen))

				// Refused again when the grace has passed since the persisted second: held one second more (boundLeft),
				// no new write and no new Event, the first refusal kept.
				clock = deleted.Add(closeRefusedGrace)
				before := fetch().ResourceVersion
				res, err = reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(time.Second), "tried again when the grace ends")
				Expect(gone()).To(BeFalse())
				Expect(fetch().ResourceVersion).To(Equal(before))
				reasons, _ = warnings()
				Expect(reasons).NotTo(ContainElement(EventCleanupRefused))

				// Past the grace: let go, the pull request untouched, and an Event names it.
				clock = deleted.Add(closeRefusedGrace + time.Second)
				Expect(reconcileOnce()).To(Succeed())
				Eventually(gone).Should(BeTrue())
				Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
				left := leftOpenOf()
				Expect(left).To(HaveLen(1))
				Expect(left[0]).To(ContainSubstring(name))
				Expect(left[0]).To(ContainSubstring(reason))
				Expect(left[0]).To(ContainSubstring(leftSays))
				Expect(left[0]).NotTo(ContainSubstring("ghs_leaked"))
			},
			Entry("close refused (403)", "close", providers.ErrForbidden, ReasonGitForbidden,
				"the close of pull request", "is left open"),
			Entry("close refused (422)", "close", providers.ErrRejected, ReasonGitRejected,
				"the close of pull request", "is left open"),
			Entry("read refused: the App lost access to the repository (403)", "read", providers.ErrForbidden,
				ReasonGitForbidden, "the read of the recorded pull request", "unknown and it may be open"),
			Entry("token refused (403)", "token", providers.ErrForbidden, ReasonGitForbidden,
				"the access token", "unknown and it may be open"),
		)

		It("lets a request without a recorded pull request go after the grace when the listing is refused", func() {
			got := drive(remediationv1alpha1.PhasePolicyChecked)
			Expect(got.Status.PullRequest).To(BeNil())
			failOp(opList, refused403)
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			deleted := clock
			_, err := reconcileResult()
			Expect(err).NotTo(HaveOccurred())
			Expect(gone()).To(BeFalse())
			Expect(refusedOf().Message).To(ContainSubstring("the listing of the pull requests of branch"))
			Expect(refusedOf().Message).To(ContainSubstring("no pull request is recorded"))
			clock = deleted.Add(closeRefusedGrace + time.Second)
			Expect(reconcileOnce()).To(Succeed())
			Eventually(gone).Should(BeTrue())
			left := leftOpenOf()
			Expect(left).To(HaveLen(1))
			Expect(left[0]).To(ContainSubstring("any pull request of branch"))
		})

		It("counts the grace from the first refusal after the deletion, not from an older refusal of a stale plan", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			// The stale plan's close was refused days ago: PullRequestNotClosed with an old transition time.
			got := fetch()
			meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
				Type: remediationv1alpha1.ConditionPullRequestNotClosed, Status: metav1.ConditionTrue,
				Reason: ReasonGitForbidden, Message: "refused", LastTransitionTime: metav1.NewTime(clock.Add(-72 * time.Hour)),
			})
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
			failClose(refused403)
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			res, err := reconcileResult()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(closeRefusedRetry))
			Expect(gone()).To(BeFalse(), "the grace starts now")
			Expect(refusedOf().LastTransitionTime.Time).To(BeTemporally("==", clock))
		})

		It("never lets the request go on failures worth a retry within 24 hours", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			failClose(&providers.RetryableError{Err: errors.New("bad gateway")})
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			for range 3 {
				clock = clock.Add(closeRefusedGrace)
				Expect(reconcileOnce()).NotTo(Succeed(), "retried with backoff")
				Expect(gone()).To(BeFalse())
			}
			Expect(refusedOf()).To(BeNil(), "a failure worth a retry is no refusal")
			Expect(failingOf()).NotTo(BeNil(), "but a failure")
			Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		})

		It("keeps holding past the 15 minutes while an attempt fails other than by a refusal, until the next refused attempt releases it", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			failClose(refused403)
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			first := clock
			Expect(reconcileOnce()).To(Succeed())
			Expect(refusedOf()).NotTo(BeNil())

			// Past the grace, a close that fails for a reason worth a retry, or a read of the pull request that
			// fails without a refusal, keeps the finalizer and the time of the first refusal.
			clock = first.Add(closeRefusedGrace + time.Minute)
			failClose(&providers.RetryableError{Err: errors.New("bad gateway")})
			Expect(reconcileOnce()).NotTo(Succeed())
			Expect(gone()).To(BeFalse())
			failOp(opGet, &providers.RetryableError{Err: errors.New("timeout")})
			Expect(reconcileOnce()).NotTo(Succeed())
			Expect(gone()).To(BeFalse(), "a failed read that is not a refusal is retried")
			Expect(refusedOf().LastTransitionTime.Time).To(BeTemporally("==", first), "the first refusal is kept")

			// The next refusal lets it go at once: the grace counts from the first one.
			failClose(refused403)
			Expect(reconcileOnce()).To(Succeed())
			Eventually(gone).Should(BeTrue())
		})

		It("lets the request go once a retried close goes through within the grace", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			failClose(refused403)
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			Expect(reconcileOnce()).To(Succeed())
			Expect(gone()).To(BeFalse())
			clock = clock.Add(closeRefusedRetry)
			failClose(nil)
			Expect(reconcileOnce()).To(Succeed())
			Eventually(gone).Should(BeTrue())
			Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
			Expect(leftOpenOf()).To(BeEmpty(), "nothing was left open")
		})

		It("clears CleanupRefused and CleanupFailing in one write when the cleanup completes after a refusal", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			// Another finalizer keeps the deleted request readable after Shadewright's goes.
			const other = "example.com/keep"
			got := fetch()
			base := got.DeepCopy()
			controllerutil.AddFinalizer(got, other)
			Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
			DeferCleanup(func() {
				got := &remediationv1alpha1.RemediationRequest{}
				if k8sClient.Get(ctx, key().NamespacedName, got) == nil {
					base := got.DeepCopy()
					if controllerutil.RemoveFinalizer(got, other) {
						Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
					}
				}
			})
			failClose(refused403)
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			Expect(reconcileOnce()).To(Succeed())
			Expect(refusedOf()).NotTo(BeNil())
			Expect(failingOf()).NotTo(BeNil())

			clock = clock.Add(closeRefusedRetry)
			failClose(nil)
			Expect(reconcileOnce()).To(Succeed())
			after := fetch()
			Expect(controllerutil.ContainsFinalizer(after, FinalizerName)).To(BeFalse(), "the cleanup completed")
			Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
			Expect(meta.IsStatusConditionTrue(after.Status.Conditions, remediationv1alpha1.ConditionCleanupRefused)).
				To(BeFalse(), "a completed cleanup shows no refusal")
			Expect(meta.IsStatusConditionTrue(after.Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)).
				To(BeFalse(), "nor a failure")
		})

		// Failures no refusal class names (#288): the token mint answered 422 or 404 (a repository removed from the
		// App's installation, the App uninstalled), a signer that cannot sign, 404s on the reads of a cached token
		// for a repository outside the installation. The second bound ends them: 24 hours after the first failure.
		DescribeTable("holds a deleted request on any failure of the cleanup and lets it go only after 24 hours",
			func(fault func()) {
				pr := drive(remediationv1alpha1.PhaseAwaitingApproval).Status.PullRequest
				name := fmt.Sprintf("#%d (%s)", pr.Number, pr.URL)
				fault()
				Expect(k8sClient.Delete(ctx, request)).To(Succeed())
				deleted := clock

				_ = reconcileOnce()
				Expect(gone()).To(BeFalse(), "the finalizer holds the request")
				c := failingOf()
				Expect(c).NotTo(BeNil(), "set at the first failed attempt")
				Expect(c.Status).To(Equal(metav1.ConditionTrue))
				Expect(c.Reason).To(Equal(ReasonCleanupFailed))
				Expect(c.Message).To(ContainSubstring(name), "the recorded pull request")
				Expect(c.Message).NotTo(ContainSubstring("ghs_leaked"))
				Expect(c.Message).NotTo(ContainSubstring("status 422"))
				Expect(c.LastTransitionTime.Time).To(BeTemporally("==", deleted), "on the executor's clock")
				Expect(refusedOf()).To(BeNil(), "no refusal class")
				reasons, _ := warnings()
				Expect(reasons).To(ContainElement(EventCleanupFailing))
				Expect(reasons).NotTo(ContainElement(EventPullRequestLeftOpen))

				// At the bound from the persisted second: held one second more (boundLeft), the first failure kept, no new Event.
				clock = deleted.Add(cleanupFailingBound)
				res, err := reconcileResult()
				if err == nil {
					Expect(res.RequeueAfter).To(BeNumerically("<=", time.Second), "never later than the bound")
				}
				Expect(gone()).To(BeFalse())
				Expect(failingOf().LastTransitionTime.Time).To(BeTemporally("==", deleted))
				reasons, _ = warnings()
				Expect(reasons).To(BeEmpty())

				// Past the bound: let go, and an Event says the state of the pull request is not known.
				clock = deleted.Add(cleanupFailingBound + time.Second)
				Expect(reconcileOnce()).To(Succeed())
				Eventually(gone).Should(BeTrue())
				Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
				left := leftOpenOf()
				Expect(left).To(HaveLen(1))
				Expect(left[0]).To(ContainSubstring("could not be finished for 24 hours"))
				Expect(left[0]).To(ContainSubstring(name))
				Expect(left[0]).To(ContainSubstring("not known and it may be open"))
				Expect(left[0]).NotTo(ContainSubstring("ghs_leaked"))
			},
			Entry("token mint answered 422 (repository removed from the installation)", func() {
				openErr = errors.New("mint the installation token: status 422 from https://api.github.com with ghs_leaked")
			}),
			Entry("token mint answered 404 (App uninstalled)", func() {
				openErr = fmt.Errorf("mint the installation token with ghs_leaked: %w", providers.ErrNotFound)
			}),
			Entry("signer error (Key Vault cannot sign, key deleted)", func() {
				openErr = errors.New("sign the App JWT: the key ghs_leaked is disabled")
			}),
			Entry("reads answer 404 with a cached token", func() {
				git.SetFail(func(op string) error {
					if op == opGet || op == opList {
						return fmt.Errorf("status 404 with ghs_leaked: %w", providers.ErrNotFound)
					}
					return nil
				})
			}),
		)

		It("clears CleanupFailing once the cleanup finishes within 24 hours, and lets the request go without a warning", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			failClose(&providers.RetryableError{Err: errors.New("bad gateway")})
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			Expect(reconcileOnce()).NotTo(Succeed(), "retried with backoff")
			Expect(failingOf()).NotTo(BeNil())
			_, _ = warnings()

			// The failure stops before the bound; the patch is held back so the status can be read.
			clock = clock.Add(cleanupFailingBound - time.Minute)
			failClose(nil)
			r := reconciler()
			r.Client = &patchGone{Client: k8sClient}
			_, err := r.Reconcile(ctx, key())
			Expect(err).NotTo(HaveOccurred())
			Expect(failingOf()).To(BeNil(), "cleared when the cleanup finishes")
			Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))

			Expect(reconcileOnce()).To(Succeed())
			Eventually(gone).Should(BeTrue())
			reasons, _ := warnings()
			Expect(reasons).To(BeEmpty(), "no Warning Event")
		})

		It("lets a refused cleanup go after 24 hours of failures even when the refusal is younger than the grace", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			failClose(&providers.RetryableError{Err: errors.New("bad gateway")})
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			first := clock
			Expect(reconcileOnce()).NotTo(Succeed())
			clock = first.Add(cleanupFailingBound + time.Second)
			failClose(refused403)
			Expect(reconcileOnce()).To(Succeed())
			Eventually(gone).Should(BeTrue())
			left := leftOpenOf()
			Expect(left).To(HaveLen(1))
			Expect(left[0]).To(ContainSubstring("could not be finished for 24 hours"))
		})

		It("records the Event before the finalizer patch, so a patch that finds the request gone loses nothing", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			failClose(refused403)
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			Expect(reconcileOnce()).To(Succeed())
			clock = clock.Add(closeRefusedGrace + time.Second)
			r := reconciler()
			r.Client = &patchGone{Client: k8sClient}
			_, err := r.Reconcile(ctx, key())
			Expect(err).NotTo(HaveOccurred(), "a request already gone is no error")
			Expect(leftOpenOf()).To(HaveLen(1))
		})
	})

	It("closes on deletion the open pull request of its branch behind a closed newer one that carries its marker (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		reopened := reopenBehindNewer(got, pullRequestBody(got, "", nil))
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[reopened.Number-1].State).To(Equal(providers.ChangeRequestClosed),
			"#2 carries this request's marker: it is this request's change, whatever its number")
	})

	It("closes on deletion the open pull request with its marker when the recorded number is gone (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: "opened again", Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		r := reconciler()
		r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return &lostChangeRequest{Git: git, n: n}, nil
		}
		_, err = r.Reconcile(ctx, key())
		Expect(err).NotTo(HaveOccurred(), "a person deleted the holder: the branch's pull requests with its marker are its change")
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[next.Number-1].State).To(Equal(providers.ChangeRequestClosed),
			"a 404 on the recorded number never hides an open pull request with this request's marker")
	})

	It("keeps the finalizer when the read that showed another pull request of the request on deletion fails afterwards (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: "opened again", Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		r := reconciler()
		// The read shows #next with this request's marker (errOwnListed), then the write of condition
		// PullRequestUnreadable must read the stored request, and that read fails: the verdict is unknown again,
		// without the listings, and must not be taken for the deletion exception.
		r.APIReader = failingRequestReader{Reader: k8sClient, name: got.Name}
		err = r.closeAllOwn(ctx, &lostChangeRequest{Git: git, n: n}, fetch())
		Expect(err).To(HaveOccurred(), "a verdict without its listings is retried")
		Expect(err).To(MatchError(errOwnListed), "the error still says what the read showed")
		Expect(git.ChangeRequests()[next.Number-1].State).To(Equal(providers.ChangeRequestOpen))
	})

	It("closes on deletion an open pull request with its marker that only the open listing shows (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(got),
			BaseBranch: mainBranch, Title: openedLater, Body: pullRequestBody(got, "", nil)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		r := reconciler()
		r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			// The listing in all states misses #next; only the open listing has it.
			return &missedInAll{lostChangeRequest: lostChangeRequest{Git: git, n: n}, missed: next.Number}, nil
		}
		_, err = r.Reconcile(ctx, key())
		Expect(err).NotTo(HaveOccurred())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[next.Number-1].State).To(Equal(providers.ChangeRequestClosed),
			"the open listing is read for the close as well as the listing that checked the 404")
	})

	It("lets the finalizer go when a person removed the marker from the open recorded pull request (#688)", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		n := got.Status.PullRequest.Number
		Expect(git.SetBody(n, unmarkedBody)).To(Succeed())
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed(), "open without the marker is not a conflict with the open listing")
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[n-1].State).To(Equal(providers.ChangeRequestOpen),
			"a pull request without this request's marker is not its change and is left alone")
	})

	It("keeps the finalizer while the recorded number cannot be read, and never takes that as finished (#688)", func() {
		for name, fault := range map[string]func(op string) error{
			"another pull request answered": func(op string) error {
				if op == opGet {
					return fmt.Errorf("pull request: %w", providers.ErrUntrustedAnswer)
				}
				return nil
			},
			"a read refused as a conflict": func(op string) error {
				if op == opGet {
					return fmt.Errorf("pull request: %w", providers.ErrConflict)
				}
				return nil
			},
			"404 on both look-ups": func(op string) error {
				if op == opGet || op == opList {
					return fmt.Errorf("pull request: %w", providers.ErrNotFound)
				}
				return nil
			},
		} {
			By(name)
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			n := got.Status.PullRequest.Number
			git.SetFail(fault)
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			_ = reconcileOnce()
			Consistently(gone, "300ms").Should(BeFalse(), name)
			Expect(git.ChangeRequests()[n-1].State).To(Equal(providers.ChangeRequestOpen), name)
			git.SetFail(nil)
			Expect(reconcileOnce()).To(Succeed())
			Eventually(gone).Should(BeTrue())
			Expect(git.ChangeRequests()[n-1].State).To(Equal(providers.ChangeRequestClosed), name)
			// A new request for the next case.
			request = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("pr-rr-%d-%s", seq, strings.ReplaceAll(name, " ", "-")))
			request.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
			request.Spec.TargetRef.Name = deployment.Name
			request.Spec.TargetRef.UID = remediationv1alpha1.UID(deployment.UID)
			Expect(k8sClient.Create(ctx, request)).To(Succeed())
		}
	})

	It("keeps the finalizer while the branch still lists the number the Git host answers 404 for (#688)", func() {
		drive(remediationv1alpha1.PhaseAwaitingApproval)
		git.SetFail(func(op string) error {
			if op == opGet {
				return fmt.Errorf("pull request: %w", providers.ErrNotFound)
			}
			return nil
		})
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		_ = reconcileOnce()
		Consistently(gone, "300ms").Should(BeFalse(), "a 404 the branch does not confirm is a failed read")
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		git.SetFail(nil)
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("keeps the finalizer when the read by branch after a 404 fails (#688)", func() {
		drive(remediationv1alpha1.PhaseAwaitingApproval)
		git.SetFail(func(op string) error {
			switch op {
			case opGet:
				return fmt.Errorf("pull request: %w", providers.ErrNotFound)
			case opList:
				return &providers.RetryableError{Err: errors.New("connection reset")}
			}
			return nil
		})
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		_ = reconcileOnce()
		Consistently(gone, "300ms").Should(BeFalse(), "a 404 alone never finishes the cleanup")
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		git.SetFail(nil)
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
	// looks up the change request, by its branch or by its recorded number, which it does after the freshness check
	// passed and before any write.
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
				if (op == opList || op == opGet) && armed {
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

		// Issue #338: the gate is also the provider's write checkpoint, asked before each write request of one
		// method. The fake commits in two writes (the commit object, then the branch); the pause is set between.
		It("stops between two writes of one commit, keeps the state and resumes without a duplicate", func() {
			defer git.SetBetween(nil)
			Expect(reconcileOnce()).To(Succeed()) // finalizer
			git.SetBetween(func(op string) {
				if op == opCommit {
					git.SetBetween(nil)
					setPaused(true)
				}
			})
			for range 5 {
				if pausedCondition() != nil {
					break
				}
				res, err := reconcileResult()
				Expect(err).NotTo(HaveOccurred(), "a pause is a wait, not an error")
				if pausedCondition() != nil {
					Expect(res.RequeueAfter).To(Equal(freshRecheckDelay))
				}
			}
			Expect(pausedCondition()).NotTo(BeNil(), "the pause between the commit's writes was seen")
			Expect(writes).To(Equal([]string{opCreateBranch}), "the commit did not complete")
			base, err := git.BranchHead(ctx, mainBranch)
			Expect(err).NotTo(HaveOccurred())
			head, err := git.BranchHead(ctx, freshness.BranchName(fetch()))
			Expect(err).NotTo(HaveOccurred())
			Expect(head).To(Equal(base), "the branch was not moved after the pause")
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "the phase is kept")
			Expect(got.Status.Deadline).NotTo(BeNil(), "the deadline is kept")
			Expect(recorder.Events).To(Receive(Equal("Normal " + ReasonWritesPaused + " " + messageWritesPaused)))

			setPaused(false)
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			Expect(pausedCondition()).To(BeNil())
			Expect(writes).To(Equal([]string{opCreateBranch, opCommit, opOpen}), "each write once, none repeated")
			Expect(openRequests()).To(HaveLen(1))
			Expect(git.ChangeRequests()).To(HaveLen(1))
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
					if op == opList {
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

	// Issue #434: a merge was seen about four minutes late, because a waiting pull request was looked at only
	// with the full recheck, every five minutes. It is now looked at every poll interval in between.
	Context("while the pull request waits for review", func() {
		BeforeEach(func() {
			faulty = newFaultyGit(git)
		})

		// look runs one reconcile with r and returns its requeue, and how many pull request look-ups and
		// target branch head reads it made.
		look := func(r *RemediationRequestReconciler) (time.Duration, int, int) {
			finds, heads := faulty.count(opGet), faulty.count(opBranchHead)
			res, err := r.Reconcile(ctx, key())
			Expect(err).NotTo(HaveOccurred())
			return res.RequeueAfter, faulty.count(opGet) - finds, faulty.count(opBranchHead) - heads
		}

		It("looks at the pull request every poll interval and sees a merge within it", func() {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			// One reconciler from here on, as one executor process: it remembers its last full recheck.
			r := reconciler()
			wait, finds, heads := look(r)
			Expect(wait).To(Equal(DefaultPullRequestPollInterval), "the next look is after the poll interval")
			Expect(finds).To(Equal(1))
			Expect(heads).To(BeNumerically(">", 0), "the first look of a process is a full recheck")
			for i := range 4 {
				clock = clock.Add(DefaultPullRequestPollInterval)
				wait, finds, heads = look(r)
				Expect(wait).To(Equal(DefaultPullRequestPollInterval), "poll %d", i+1)
				Expect(finds).To(Equal(1), "poll %d looks at the pull request", i+1)
				Expect(heads).To(BeZero(), "poll %d reads nothing else from Git", i+1)
			}
			clock = clock.Add(DefaultPullRequestPollInterval)
			wait, _, heads = look(r)
			Expect(wait).To(Equal(DefaultPullRequestPollInterval))
			Expect(heads).To(BeNumerically(">", 0), "the plan gets its full recheck every five minutes")

			_, err := git.Merge(got.Status.PullRequest.Number)
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(DefaultPullRequestPollInterval)
			wait, finds, heads = look(r)
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged), "the merge is seen at the next poll")
			Expect(wait).To(BeZero())
			Expect(finds).To(Equal(1))
			Expect(heads).To(BeZero(), "a merge is recorded before any freshness read")
			_, kept := r.checked.Load(key().NamespacedName)
			Expect(kept).To(BeFalse(), "an ended request is not remembered")
		})

		It("checks the plan in full on the first look of a process and when the full recheck is due", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			r := reconciler()
			r.PullRequestPollInterval = 2 * time.Minute
			wait, _, _ := look(r)
			Expect(wait).To(Equal(2 * time.Minute))
			clock = clock.Add(4 * time.Minute)
			wait, _, heads := look(r)
			Expect(heads).To(BeZero())
			Expect(wait).To(Equal(time.Minute), "the poll before the full recheck ends when it is due")
			clock = clock.Add(time.Minute)
			_, _, heads = look(r)
			Expect(heads).To(BeNumerically(">", 0))
			_, _, heads = look(reconciler())
			Expect(heads).To(BeNumerically(">", 0), "a new process starts with a full recheck")
		})

		It("waits as long as the Git host asks when a look is rate limited, up to the full recheck's period", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			r := reconciler()
			look(r)
			clock = clock.Add(DefaultPullRequestPollInterval)
			faulty.failOn(opGet, &providers.RetryableError{RetryAfter: 3 * time.Minute, Err: errors.New("rate limited")})
			wait, _, heads := look(r)
			Expect(wait).To(Equal(3*time.Minute), "not the poll interval")
			Expect(heads).To(BeZero(), "the freshness check does not call the host again at once")
			faulty.failOn(opGet, &providers.RetryableError{RetryAfter: time.Hour, Err: errors.New("rate limited")})
			wait, _, _ = look(r)
			Expect(wait).To(Equal(FullRecheckInterval), "never longer than the full recheck's period")
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		})

		It("finishes a stale plan's cleanup at the next look instead of waiting for the full recheck", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			r := reconciler()
			look(r)
			// A cleanup that wrote its mark but lost the close (a restart, a deadline) leaves this status.
			got := fetch()
			meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
				Type: remediationv1alpha1.ConditionPlanFresh, Status: metav1.ConditionFalse,
				Reason: freshness.ReasonPolicyChanged, Message: "The policy changed",
			})
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
			clock = clock.Add(DefaultPullRequestPollInterval)
			wait, _, _ := look(r)
			Expect(wait).To(BeZero(), "not the poll interval")
			got = fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
			Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh).Reason).
				To(Equal(freshness.ReasonPolicyChanged))
			Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestClosed), "the close is done again")
		})

		Context("when writes were paused", func() {
			setPaused := func(on bool) {
				live := &remediationv1alpha1.RemediationSettings{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
				live.Spec.Paused = new(on)
				Expect(k8sClient.Update(ctx, live)).To(Succeed())
			}
			paused := func() *metav1.Condition {
				return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionWritesPaused)
			}
			// pauseBetweenFullRechecks reaches a waiting request whose last full recheck is recent and which
			// carries WritesPaused, as a pause between two full rechecks leaves it.
			pauseBetweenFullRechecks := func() *RemediationRequestReconciler {
				recorder = record.NewFakeRecorder(20)
				drive(remediationv1alpha1.PhaseAwaitingApproval)
				r := reconciler()
				look(r)
				setPaused(true)
				clock = clock.Add(DefaultPullRequestPollInterval)
				look(r)
				Expect(paused()).NotTo(BeNil())
				Expect(recorder.Events).To(Receive(Equal("Normal " + ReasonWritesPaused + " " + messageWritesPaused)))
				setPaused(false)
				clock = clock.Add(DefaultPullRequestPollInterval)
				return r
			}

			It("resumes at the first look after the pause lifts, not at the next full recheck", func() {
				r := pauseBetweenFullRechecks()
				look(r)
				Expect(paused()).To(BeNil())
				Expect(recorder.Events).To(Receive(Equal("Normal " + EventWritesResumed + " " + messageWritesResumed)))
			})

			It("resumes also when that look is rate limited", func() {
				r := pauseBetweenFullRechecks()
				faulty.failOn(opGet, &providers.RetryableError{RetryAfter: 3 * time.Minute, Err: errors.New("rate limited")})
				wait, _, heads := look(r)
				Expect(heads).To(BeNumerically(">", 0), "the full recheck runs")
				Expect(wait).To(Equal(DefaultPullRequestPollInterval))
				Expect(paused()).To(BeNil())
				Expect(recorder.Events).To(Receive(Equal("Normal " + EventWritesResumed + " " + messageWritesResumed)))
			})
		})

		It("gives a look that fails otherwise the full recheck", func() {
			drive(remediationv1alpha1.PhaseAwaitingApproval)
			r := reconciler()
			look(r)
			clock = clock.Add(DefaultPullRequestPollInterval)
			faulty.failOn(opGet, errors.New("connection reset"))
			wait, _, heads := look(r)
			Expect(heads).To(BeNumerically(">", 0), "the full recheck runs, as it did before the polls")
			Expect(wait).To(Equal(DefaultPullRequestPollInterval))
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		})
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
				Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval))
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
			file, err := git.ReadFile(ctx, crs[0].HeadSHA, sourcePath, providers.MaxFileBytes)
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
			faulty.beforeOp(opList, func() {
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
			// A flow mapping on the path: the freshness check still reads the previous limit, the patcher refuses.
			// (An anchor no longer serves: the freshness check refuses it first since #594.)
			_, err := git.Push(mainBranch, sourcePath, bytes.Replace(manifest("256Mi"),
				[]byte("limits:\n              memory: 256Mi\n"), []byte("limits: {memory: 256Mi}\n"), 1))
			Expect(err).NotTo(HaveOccurred())
			Expect(reconcileOnce()).To(Succeed())
			stoppedWaiting(ReasonPatchRefused)
		})

		It("closes a waiting pull request whose recorded head carries someone else's commit (#524)", func() {
			got := drive(remediationv1alpha1.PhaseAwaitingApproval)
			// A release before #524 recorded the head of a branch someone else had pushed to before the open.
			foreign, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
			got.Status.PullRequest.HeadSHA = foreign
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
			// An unrelated merge moves the target branch, so the change is rendered again.
			_, err = git.Push(mainBranch, "apps/other.yaml", []byte("a: 1\n"))
			Expect(err).NotTo(HaveOccurred())
			Expect(reconcileOnce()).To(Succeed())
			stoppedWaiting(ReasonBranchDiverged)
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
				// The cleanup closes only the pull requests with this request's marker, so the expiry does not
				// release the lock itself (#688): the read of the branch after it does (checkHeld).
				Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "the expiry does not release the lock on its own")
				Expect(reconcileOnce()).To(Succeed())
				Expect(fetch().Status.HoldsTarget).To(HaveValue(BeFalse()), "the read of the branch finds none open")
			})

			It("drops an old first look when the cleanup at the deadline finds its pull request on the branch (#688)", func() {
				loseStatusWrites()
				n := openRequests()[0].Number
				// An old look saw that number missing, long enough ago to confirm.
				seenGone(n)
				clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
				Expect(reconcileOnce()).To(Succeed())
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
				Expect(got.Status.PullRequest).To(BeNil(), "no status write recorded the pull request")
				Expect(lookedAt()).To(BeZero(), "the listing of the branch that found it dropped the first look")
				// One 404 for that number now only keeps a first look anew and waits.
				v := reconciler().changeOf(ctx, &lostChangeRequest{Git: git, n: n}, got, changeQuery{number: n})
				Expect(v.state).To(Equal(changeUnknown))
				Expect(v.err).To(MatchError(errGoneUnconfirmed))
			})

			It("says why it keeps the lock after the deadline while its repository is not in the settings (#688)", func() {
				recorder = record.NewFakeRecorder(20)
				loseStatusWrites()
				marked := openRequests()[0]
				setRepositories := func(repos []remediationv1alpha1.GitRepository) {
					GinkgoHelper()
					live := &remediationv1alpha1.RemediationSettings{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, live)).To(Succeed())
					live.Spec.Repositories = repos
					Expect(k8sClient.Update(ctx, live)).To(Succeed())
				}
				repos := settings.Spec.Repositories
				setRepositories(nil)
				clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
				Expect(reconcileOnce()).To(Succeed())
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
				Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "its pull request may be open")
				unreadable := func() *metav1.Condition {
					return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
				}
				c := unreadable()
				Expect(c).NotTo(BeNil(), "the status says why the lock stays")
				Expect(c.Status).To(Equal(metav1.ConditionTrue))
				Expect(c.Reason).To(Equal(reasonRepositoryUnavailable))
				since := c.LastTransitionTime
				Expect(recorder.Events).To(HaveLen(1))
				Expect(<-recorder.Events).To(HavePrefix(corev1.EventTypeWarning + " " + EventPullRequestUnreadable))
				// The later looks keep the lock and the condition, with no new Event.
				_, err := reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()))
				Expect(unreadable().LastTransitionTime).To(Equal(since))
				Expect(recorder.Events).To(BeEmpty())
				// The repository comes back with another target branch, then with an entry that cannot be used: the
				// condition says what is true now, with one Warning Event per reason.
				changed := slices.Clone(repos)
				changed[0].TargetBranch = "next-target"
				setRepositories(changed)
				_, err = reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()))
				Expect(unreadable().Reason).To(Equal(reasonTargetBranchChanged))
				Expect(recorder.Events).To(HaveLen(1))
				Expect(<-recorder.Events).To(HavePrefix(corev1.EventTypeWarning + " " + EventPullRequestUnreadable))
				setRepositories(repos)
				openErr = fmt.Errorf("bad key: %w", providers.ErrInvalidConfig)
				_, err = reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()))
				Expect(unreadable().Reason).To(Equal(string(sourcemap.ReasonRepositoryUnusable)))
				Expect(recorder.Events).To(HaveLen(1))
				Expect(<-recorder.Events).To(HavePrefix(corev1.EventTypeWarning + " " + EventPullRequestUnreadable))
				openErr = nil
				// Once the repository is configured again the look works: the condition goes, and the open pull
				// request of its own is recorded and keeps the lock.
				setRepositories(repos)
				_, err = reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				Expect(unreadable()).To(BeNil())
				got = fetch()
				Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
				Expect(got.Status.PullRequest.Number).To(Equal(marked.Number))
				Expect(recorder.Events).To(HaveLen(1))
				Expect(<-recorder.Events).To(HavePrefix(corev1.EventTypeNormal + " " + EventPullRequestReadable))
			})

			It("keeps the lock after the deadline while an unmarked pull request is open on the branch (#688)", func() {
				// Marked #1 is opened and its status write lost; a person closes it and opens #2, without the
				// marker, from the same branch. The deadline's cleanup closes only marked ones: #2 may still be
				// merged and deliver the change, so the target stays locked until it is closed or merged.
				loseStatusWrites()
				marked := openRequests()[0]
				Expect(git.CloseChangeRequest(ctx, marked.Number)).To(Succeed())
				other, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: marked.HeadBranch,
					BaseBranch: mainBranch, Title: "opened by a person", Body: unmarkedBody})
				Expect(err).NotTo(HaveOccurred())
				clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
				Expect(reconcileOnce()).To(Succeed())
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
				Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "the expiry does not release the lock")
				Expect(faulty.count(opClose)).To(BeZero(), "the unmarked one is not closed")
				_, err = reconcileResult()
				Expect(err).NotTo(HaveOccurred())
				got = fetch()
				Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()), "the unmarked open one keeps the lock")
				held := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)
				Expect(held).NotTo(BeNil())
				Expect(held.Message).To(ContainSubstring(fmt.Sprintf("#%d", other.Number)))
				_, reconcileSecond := second()
				expectLocked(reconcileSecond())
				Expect(git.CloseChangeRequest(ctx, other.Number)).To(Succeed())
				Expect(reconcileOnce()).To(Succeed())
				Expect(fetch().Status.HoldsTarget).To(HaveValue(BeFalse()), "none is open any more")
			})

			DescribeTable("expires with a clear message when the close after the deadline is refused",
				func(op string, class error, reason, what string) {
					loseStatusWrites()
					clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
					faulty.failOn(op, fmt.Errorf("status 403 from https://github.com/example/fleet: %w", class))
					res, err := reconcileResult()
					Expect(err).NotTo(HaveOccurred(), "a final refusal is not handed back for a retry")
					Expect(res).To(Equal(reconcile.Result{}))
					got := fetch()
					Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
					c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
					Expect(c.Reason).To(Equal(ReasonPullRequestDeadlineCleanupRefused))
					Expect(c.Message).To(ContainSubstring(reason))
					Expect(c.Message).To(ContainSubstring("close it by hand"))
					Expect(c.Message).To(ContainSubstring(what), "the message says what the host refused")
					Expect(c.Message).NotTo(ContainSubstring("https://"))
					Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen), "the refused close left it open")

					// The pull request it may have opened is not recorded, but the target stays locked (#376).
					Expect(got.Status.PullRequest).To(BeNil())
					Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
					_, reconcileSecond := second()
					expectLocked(reconcileSecond())
					// A look-up that fails keeps the lock and is tried again after the slow interval.
					faulty.failOn(opList, providers.ErrForbidden)
					res, err = reconcileResult()
					Expect(err).NotTo(HaveOccurred())
					Expect(res.RequeueAfter).To(Equal(heldRecheckInterval))
					Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()))
					expectLocked(reconcileSecond())
					// Once it can look, it finds the pull request by its branch and marker and records it.
					faulty.failOn("", nil)
					res, err = reconcileResult()
					Expect(err).NotTo(HaveOccurred())
					Expect(res.RequeueAfter).To(Equal(heldRecheckInterval))
					got = fetch()
					Expect(got.Status.PullRequest).NotTo(BeNil())
					Expect(got.Status.PullRequest.Number).To(Equal(openRequests()[0].Number))
					Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
					expectLocked(reconcileSecond())
					Expect(git.ChangeRequests()).To(HaveLen(1), "no second pull request while the first is open")
					// A person closes it: the next look-up releases the lock and the waiting request goes on.
					Expect(git.CloseChangeRequest(ctx, got.Status.PullRequest.Number)).To(Succeed())
					Expect(reconcileOnce()).To(Succeed())
					got = fetch()
					Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
					Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
						To(Equal(ReasonChangeRequestClosed))
					Expect(reconcileSecond().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
				},
				Entry("finding it (403)", opList, providers.ErrForbidden, ReasonGitForbidden, "to read the pull requests"),
				Entry("closing it (403)", opClose, providers.ErrForbidden, ReasonGitForbidden, "to close a pull request"),
				Entry("closing it (422)", opClose, providers.ErrRejected, ReasonGitRejected, "to close a pull request"),
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
				Entry("finding the pull request", opList, ""),
				Entry("reading the head of the pull request branch", opBranchHead, opList),
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
				// Not "finding the pull request": a refused read of the branch's pull requests is retried until the
				// deadline (#688); see the cells "adoption on unknown from a 403" and "expire without a recorded
				// pull request on unknown from a 403".
				{"reading the head of the pull request branch", opBranchHead, opList},
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
					Expect(cond.Message).NotTo(ContainSubstring("status 403"))

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
				file, err := git.ReadFile(ctx, got.Status.PullRequest.HeadSHA, sourcePath, providers.MaxFileBytes)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(file.Data)).To(ContainSubstring("memory: 384Mi"))
				Expect(string(file.Data)).To(ContainSubstring("memory: 320Mi"))
			})
		})
	})

	// Every caller that reads a pull request goes through changeOf (#688), and each gets every verdict here, in each
	// of the ways it arises, so a missing cell is a missing entry of the table. Each cell checks what the caller
	// did, not only the phase: which pull request it closed, the lock, the finalizer and how it asked to be called
	// again. Unknown never ends, releases or deletes anything.
	Context("for each caller and each verdict of changeOf (#688)", func() {
		// verdictCase is one way the Git host makes changeOf answer state.
		type verdictCase struct {
			state changeState
			how   string
		}
		var (
			isOpen         = verdictCase{changeOpen, ""}
			isMerged       = verdictCase{changeMerged, ""}
			isClosed       = verdictCase{changeClosed, ""}
			isGone         = verdictCase{changeGone, ""}
			goneOtherOpen  = verdictCase{changeGone, "another pull request open on the branch"}
			goneFirstLook  = verdictCase{changeUnknown, "a first look that finds it missing"}
			failedRead     = verdictCase{changeUnknown, "a server error"}
			unconfirmed404 = verdictCase{changeUnknown, "an unconfirmed 404"}
			contradicting  = verdictCase{changeUnknown, "contradicting reads"}
			forbidden      = verdictCase{changeUnknown, "a 403"}
			cases          = []verdictCase{isOpen, isMerged, isClosed, isGone, goneOtherOpen, goneFirstLook,
				failedRead, unconfirmed404, contradicting, forbidden}
		)
		name := func(c verdictCase) string {
			if c.how == "" {
				return c.state.String()
			}
			return c.state.String() + " from " + c.how
		}
		// openOther opens a pull request without the request's marker from its branch, as a person can.
		openOther := func() int64 {
			GinkgoHelper()
			other, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(fetch()),
				BaseBranch: mainBranch, Title: "opened by a person", Body: unmarkedBody})
			Expect(err).NotTo(HaveOccurred())
			return other.Number
		}
		// fault makes the reads of pull request n fail as c says; n is 0 for a request with none recorded, whose
		// read is the listing of the branch. It replaces faulty, which then counts the caller's calls only.
		fault := func(c verdictCase, n int64) {
			faulty = newFaultyGit(git)
			switch c {
			case failedRead:
				git.SetFail(func(op string) error {
					if op == opGet || op == opList {
						return &providers.RetryableError{Err: errors.New("server error")}
					}
					return nil
				})
			case forbidden:
				git.SetFail(func(op string) error {
					if op == opGet || op == opList {
						return fmt.Errorf("status 403: %w", providers.ErrForbidden)
					}
					return nil
				})
			case unconfirmed404:
				// The listing of the branch still has the number, so the 404 is not believed.
				op := opGet
				if n == 0 {
					op = opList
				}
				faulty.failOn(op, fmt.Errorf("status 404: %w", providers.ErrNotFound))
			case contradicting:
				// Read as closed by its number while the branch lists it open: a lagging read.
				faulty.stale[n] = providers.ChangeRequestClosed
			}
		}
		// recordedIn leaves a recorded pull request in the state c; it returns its number and that of another
		// pull request opened from the branch, if any.
		recordedIn := func(c verdictCase, n int64) (own, other int64) {
			GinkgoHelper()
			switch c {
			case isMerged:
				_, err := git.Merge(n)
				Expect(err).NotTo(HaveOccurred())
			case isClosed:
				Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
			case isGone, goneOtherOpen, goneFirstLook:
				// Closed, and the status records a number the host has not: a 404 the branch confirms. Gone needs
				// it seen missing once before by this process, at least confirmWait earlier (goneLooks). The closed one loses
				// the marker: a listing that shows a pull request of this request is never gone.
				Expect(git.CloseChangeRequest(ctx, n)).To(Succeed())
				Expect(git.SetBody(n, unmarkedBody)).To(Succeed())
				got := fetch()
				got.Status.PullRequest.Number = 9999
				Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
				if c != goneFirstLook {
					seenGone(9999)
				}
				if c == goneOtherOpen {
					other = openOther()
				}
			}
			fault(c, n)
			return n, other
		}
		recorded := func(c verdictCase) (int64, int64) {
			GinkgoHelper()
			return recordedIn(c, drive(remediationv1alpha1.PhaseAwaitingApproval).Status.PullRequest.Number)
		}
		// unrecorded leaves the request in PolicyChecked with a pull request of its own whose recording was lost,
		// in the state c; for gone there is none of its own.
		unrecorded := func(c verdictCase) (own, other int64) {
			GinkgoHelper()
			if c.state == changeGone {
				faulty = newFaultyGit(git)
				for faulty.count(opCommit) == 0 {
					Expect(reconcileOnce()).To(Succeed())
				}
				if c == goneOtherOpen {
					other = openOther()
				}
				fault(c, 0)
				return 0, other
			}
			lostOpen()
			own = openRequests()[0].Number
			switch c {
			case isMerged:
				_, err := git.Merge(own)
				Expect(err).NotTo(HaveOccurred())
			case isClosed:
				Expect(git.CloseChangeRequest(ctx, own)).To(Succeed())
			}
			fault(c, 0)
			return own, other
		}
		// outcome is what a caller did. closed names the pull requests it called the close on ("own", "another"; a
		// close of a merged one is refused by the host and retried after the watch records the merge), lock and
		// finalizer are "-" once the request is deleted, and result is how it asked to be called again: "requeue"
		// with a delay, "error" for the error backoff, or "done". cond is the reason of condition
		// PullRequestUnreadable ("" without it), warnings the PullRequestUnreadable Warning Events recorded, seen the
		// number of the first look this process keeps (goneLooks, 0 without it), class the class of a returned error ("" without one) and delay
		// the delay asked for.
		type outcome struct {
			phase, closed, lock, finalizer, result string
			cond                                   string
			warnings                               int
			seen                                   int64
			class                                  string
			delay                                  time.Duration
		}
		const (
			held, released, kept, removed, none = "held", "released", "kept", "removed", "none"
			requeue, failed, done, deleted      = "requeue", "error", "done", "deleted"
		)
		var (
			awaiting = string(remediationv1alpha1.PhaseAwaitingApproval)
			stopped  = string(remediationv1alpha1.PhaseNeedsIntervention)
			expired  = string(remediationv1alpha1.PhaseExpired)
			merged   = string(remediationv1alpha1.PhaseMerged)
			stale    = string(remediationv1alpha1.PhaseSuperseded)
			created  = string(remediationv1alpha1.PhasePRCreated)
			checked  = string(remediationv1alpha1.PhasePolicyChecked)
		)
		// classOf names the class of an error a caller returned.
		classOf := func(err error) string {
			switch {
			case errors.Is(err, errGoneUnconfirmed):
				return "gone-wait"
			case errors.Is(err, errUnconfirmedNotFound):
				return "unconfirmed-404"
			case errors.Is(err, errConflictingReads):
				return "contradicting"
			case providers.IsForbidden(err):
				return "refused-403"
			case providers.IsRetryable(err):
				return "retryable"
			case providers.IsConflict(err):
				return "host-conflict"
			case providers.IsNotFound(err):
				return "not-found"
			}
			return "other: " + err.Error()
		}
		observe := func(own, other int64, res reconcile.Result, err error) outcome {
			GinkgoHelper()
			git.SetFail(nil)
			var closed []string
			if faulty != nil {
				for _, n := range []int64{own, other} {
					if n != 0 && faulty.count(opClose+" "+strconv.FormatInt(n, 10)) > 0 {
						closed = append(closed, map[bool]string{true: "own", false: "another"}[n == own])
					}
				}
				Expect(faulty.count(opClose)).To(Equal(len(closed)), "no other pull request is closed")
			}
			o := outcome{phase: deleted, closed: none, lock: "-", finalizer: "-", result: done, delay: res.RequeueAfter}
			if len(closed) > 0 {
				o.closed = strings.Join(closed, ",")
			}
			switch {
			case err != nil && !errors.Is(err, errStopped) && !errors.Is(err, ErrSuperseded):
				o.result, o.class = failed, classOf(err)
			case res.RequeueAfter > 0:
				o.result = requeue
			}
			for len(recorder.Events) > 0 {
				if strings.HasPrefix(<-recorder.Events, corev1.EventTypeWarning+" "+EventPullRequestUnreadable+" ") {
					o.warnings++
				}
			}
			got := &remediationv1alpha1.RemediationRequest{}
			if apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got)) {
				return o
			}
			if c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable); c != nil {
				Expect(c.Status).To(Equal(metav1.ConditionTrue))
				Expect(c.Message).NotTo(ContainSubstring("status 403"), "no answer of the host in the message")
				o.cond = c.Reason
			}
			o.seen, _ = firstLook()
			o.phase, o.lock, o.finalizer = string(got.Status.Phase), released, removed
			if holdsLock(got) {
				o.lock = held
			}
			if controllerutil.ContainsFinalizer(got, FinalizerName) {
				o.finalizer = kept
			}
			return o
		}
		deleteOnce := func(own, other int64) outcome {
			GinkgoHelper()
			Expect(k8sClient.Delete(ctx, request)).To(Succeed())
			res, err := reconcileResult()
			return observe(own, other, res, err)
		}
		staleErr := &freshness.StaleError{Reason: freshness.ReasonTargetFileChanged, Message: "stale"}
		cleanUp := func(own, other int64) outcome {
			GinkgoHelper()
			return observe(own, other, reconcile.Result{}, reconciler().cleanUpStale(ctx, fetch(), staleErr))
		}
		// o builds an outcome: phase, closed, lock, finalizer, result; the other fields follow from the verdict
		// (expect).
		o := func(phase, closed, lock, finalizer, result string) outcome {
			return outcome{phase: phase, closed: closed, lock: lock, finalizer: finalizer, result: result}
		}
		na := outcome{phase: "n/a"}
		// delayOf is the delay a requeue asks for: a stopped holder looks again after heldRecheckInterval, a first look
		// that finds the number missing after the confirmation wait, and the watch after its poll interval.
		delayOf := func(vc verdictCase, base outcome) time.Duration {
			switch {
			case base.phase == stopped:
				return heldRecheckInterval
			case vc == goneFirstLook:
				return confirmWait
			}
			return reconciler().pullRequestPollInterval()
		}
		// expect completes the outcome base of a cell with what follows from the verdict case vc: the condition and
		// its one Warning Event for a refused read, the first look at a number seen missing (own is 0 without a recorded
		// number; recorded is false for a caller without one), the class of a returned error, and the delay of a requeue.
		expect := func(vc verdictCase, recorded bool, base outcome) outcome {
			if base.phase == deleted {
				return base
			}
			if vc == forbidden {
				base.cond, base.warnings = ReasonGitForbidden, 1
			}
			if recorded && vc == goneFirstLook {
				// The first look is kept while it waits; the look that confirms gone drops it.
				base.seen = 9999
			}
			if base.result == failed {
				base.class = map[verdictCase]string{failedRead: "retryable", forbidden: "refused-403",
					unconfirmed404: "unconfirmed-404", contradicting: "contradicting", goneFirstLook: "gone-wait",
					isMerged: "host-conflict"}[vc]
				if vc == unconfirmed404 && !recorded {
					// Without a recorded number the 404 is the listing's own, never an answer.
					base.class = "not-found"
				}
			}
			if base.result == requeue && base.delay == 0 {
				base.delay = delayOf(vc, base)
			}
			return base
		}
		// refusedOnDelete is the finalizer's cell for a read the Git host refuses for good (#288): held, CleanupRefused
		// set, and tried again after closeRefusedRetry until the grace ends.
		refusedOnDelete := func(phase string) outcome {
			out := o(phase, none, held, kept, requeue)
			out.delay = closeRefusedRetry
			return out
		}
		// A caller is setup, which leaves the request and the Git host in a verdict case, and act, which runs the
		// caller once and observes it.
		type caller struct {
			setup func(verdictCase) (own, other int64)
			act   func(own, other int64) outcome
			want  map[verdictCase]outcome
		}
		callers := map[string]caller{
			"watch": {
				setup: recorded,
				act: func(own, other int64) outcome {
					res, err := reconcileResult()
					return observe(own, other, res, err)
				},
				want: map[verdictCase]outcome{
					isOpen:         o(awaiting, none, held, kept, requeue),
					isMerged:       o(merged, none, released, kept, done),
					isClosed:       o(stopped, none, released, kept, done),
					isGone:         o(stopped, none, held, kept, done),
					goneOtherOpen:  o(stopped, none, held, kept, done),
					goneFirstLook:  o(awaiting, none, held, kept, requeue),
					failedRead:     o(awaiting, none, held, kept, requeue),
					unconfirmed404: o(awaiting, none, held, kept, requeue),
					contradicting:  o(awaiting, none, held, kept, requeue),
					forbidden:      o(awaiting, none, held, kept, requeue),
				},
			},
			"checkHeld": {
				setup: func(c verdictCase) (int64, int64) {
					return recordedIn(c, stopHeadMoved().Status.PullRequest.Number)
				},
				act: func(own, other int64) outcome {
					res, err := reconcileResult()
					return observe(own, other, res, err)
				},
				want: map[verdictCase]outcome{
					isOpen:         o(stopped, none, held, kept, requeue),
					isMerged:       o(stopped, none, released, kept, done),
					isClosed:       o(stopped, none, released, kept, done),
					isGone:         o(stopped, none, released, kept, done),
					goneOtherOpen:  o(stopped, none, held, kept, requeue),
					goneFirstLook:  o(stopped, none, held, kept, requeue),
					failedRead:     o(stopped, none, held, kept, requeue),
					unconfirmed404: o(stopped, none, held, kept, requeue),
					contradicting:  o(stopped, none, held, kept, requeue),
					forbidden:      o(stopped, none, held, kept, requeue),
				},
			},
			"finalize": {
				setup: recorded,
				act:   deleteOnce,
				want: map[verdictCase]outcome{
					isOpen:         o(deleted, "own", "-", "-", done),
					isMerged:       o(deleted, none, "-", "-", done),
					isClosed:       o(deleted, none, "-", "-", done),
					isGone:         o(deleted, none, "-", "-", done),
					goneOtherOpen:  o(deleted, none, "-", "-", done),
					goneFirstLook:  o(awaiting, none, held, kept, requeue),
					failedRead:     o(awaiting, none, held, kept, failed),
					unconfirmed404: o(awaiting, none, held, kept, failed),
					contradicting:  o(awaiting, none, held, kept, failed),
					// A refused read starts the grace of the deletion (#288).
					forbidden: refusedOnDelete(awaiting),
				},
			},
			"cleanUpStale": {
				// A recorded merge is the watch's to record, which runs first: the close is retried meanwhile.
				setup: recorded,
				act:   cleanUp,
				want: map[verdictCase]outcome{
					isOpen:         o(stale, "own", held, kept, done),
					isMerged:       o(awaiting, "own", held, kept, failed),
					isClosed:       o(stale, "own", held, kept, done),
					isGone:         o(stopped, none, held, kept, done),
					goneOtherOpen:  o(stopped, none, held, kept, done),
					goneFirstLook:  o(awaiting, none, held, kept, failed),
					failedRead:     o(awaiting, none, held, kept, failed),
					unconfirmed404: o(awaiting, none, held, kept, failed),
					contradicting:  o(awaiting, none, held, kept, failed),
					// A refused read is retried with the mark kept, never an end (#199).
					forbidden: o(awaiting, none, held, kept, failed),
				},
			},
			"expire": {
				setup: recorded,
				act: func(own, other int64) outcome {
					res, err := reconciler().expire(ctx, fetch())
					return observe(own, other, res, err)
				},
				want: map[verdictCase]outcome{
					isOpen:         o(expired, "own", held, kept, done),
					isMerged:       o(awaiting, "own", held, kept, failed),
					isClosed:       o(expired, "own", held, kept, done),
					isGone:         o(expired, none, held, kept, done),
					goneOtherOpen:  o(expired, none, held, kept, done),
					goneFirstLook:  o(awaiting, none, held, kept, requeue),
					failedRead:     o(awaiting, none, held, kept, failed),
					unconfirmed404: o(awaiting, none, held, kept, failed),
					contradicting:  o(awaiting, none, held, kept, failed),
					forbidden:      o(expired, none, held, kept, done),
				},
			},
			"refuseWaiting": {
				setup: recorded,
				act: func(own, other int64) outcome {
					err := reconciler().refuseWaiting(ctx, fetch(), &renderRefusal{reason: ReasonBranchDiverged, message: "m"})
					return observe(own, other, reconcile.Result{}, err)
				},
				want: map[verdictCase]outcome{
					isOpen:         o(stopped, "own", held, kept, done),
					isMerged:       o(awaiting, "own", held, kept, failed),
					isClosed:       o(stopped, "own", held, kept, done),
					isGone:         o(stopped, none, held, kept, done),
					goneOtherOpen:  o(stopped, none, held, kept, done),
					goneFirstLook:  o(awaiting, none, held, kept, failed),
					failedRead:     o(awaiting, none, held, kept, failed),
					unconfirmed404: o(awaiting, none, held, kept, failed),
					contradicting:  o(awaiting, none, held, kept, failed),
					forbidden:      o(awaiting, none, held, kept, failed),
				},
			},
			"adoption": {
				setup: unrecorded,
				act: func(own, other int64) outcome {
					res, err := reconcileResult()
					return observe(own, other, res, err)
				},
				want: map[verdictCase]outcome{
					isOpen:         o(created, none, held, kept, done),
					isMerged:       o(merged, none, released, kept, done),
					isClosed:       o(stopped, none, held, kept, done),
					isGone:         o(created, none, held, kept, done),
					goneOtherOpen:  o(stopped, none, held, kept, done),
					goneFirstLook:  na,
					failedRead:     o(checked, none, held, kept, failed),
					unconfirmed404: o(checked, none, held, kept, failed),
					contradicting:  na,
					forbidden:      o(checked, none, held, kept, failed),
				},
			},
			"expire without a recorded pull request": {
				setup: func(c verdictCase) (int64, int64) {
					own, other := unrecorded(c)
					clock = fetch().Status.Deadline.Add(time.Minute)
					return own, other
				},
				act: func(own, other int64) outcome {
					res, err := reconcileResult()
					return observe(own, other, res, err)
				},
				want: map[verdictCase]outcome{
					isOpen:         o(expired, "own", held, kept, done),
					isMerged:       o(merged, none, released, kept, done),
					isClosed:       o(expired, none, held, kept, done),
					isGone:         o(expired, none, held, kept, done),
					goneOtherOpen:  o(expired, none, held, kept, done),
					goneFirstLook:  na,
					failedRead:     o(checked, none, held, kept, failed),
					unconfirmed404: o(checked, none, held, kept, failed),
					contradicting:  na,
					forbidden:      o(expired, none, held, kept, done),
				},
			},
			"cleanUpStale without a recorded pull request": {
				setup: unrecorded,
				act:   cleanUp,
				want: map[verdictCase]outcome{
					isOpen:         o(stale, "own", held, kept, done),
					isMerged:       o(merged, none, released, kept, done),
					isClosed:       o(stale, none, held, kept, done),
					isGone:         o(stale, none, held, kept, done),
					goneOtherOpen:  o(stale, none, held, kept, done),
					goneFirstLook:  na,
					failedRead:     o(checked, none, held, kept, failed),
					unconfirmed404: o(checked, none, held, kept, failed),
					contradicting:  na,
					// A refused read is retried with the mark kept, never an end (#199).
					forbidden: o(checked, none, held, kept, failed),
				},
			},
			"finalize without a recorded pull request": {
				setup: unrecorded,
				act:   deleteOnce,
				want: map[verdictCase]outcome{
					isOpen:         o(deleted, "own", "-", "-", done),
					isMerged:       o(deleted, none, "-", "-", done),
					isClosed:       o(deleted, none, "-", "-", done),
					isGone:         o(deleted, none, "-", "-", done),
					goneOtherOpen:  o(deleted, none, "-", "-", done),
					goneFirstLook:  na,
					failedRead:     o(checked, none, held, kept, failed),
					unconfirmed404: o(checked, none, held, kept, failed),
					contradicting:  na,
					// A refused listing starts the grace of the deletion (#288).
					forbidden: refusedOnDelete(checked),
				},
			},
		}
		entries := make([]TableEntry, 0, len(callers)*len(cases))
		for _, caller := range slices.Sorted(maps.Keys(callers)) {
			for i, c := range cases {
				if callers[caller].want[c] == na {
					// Without a recorded number the listing of the branch is the one read: there is nothing for it to
					// contradict, and no number to see missing (no pull request of its own on the branch is gone at
					// once, as before any was opened).
					continue
				}
				entries = append(entries, Entry(caller+" on "+name(c), caller, i))
			}
		}
		It("has a cell for every caller and every way to each verdict", func() {
			states := map[changeState]bool{}
			for _, c := range cases {
				states[c.state] = true
			}
			Expect(states).To(HaveLen(len(changeStates)))
			for caller, c := range callers {
				Expect(c.want).To(HaveLen(len(cases)), caller)
			}
		})
		DescribeTable("answers each verdict as its rule says", func(name string, i int) {
			c, vc := callers[name], cases[i]
			want, ok := c.want[vc]
			Expect(ok).To(BeTrue(), "no expected result for %s on %s", name, vc.how)
			recorder = record.NewFakeRecorder(50)
			own, other := c.setup(vc)
			// A caller without a recorded number has no cell for a first look that finds it missing.
			recorded := c.want[goneFirstLook] != na
			Expect(c.act(own, other)).To(Equal(expect(vc, recorded, want)))
			if vc == goneFirstLook {
				// The first look only waits; once the wait is over the same answer is the confirmed end, as when the
				// number was seen missing before (isGone).
				clock = clock.Add(confirmWait)
				Expect(c.act(own, other)).To(Equal(expect(isGone, recorded, c.want[isGone])))
			}
		}, entries)
	})

})

// patchGone answers every patch with NotFound, as for a request that went away between the read and the patch.
type patchGone struct{ client.Client }

func (p *patchGone) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: remediationv1alpha1.GroupVersion.Group,
		Resource: "remediationrequests"}, "gone")
}

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

// unmarkedBody is the body of a pull request a person opened from a request's branch, without its marker.
const unmarkedBody = "no marker"

// lostChangeRequest is a Git host that has lost change request n: GetChangeRequest answers 404 for it, and the
// listing of its branch leaves it out.
type lostChangeRequest struct {
	*fakegit.Git
	n int64
}

func (l *lostChangeRequest) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	if n == l.n {
		return providers.ChangeRequest{}, fmt.Errorf("pull request %d: %w", n, providers.ErrNotFound)
	}
	return l.Git.GetChangeRequest(ctx, n)
}

func (l *lostChangeRequest) ListChangeRequests(ctx context.Context, branch string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := l.Git.ListChangeRequests(ctx, branch, openOnly)
	return slices.DeleteFunc(all, func(cr providers.ChangeRequest) bool { return cr.Number == l.n }), err
}

// pausedLost is lostChangeRequest that runs pause once, on the first listing of the branch in all states: after
// the read by number answered 404 and before the call settles, as a call that another worker overtakes.
type pausedLost struct {
	lostChangeRequest
	pause  func()
	paused bool
}

func (p *pausedLost) ListChangeRequests(ctx context.Context, branch string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	if !openOnly && !p.paused {
		p.paused = true
		p.pause()
	}
	return p.lostChangeRequest.ListChangeRequests(ctx, branch, openOnly)
}

// startOtherWorker runs read in another goroutine, as another worker of the controller, and returns its verdict
// once it is done, without waiting for it.
func startOtherWorker(read func() changeVerdict) <-chan changeVerdict {
	done := make(chan changeVerdict, 1)
	go func() {
		defer GinkgoRecover()
		done <- read()
	}()
	return done
}

// pausedOpen is a Git host that runs pause once, on the first listing of the open pull requests of the branch:
// after the read by number and before the call settles.
type pausedOpen struct {
	*fakegit.Git
	pause  func()
	paused bool
}

func (p *pausedOpen) ListChangeRequests(ctx context.Context, branch string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	if openOnly && !p.paused {
		p.paused = true
		p.pause()
	}
	return p.Git.ListChangeRequests(ctx, branch, openOnly)
}

// countedReads counts the reads of pull requests that go through it.
type countedReads struct {
	providers.GitProvider
	reads *atomic.Int32
}

func (c *countedReads) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	c.reads.Add(1)
	return c.GitProvider.GetChangeRequest(ctx, n)
}

func (c *countedReads) ListChangeRequests(ctx context.Context, branch string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	c.reads.Add(1)
	return c.GitProvider.ListChangeRequests(ctx, branch, openOnly)
}

// openedLater is the title of a newer pull request a test opens on the branch.
const openedLater = "opened later"

// missedInAll is lostChangeRequest whose listing of the branch in all states also misses pull request missed,
// which its open listing still has, as a listing that lags may.
type missedInAll struct {
	lostChangeRequest
	missed int64
}

func (m *missedInAll) ListChangeRequests(ctx context.Context, branch string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := m.lostChangeRequest.ListChangeRequests(ctx, branch, openOnly)
	if openOnly {
		return all, err
	}
	return slices.DeleteFunc(all, func(cr providers.ChangeRequest) bool { return cr.Number == m.missed }), err
}

// countingReader counts the reads of a RemediationRequest through it.
type countingReader struct {
	client.Reader
	requests atomic.Int64
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.RemediationRequest); ok {
		c.requests.Add(1)
	}
	return c.Reader.Get(ctx, key, obj, opts...)
}

// laggingList is a Git host whose listing of the open pull requests of a branch still shows pull request n open
// after it was closed, as a listing that lags behind the read by number may.
type laggingList struct {
	*fakegit.Git
	n int64
}

func (l *laggingList) ListChangeRequests(ctx context.Context, branch string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := l.Git.ListChangeRequests(ctx, branch, openOnly)
	if err != nil || !openOnly || slices.ContainsFunc(all, func(cr providers.ChangeRequest) bool { return cr.Number == l.n }) {
		return all, err
	}
	cr, err := l.GetChangeRequest(ctx, l.n)
	if err != nil {
		return nil, err
	}
	cr.State = providers.ChangeRequestOpen
	return append([]providers.ChangeRequest{cr}, all...), nil
}

// Operation names of faultyGit. A ReadFile fault or count may name the path too: "ReadFile <path>".
const (
	opFind         = "FindChangeRequest"
	opGet          = "GetChangeRequest"
	opList         = "ListChangeRequests"
	opCreateBranch = "CreateBranch"
	opReadFile     = "ReadFile"
	opListDir      = "ListDir"
	opCheckMerged  = "CheckMerged"
	opComment      = "CommentChangeRequest"
	opCheckBranch  = "CheckBranch"
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
	// stale, per number, is the state GetChangeRequest answers instead of the host's, as a lagging read.
	stale map[int64]providers.ChangeRequestState
}

func newFaultyGit(g *fakegit.Git) *faultyGit {
	return &faultyGit{Git: g, calls: map[string]int{}, before: map[string]func(){},
		stale: map[int64]providers.ChangeRequestState{}}
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

func (f *faultyGit) ReadFile(ctx context.Context, sha, path string, limit int) (providers.File, error) {
	if err := f.enter(opReadFile, path); err != nil {
		return providers.File{}, err
	}
	file, err := f.Git.ReadFile(ctx, sha, path, limit)
	return file, f.leave(ctx, opReadFile, err)
}

func (f *faultyGit) ListDir(ctx context.Context, sha, path string) ([]providers.DirEntry, error) {
	if err := f.enter(opListDir, path); err != nil {
		return nil, err
	}
	entries, err := f.Git.ListDir(ctx, sha, path)
	return entries, f.leave(ctx, opListDir, err)
}

func (f *faultyGit) BranchHead(ctx context.Context, branch string) (string, error) {
	if err := f.enter(opBranchHead, branch); err != nil {
		return "", err
	}
	sha, err := f.Git.BranchHead(ctx, branch)
	return sha, f.leave(ctx, opBranchHead, err)
}

func (f *faultyGit) CheckBranch(ctx context.Context, sha, path string) (providers.BranchState, error) {
	if err := f.enter(opCheckBranch, sha); err != nil {
		return 0, err
	}
	state, err := f.Git.CheckBranch(ctx, sha, path)
	return state, f.leave(ctx, opCheckBranch, err)
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

func (f *faultyGit) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	if err := f.enter(opGet, strconv.FormatInt(n, 10)); err != nil {
		return providers.ChangeRequest{}, err
	}
	cr, err := f.Git.GetChangeRequest(ctx, n)
	if err = f.leave(ctx, opGet, err); err != nil {
		return providers.ChangeRequest{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.stale[n]; ok {
		cr.State = st
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

func (f *faultyGit) ListChangeRequests(ctx context.Context, branch string, openOnly bool) ([]providers.ChangeRequest, error) {
	if err := f.enter(opList, branch); err != nil {
		return nil, err
	}
	crs, err := f.Git.ListChangeRequests(ctx, branch, openOnly)
	if err = f.leave(ctx, opList, err); err != nil {
		return nil, err
	}
	return crs, nil
}

func (f *faultyGit) CloseChangeRequest(ctx context.Context, n int64) error {
	if err := f.enter(opClose, strconv.FormatInt(n, 10)); err != nil {
		return err
	}
	return f.leave(ctx, opClose, f.Git.CloseChangeRequest(ctx, n))
}

func (f *faultyGit) CheckMerged(ctx context.Context, head, merge, path string) (providers.BranchState, string, error) {
	if err := f.enter(opCheckMerged, head); err != nil {
		return 0, "", err
	}
	state, parent, err := f.Git.CheckMerged(ctx, head, merge, path)
	return state, parent, f.leave(ctx, opCheckMerged, err)
}

func (f *faultyGit) CommentChangeRequest(ctx context.Context, n int64, marker, body string) error {
	if err := f.enter(opComment, ""); err != nil {
		return err
	}
	return f.leave(ctx, opComment, f.Git.CommentChangeRequest(ctx, n, marker, body))
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
	It("shows the size of the step beside the policy, from the request's numbers (ADR-0026)", func() {
		rr := bodyRequest()
		m := rr.Spec.Action.SetContainerMemoryLimit
		m.Previous, m.Desired = resource.MustParse("48Mi"), resource.MustParse("64Mi")
		body := pullRequestBody(rr, "", nil)
		Expect(body).To(ContainSubstring("**Size:** an increase of 34% of the previous value, within policy `" +
			string(rr.Spec.PolicyRef) + "`."))
		Expect(body).To(ContainSubstring("The diagnosis, with the model's unverified reason for the size, is in the " +
			"analysis record and the analyzer's log for this request's name, not in this pull request"))
		m.Previous, m.Desired = resource.MustParse("100Mi"), resource.MustParse("80Mi")
		Expect(pullRequestBody(rr, "", nil)).To(ContainSubstring("**Size:** a decrease of 20% of the previous value"))
		k := remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: 2, DesiredValue: 3}
		rr.Spec.Action = remediationv1alpha1.Action{Type: remediationv1alpha1.ActionAdjustKedaMaxReplicas,
			AdjustKedaMaxReplicas: &k}
		Expect(stepSize(rr)).To(Equal("an increase of 50% of the previous value"))
		k.PreviousValue = 0
		Expect(stepSize(rr)).To(BeEmpty())
	})

	It("carries no secret and keeps the marker exact", func() {
		rr := bodyRequest()
		// Names are validated by the schema; the body is redacted anyway, so a token that slipped through a
		// future field never reaches the Git host.
		rr.Spec.IncidentRef.Name = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
		body := pullRequestBody(rr, "", nil)
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
		hexToken := "3f1c9a7be2d84f06a5b1c3e9d7f2a4b6c8e0d1f3" // #nosec G101 -- made-up hex run, not a credential
		rr.Status.Source.Path = remediationv1alpha1.GitFilePath("apps/" + hexToken + "/deployment.yaml")
		body := pullRequestBody(rr, name, nil)
		Expect(body).To(ContainSubstring("for incident `" + name + "`."))
		Expect(body).To(ContainSubstring("incident `" + name + "`, policy `p/1`"))
		Expect(body).NotTo(ContainSubstring(hexToken), "a hex token in other text is still redacted")
		Expect(body).To(ContainSubstring("[REDACTED:token]"))
		Expect(hasMarker(body, rr)).To(BeTrue())
	})

	It("names a request called by the derivation of its own spec verbatim and still redacts free text (#433)", func() {
		rr := bodyRequest()
		// The analyzer names a request "remediation-" plus 24 hex digits of a hash of its spec, a run the
		// redactor would otherwise take for a token, so a reviewer could not find the request from the pull
		// request.
		name, err := remediationv1alpha1.RequestName(rr.Spec)
		Expect(err).NotTo(HaveOccurred())
		rr.Name = string(name)
		hexToken := "3f1c9a7be2d84f06a5b1c3e9d7f2a4b6c8e0d1f3" // #nosec G101 -- made-up hex run, not a credential
		rr.Status.Source.Path = remediationv1alpha1.GitFilePath("apps/" + hexToken + "/deployment.yaml")
		body := pullRequestBody(rr, "", nil)
		Expect(body).To(ContainSubstring("RemediationRequest `" + requestNamespace + "/" + rr.Name + "` (UID"))
		Expect(body).NotTo(ContainSubstring(hexToken), "a hex token in other text is still redacted")
		Expect(body).To(ContainSubstring("[REDACTED:token]"))
		Expect(hasMarker(body, rr)).To(BeTrue())
	})

	It("redacts a request name that is not the derivation of its spec or sits in a token-like namespace", func() {
		hex40 := strings.Repeat("3f1c9a7b", 5)
		secret24 := "3f1c9a7be2d84f06a5b1c3e9" // #nosec G101 -- made-up hex run, not a credential
		derived, err := remediationv1alpha1.RequestName(bodyRequest().Spec)
		Expect(err).NotTo(HaveOccurred())
		otherPlan := bodyRequest()
		otherPlan.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("512Mi")
		for _, tc := range []struct {
			namespace, name, secret string
			spec                    *remediationv1alpha1.RemediationRequest
		}{
			{requestNamespace, hex40, hex40, nil},
			{requestNamespace, "remediation-" + hex40, hex40, nil},
			// The attack: a chosen hex secret in the analyzer's shape, not the hash of the spec.
			{requestNamespace, "remediation-" + secret24, secret24, nil},
			// The derivation of another plan is not this request's name.
			{requestNamespace, string(derived), strings.TrimPrefix(string(derived), "remediation-"), otherPlan},
			{hex40, string(derived), hex40, nil},
		} {
			rr := bodyRequest()
			if tc.spec != nil {
				rr = tc.spec
			}
			rr.Namespace, rr.Name = tc.namespace, tc.name
			body := pullRequestBody(rr, "", nil)
			Expect(body).NotTo(ContainSubstring(tc.secret), tc.namespace+"/"+tc.name)
			Expect(body).To(ContainSubstring("RemediationRequest `[REDACTED:token]` (UID"), tc.namespace+"/"+tc.name)
			Expect(hasMarker(body, rr)).To(BeTrue())
		}
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
			body := pullRequestBody(rr, "", nil)
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

// switchedRequestReader is a reader whose get of the RemediationRequest called name fails with a server error
// while fail is set; every other read goes to Reader.
type switchedRequestReader struct {
	client.Reader
	name string
	fail *atomic.Bool
}

func (f switchedRequestReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.RemediationRequest); ok && key.Name == f.name && f.fail.Load() {
		return apierrors.NewInternalError(errors.New("etcd is not answering"))
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// openListedOnly is a Git host that answers 404 for pull request n and leaves it out of the listing of its branch
// in all states, while the open listing still shows it, as reads that lag behind each other may.
type openListedOnly struct {
	*fakegit.Git
	n int64
}

func (o *openListedOnly) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	if n == o.n {
		return providers.ChangeRequest{}, fmt.Errorf("pull request %d: %w", n, providers.ErrNotFound)
	}
	return o.Git.GetChangeRequest(ctx, n)
}

func (o *openListedOnly) ListChangeRequests(ctx context.Context, branch string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := o.Git.ListChangeRequests(ctx, branch, openOnly)
	if openOnly {
		return all, err
	}
	return slices.DeleteFunc(all, func(cr providers.ChangeRequest) bool { return cr.Number == o.n }), err
}
