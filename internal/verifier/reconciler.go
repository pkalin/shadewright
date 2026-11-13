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

package verifier

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/ownership"
	"github.com/pkalin/shadewright/internal/verification/clearing"
)

// Reconciler samples the pods of every RemediationRequest in Verifying and writes the request's
// VerificationReport. It holds no state of its own between reconciles: the window, the baselines and the
// sample counter live in the report, so a restart or a lost lease forgets nothing (ADR-0030 section 4).
type Reconciler struct {
	// Client reads RemediationRequests from the manager's cache (operator namespace only) and makes the
	// verifier's two writes: the create of a report and the update of its status.
	Client client.Client
	// APIReader reads reports, targets and ReplicaSets one at a time and lists pods, never through a cache:
	// the verifier holds get on those kinds and list on pods, and no watch on any of them.
	APIReader client.Reader
	// Namespace is the operator namespace, the only namespace whose requests are sampled.
	Namespace string
	// Watched are the namespaces of --watch-namespaces. A request whose target is elsewhere is not sampled,
	// since the verifier holds no grant there: it gets no report, which the executor reads as no evidence.
	Watched []string
	// Now is the verifier's clock. Nil uses time.Now.
	Now func() time.Time
	// ReadTimeout bounds each call to the API server: every read and write gets a context with this deadline,
	// so one hung call cannot hold the single worker past the gap threshold. Zero uses DefaultReadTimeout.
	ReadTimeout time.Duration
}

const (
	// DefaultReadTimeout is the deadline of one call to the API server.
	DefaultReadTimeout = 10 * time.Second
	// MaxObservation bounds one sample on the verifier's clock, from the start of its reads (the target, then
	// the pod list) to the moment
	// the status write is about to be sent. A sample that took longer is discarded: nothing is written and the
	// counter does not rise, since pods listed that long ago are not evidence of the time the sample would
	// carry. It is a third of clearing.GapAfter, so a discarded sample and its retry fit before a gap.
	MaxObservation = 20 * time.Second
)

// skipReason says why a reconcile took no sample. It is a fixed string, never text read from the cluster.
type skipReason string

const (
	// skipNotServed: the request is not in Verifying, or is a revert. Nothing to do and nothing logged.
	skipNotServed skipReason = "NotServed"
	// skipAwaitingInputs: Verifying, but the merge receipt or the rollout's template revision is not recorded
	// yet. The executor writes them in a later status write; the request is looked at again every poll.
	skipAwaitingInputs skipReason = "AwaitingInputs"
	// skipKindNotSampled: the target is not an apps/v1 Deployment or StatefulSet (a ScaledObject has no pods
	// of its own, ADR-0030 section 3).
	skipKindNotSampled skipReason = "TargetKindNotSampled"
	// skipNamespaceNotWatched: the target's namespace is not in --watch-namespaces, where the verifier holds
	// no grant.
	skipNamespaceNotWatched skipReason = "NamespaceNotWatched"
	// skipTargetUIDUnknown: no report yet, the target does not exist and the request names no target UID, so
	// there is nothing to bind a report to.
	skipTargetUIDUnknown skipReason = "TargetUIDUnknown"
	// skipReportNotBound: a report of the request's name exists but is bound to another request or rollout.
	skipReportNotBound skipReason = "ReportNotBound"
	// skipObservationTooLong: the reads of the sample took longer than MaxObservation.
	skipObservationTooLong skipReason = "ObservationTooLong"
)

// SetupWithManager registers the reconciler. It watches RemediationRequests only and runs again on a change of
// what decides whether a request is sampled; while a request is Verifying it requeues itself every poll interval.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("verifier").
		For(&v1alpha1.RemediationRequest{}, builder.WithPredicates(inputsChanged())).
		Complete(r)
}

// inputsChanged lets through creates and the updates that change an input of sampled: the phase, the merge
// receipt, or the rollout and its template revision. Other status writes of the executor are dropped: the
// sampling cadence comes from RequeueAfter, so a busy request is not sampled more often.
func inputsChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*v1alpha1.RemediationRequest)
			n, ok2 := e.ObjectNew.(*v1alpha1.RemediationRequest)
			if !ok1 || !ok2 {
				return true
			}
			return o.Status.Phase != n.Status.Phase ||
				!equality.Semantic.DeepEqual(o.Status.MergeReceipt, n.Status.MergeReceipt) ||
				(o.Status.Rollout == nil) != (n.Status.Rollout == nil) ||
				templateRevision(o) != templateRevision(n)
		},
		DeleteFunc: func(event.DeleteEvent) bool { return false },
	}
}

// templateRevision is status.rollout.templateRevision, empty without a rollout.
func templateRevision(rr *v1alpha1.RemediationRequest) string {
	if rr.Status.Rollout == nil {
		return ""
	}
	return rr.Status.Rollout.TemplateRevision
}

// call runs one API call with the read timeout.
func (r *Reconciler) call(ctx context.Context, f func(context.Context) error) error {
	d := r.ReadTimeout
	if d <= 0 {
		d = DefaultReadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx)
}

// now reads the verifier's clock.
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// CacheOptions restricts the verifier's cache to RemediationRequests in the operator namespace, the only kind
// it watches. Nothing else is cached: every other read goes through the API reader.
func CacheOptions(namespace string) cache.Options {
	return cache.Options{
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&v1alpha1.RemediationRequest{}: {Namespaces: map[string]cache.Config{namespace: {}}},
		},
	}
}

// Reconcile takes one sample for a request in Verifying and writes it into the request's report, creating the
// report with its first sample. A failed read is returned as an error and retried: it is not a sample, and
// nothing is written. Every reconcile that takes no sample for a request in Verifying logs why, with a fixed
// reason.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx).WithValues("request", req.String())
	rr := &v1alpha1.RemediationRequest{}
	if err := r.call(ctx, func(ctx context.Context) error { return r.Client.Get(ctx, req.NamespacedName, rr) }); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading the RemediationRequest: %w", err)
	}
	t, why := r.sampled(rr)
	switch why {
	case "":
	case skipNotServed:
		return ctrl.Result{}, nil
	case skipAwaitingInputs:
		// The executor records the receipt and the template revision after it sets Verifying; look again.
		log.V(1).Info("Took no sample of a RemediationRequest yet", "reason", string(why))
		return ctrl.Result{RequeueAfter: clearing.PollInterval}, nil
	default:
		// Neither the request's target nor --watch-namespaces changes while the verifier runs: no requeue.
		log.Info("Took no sample of a RemediationRequest", "reason", string(why))
		return ctrl.Result{}, nil
	}
	why, err := r.sample(ctx, rr, t)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case why == skipReportNotBound:
		// The report's spec is immutable, so it never becomes this request's: no time-based retry. A change of
		// the request's receipt or rollout runs the reconcile again through the predicate. The executor's trust
		// contract reads that report as ReportMismatch.
		log.Error(errNotBound, "Took no sample of a RemediationRequest", "reason", string(why),
			"verificationReport", clearing.ReportName(rr.UID))
		return ctrl.Result{}, nil
	case why != "":
		log.Info("Took no sample of a RemediationRequest", "reason", string(why))
	}
	return ctrl.Result{RequeueAfter: clearing.PollInterval}, nil
}

// target is what the verifier needs of the request's target to sample it.
type target struct {
	kind, namespace, name string
	// uid is spec.targetRef.uid, empty when the request has none.
	uid types.UID
}

// sampled returns the request's target when the verifier samples it: in Verifying and not being deleted, with a merge receipt and a
// recorded template revision, a Deployment or StatefulSet target in a watched namespace. Otherwise it returns
// why not. A revert is never sampled (ADR-0030 section 8); see isRevert.
func (r *Reconciler) sampled(rr *v1alpha1.RemediationRequest) (target, skipReason) {
	st := &rr.Status
	if st.Phase != v1alpha1.PhaseVerifying || !rr.DeletionTimestamp.IsZero() || isRevert(rr) {
		// A request being deleted gets no report and no further sample, whatever holds its finalizer.
		return target{}, skipNotServed
	}
	if st.MergeReceipt == nil || templateRevision(rr) == "" {
		return target{}, skipAwaitingInputs
	}
	ref := rr.Spec.TargetRef
	if ref.APIVersion != appsv1.SchemeGroupVersion.String() || (ref.Kind != ownership.KindDeployment && ref.Kind != ownership.KindStatefulSet) {
		// A ScaledObject has no pods of its own: clearing is not checked for it (ADR-0030 section 3).
		return target{}, skipKindNotSampled
	}
	if !slices.Contains(r.Watched, ref.Namespace) {
		return target{}, skipNamespaceNotWatched
	}
	return target{kind: ref.Kind, namespace: ref.Namespace, name: ref.Name, uid: types.UID(ref.UID)}, ""
}

// errNotBound is logged for a report that exists under the request's report name but whose spec is not the
// one this request and rollout give.
var errNotBound = errors.New("the VerificationReport is bound to another request or rollout")

// sample reads the report, the target, its pods and their ReplicaSets, takes the time, applies one sample and
// writes it. The report is created only together with its first sample, after every read of that sample
// succeeded, so a read that keeps failing leaves no report rather than one with an empty status: the
// executor reads a missing report as StaleReport and ends the request Inconclusive at the timeout. The one
// way to a report without a sample is a create that itself takes the observation past MaxObservation (or a
// status write that fails after it): the sample is dropped, the next reconcile takes a new one, and until
// then the executor reads the report, which has no sampledAt, as stale like a missing one. It returns a skip
// reason when it wrote nothing for a cause that is not an error.
func (r *Reconciler) sample(ctx context.Context, rr *v1alpha1.RemediationRequest, t target) (skipReason, error) {
	report := &v1alpha1.VerificationReport{}
	key := client.ObjectKey{Namespace: r.Namespace, Name: clearing.ReportName(rr.UID)}
	exists := true
	if err := r.call(ctx, func(ctx context.Context) error { return r.APIReader.Get(ctx, key, report) }); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("reading the VerificationReport: %w", err)
		}
		exists = false
	}

	// The observation starts with the read of the target: everything the sample covers is read after this.
	start := r.now()
	obj, err := r.readTarget(ctx, t)
	if err != nil {
		return "", err
	}

	if exists {
		if !boundTo(report, rr) {
			return skipReportNotBound, nil
		}
	} else {
		spec, ok := reportSpec(rr, t, obj)
		if !ok {
			// No target UID to bind the report to yet: no report, so no evidence. Look again at the next poll.
			return skipTargetUIDUnknown, nil
		}
		report = &v1alpha1.VerificationReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: r.Namespace,
				Name:      key.Name,
				// The report is deleted with its request. blockOwnerDeletion false needs no permission on the
				// request beyond the reads the verifier holds.
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         v1alpha1.GroupVersion.String(),
					Kind:               "RemediationRequest",
					Name:               rr.Name,
					UID:                rr.UID,
					Controller:         new(true),
					BlockOwnerDeletion: new(false),
				}},
			},
			Spec: spec,
		}
	}

	in := clearing.Input{MemoryLimit: memoryLimit(rr)}
	if obj != nil {
		in.Target = obj
		pods, truncated, err := r.listPods(ctx, obj)
		if err != nil {
			return "", err
		}
		in.Pods, in.Truncated = pods, truncated
		rss, err := r.readReplicaSets(ctx, t.namespace, clearing.ReplicaSetNames(report.Spec, in))
		if err != nil {
			return "", err
		}
		in.ReplicaSets = rss
	}
	// The sample time is taken after every read returned, so sampledAt never predates what was observed.
	in.Now = r.now()
	if in.Now.Sub(start) > MaxObservation {
		return skipObservationTooLong, nil
	}
	res, err := clearing.Sample(report.Spec, report.Status, in)
	if err != nil {
		return "", fmt.Errorf("sampling the pods of the rollout: %w", err)
	}

	if !exists {
		if err := r.call(ctx, func(ctx context.Context) error { return r.Client.Create(ctx, report) }); err != nil {
			return "", fmt.Errorf("creating the VerificationReport: %w", err)
		}
	}
	if r.now().Sub(start) > MaxObservation {
		// The create took the observation past its bound. The report stays without a sample; the next
		// reconcile samples it again.
		return skipObservationTooLong, nil
	}
	report.Status = res.Status
	if err := r.call(ctx, func(ctx context.Context) error { return r.Client.Status().Update(ctx, report) }); err != nil {
		return "", fmt.Errorf("writing the VerificationReport status: %w", err)
	}
	logf.FromContext(ctx).V(1).Info("Wrote a VerificationReport sample", "sample", res.Status.Sample,
		"reset", string(res.Reason), "podsCovered", res.Status.PodsCovered, "oomKilled", res.OOMKilled)
	return "", nil
}

// isRevert reports whether the request is a revert of a failed request (spec.reverts, #96), which the verifier
// never samples. The field does not exist yet, so no request is a revert; #96 adds it and this check.
func isRevert(*v1alpha1.RemediationRequest) bool {
	return false
}

// readTarget reads the target with a get. A target that does not exist is nil, which the sample reads as no
// evidence; any other failure is an error.
func (r *Reconciler) readTarget(ctx context.Context, t target) (client.Object, error) {
	var obj client.Object
	switch t.kind {
	case ownership.KindDeployment:
		obj = &appsv1.Deployment{}
	case ownership.KindStatefulSet:
		obj = &appsv1.StatefulSet{}
	default:
		return nil, fmt.Errorf("target kind %q is not sampled", t.kind)
	}
	key := client.ObjectKey{Namespace: t.namespace, Name: t.name}
	if err := r.call(ctx, func(ctx context.Context) error { return r.APIReader.Get(ctx, key, obj) }); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the target %s: %w", t.kind, err)
	}
	return obj, nil
}

// reportSpec builds the report's spec from the request: its UID, the receipt's merge commit, the template
// revision of status.rollout, the action's container, and the target's UID: spec.targetRef.uid when the request
// names one, else the UID of the target read now. ok is false when neither is known.
func reportSpec(rr *v1alpha1.RemediationRequest, t target, obj client.Object) (v1alpha1.VerificationReportSpec, bool) {
	uid := t.uid
	if uid == "" && obj != nil {
		uid = obj.GetUID()
	}
	if uid == "" {
		return v1alpha1.VerificationReportSpec{}, false
	}
	spec := v1alpha1.VerificationReportSpec{
		RequestUID:       v1alpha1.UID(rr.UID),
		MergeCommitSHA:   rr.Status.MergeReceipt.MergeCommitSHA,
		TargetUID:        v1alpha1.UID(uid),
		TemplateRevision: rr.Status.Rollout.TemplateRevision,
	}
	if a := rr.Spec.Action.SetContainerMemoryLimit; a != nil {
		spec.Container = a.Container
	}
	return spec, true
}

// boundTo reports whether an existing report is the one this request and rollout give: the same request UID,
// merge commit, template revision and container, and the request's target UID when it names one. A report
// that is not is never written into.
func boundTo(report *v1alpha1.VerificationReport, rr *v1alpha1.RemediationRequest) bool {
	s := report.Spec
	container := ""
	if a := rr.Spec.Action.SetContainerMemoryLimit; a != nil {
		container = a.Container
	}
	return types.UID(s.RequestUID) == rr.UID &&
		s.MergeCommitSHA == rr.Status.MergeReceipt.MergeCommitSHA &&
		s.TemplateRevision == rr.Status.Rollout.TemplateRevision &&
		s.Container == container &&
		(rr.Spec.TargetRef.UID == "" || s.TargetUID == rr.Spec.TargetRef.UID)
}

// memoryLimit is the desired memory limit of the action's container, nil for an action that names none.
func memoryLimit(rr *v1alpha1.RemediationRequest) *resource.Quantity {
	if a := rr.Spec.Action.SetContainerMemoryLimit; a != nil {
		q := a.Desired.DeepCopy()
		return &q
	}
	return nil
}

// readReplicaSets reads the named ReplicaSets, one get each in the target's namespace, before the sample time
// is taken, and returns a reader over what it read. A ReplicaSet that does not exist is nil; any other failure
// is an error, and the sample is not taken. A name that was not read is an error too, never a missing owner.
func (r *Reconciler) readReplicaSets(ctx context.Context, namespace string, names []string) (clearing.ReplicaSetGetter, error) {
	read := make(map[string]*appsv1.ReplicaSet, len(names))
	for _, name := range names {
		rs := &appsv1.ReplicaSet{}
		err := r.call(ctx, func(ctx context.Context) error {
			return r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, rs)
		})
		switch {
		case apierrors.IsNotFound(err):
			read[name] = nil
		case err != nil:
			return nil, fmt.Errorf("reading the ReplicaSet of a Pod: %w", err)
		default:
			read[name] = rs
		}
	}
	return func(name string) (*appsv1.ReplicaSet, error) {
		rs, ok := read[name]
		if !ok {
			return nil, errReplicaSetNotRead
		}
		return rs, nil
	}, nil
}

// errReplicaSetNotRead is returned when the sample asks for a ReplicaSet that was not read before it.
var errReplicaSetNotRead = errors.New("the sample asked for a ReplicaSet that was not read before it")

// listPods lists the pods the target's selector matches in its namespace, in pages of clearing.PageSize and
// at most clearing.MaxPages pages. truncated is true when more pods remain. Each pod is trimmed to the fields
// ADR-0030 section 7 names before anything else sees it.
func (r *Reconciler) listPods(ctx context.Context, obj client.Object) ([]corev1.Pod, bool, error) {
	var sel *metav1.LabelSelector
	switch t := obj.(type) {
	case *appsv1.Deployment:
		sel = t.Spec.Selector
	case *appsv1.StatefulSet:
		sel = t.Spec.Selector
	}
	selector, ok := ownership.Selector(sel)
	if !ok {
		// A target without a usable selector matches no pod of its own: an empty list, which the sample reads
		// as fewer pods than desired. It is never listed with an empty selector, which would match every pod.
		return nil, false, nil
	}
	var (
		pods  []corev1.Pod
		token string
	)
	for range clearing.MaxPages {
		list := &corev1.PodList{}
		opts := []client.ListOption{client.InNamespace(obj.GetNamespace()),
			client.MatchingLabelsSelector{Selector: selector}, client.Limit(clearing.PageSize)}
		if token != "" {
			opts = append(opts, client.Continue(token))
		}
		if err := r.call(ctx, func(ctx context.Context) error { return r.APIReader.List(ctx, list, opts...) }); err != nil {
			return nil, false, fmt.Errorf("listing the pods of the target: %w", err)
		}
		for i := range list.Items {
			pods = append(pods, Trim(&list.Items[i]))
		}
		token = list.Continue
		if token == "" {
			return pods, false, nil
		}
	}
	return pods, true, nil
}

// Trim keeps only the pod fields ADR-0030 section 7 names: UID, namespace, owner references, labels, the Ready
// and PodScheduled conditions (type and status), creation and deletion times, container names (and the
// restart policy that makes an init container a native sidecar), the containers' memory limits, restart
// counts, and the reason and finish time of the current and last termination. Every other field is dropped,
// the termination message included, so nothing else can be stored, logged or put into an Event.
func Trim(p *corev1.Pod) corev1.Pod {
	out := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		UID:               p.UID,
		Namespace:         p.Namespace,
		Labels:            p.Labels,
		OwnerReferences:   p.OwnerReferences,
		CreationTimestamp: p.CreationTimestamp,
		DeletionTimestamp: p.DeletionTimestamp,
	}}
	for _, c := range p.Spec.InitContainers {
		out.Spec.InitContainers = append(out.Spec.InitContainers, corev1.Container{Name: c.Name, RestartPolicy: c.RestartPolicy})
	}
	for _, c := range p.Spec.Containers {
		tc := corev1.Container{Name: c.Name}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			tc.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: q}
		}
		out.Spec.Containers = append(out.Spec.Containers, tc)
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady || c.Type == corev1.PodScheduled {
			out.Status.Conditions = append(out.Status.Conditions, corev1.PodCondition{Type: c.Type, Status: c.Status})
		}
	}
	out.Status.InitContainerStatuses = trimStatuses(p.Status.InitContainerStatuses)
	out.Status.ContainerStatuses = trimStatuses(p.Status.ContainerStatuses)
	return out
}

func trimStatuses(in []corev1.ContainerStatus) []corev1.ContainerStatus {
	if in == nil {
		return nil
	}
	out := make([]corev1.ContainerStatus, 0, len(in))
	for _, s := range in {
		out = append(out, corev1.ContainerStatus{
			Name:                 s.Name,
			RestartCount:         s.RestartCount,
			State:                corev1.ContainerState{Terminated: trimTermination(s.State.Terminated)},
			LastTerminationState: corev1.ContainerState{Terminated: trimTermination(s.LastTerminationState.Terminated)},
		})
	}
	return out
}

// trimTermination keeps the reason and the finish time of a termination. The reason is only ever compared with
// the constant OOMKilled; the message is never read.
func trimTermination(t *corev1.ContainerStateTerminated) *corev1.ContainerStateTerminated {
	if t == nil {
		return nil
	}
	return &corev1.ContainerStateTerminated{Reason: t.Reason, FinishedAt: t.FinishedAt}
}
