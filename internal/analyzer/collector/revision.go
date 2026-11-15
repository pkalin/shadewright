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

package collector

import (
	"context"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// revisionAnnotation is the revision the deployment controller writes on a Deployment and on each of its
// ReplicaSets: the Deployment's is the revision of its newest ReplicaSet, the one of its current template.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// templateCheckTTL is how long what one check read about a pod's ReplicaSet and Deployment, or about its
// StatefulSet, is reused, per object UID. It is short, as ownerMissTTL, so a rollout is noticed within it,
// and it spares a read per observation of the pods of one ReplicaSet or StatefulSet.
const templateCheckTTL = ownerMissTTL

var (
	// replacedTemplateDropped counts pod evidence the collector did not turn into an observation because the
	// pod belongs to a template its workload has retired (#724): one per container termination, symptom state
	// or event. Its one label is the workload kind, Deployment or StatefulSet, a closed set.
	replacedTemplateDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shadewright_analyzer_replaced_template_evidence_dropped_total",
		Help: "Pod evidence not turned into an observation because its workload has retired the pod's template.",
	}, []string{labelKind})
	// replacedTemplateCheckErrors counts checks that kept pod evidence because a read that decides whether the
	// pod's template was retired failed (a timeout, throttling, a refused read). Label as above.
	replacedTemplateCheckErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shadewright_analyzer_replaced_template_check_errors_total",
		Help: "Pod evidence kept because the workload state that decides whether its template was retired could not be read.",
	}, []string{labelKind})
)

func init() {
	metrics.Registry.MustRegister(replacedTemplateDropped, replacedTemplateCheckErrors)
}

// subjectVerdict is what resolving the subject of an object decided. The zero value drops the evidence
// uncounted, the safe default.
type subjectVerdict uint8

const (
	// subjectUnusable: the object or its subject does not fit the Incident schema.
	subjectUnusable subjectVerdict = iota
	// subjectUsable: the subject is the one returned.
	subjectUsable
	// subjectRetired: the object is a pod of a template its workload, the subject returned, has retired. The
	// caller records the evidence as handled without an observation and counts it (dropRetired).
	subjectRetired
)

// verdict turns the ok of a resolution into a subjectVerdict.
func verdict(ok bool) subjectVerdict {
	if ok {
		return subjectUsable
	}
	return subjectUnusable
}

// dropRetired counts and logs n pieces of evidence (what names them) dropped because their pod's template was
// retired by its workload of kind.
func dropRetired(ctx context.Context, kind, what string, n int) {
	replacedTemplateDropped.WithLabelValues(kind).Add(float64(n))
	logf.FromContext(ctx).V(1).Info("Dropped evidence of a pod whose template its workload has retired",
		"kind", kind, "evidence", what, "count", n)
}

// retiredTemplate reports whether the pod, whose controller reference is podRef, whose labels are podLabels and
// which is terminating (a deletion timestamp, or gone) when deleting is set, belongs to a template that its
// verified workload owner has retired: one that no longer runs once this pod is gone. Old pods that still
// serve, during a paused or stuck rollout, are not retired. It is decided from the verified chain and fresh
// reads with a deadline each (cached for templateCheckTTL), never from the owner cache, whose facts may
// predate the rollout:
//
//   - Deployment: the pod's ReplicaSet, still controlled by owner, is scaled to 0 replicas and has a revision
//     annotation lower than the Deployment's, read while the Deployment's status.observedGeneration equals its
//     generation. The deployment controller sets the Deployment's revision to that of its newest ReplicaSet,
//     and a rollback gives the revived ReplicaSet a new, higher revision only once it has observed the
//     rollback; until then the generations differ and the evidence is kept. Revisions are compared, not
//     templates: a template compared field by field can differ only through defaults an API server upgrade
//     added. A ReplicaSet that is not found while the pod is terminating (revisionHistoryLimit 0 deletes it
//     first) is retired too.
//   - StatefulSet: the pod is terminating and its controller-revision-hash label differs from
//     status.updateRevision, read while status.observedGeneration equals the generation, with an update
//     strategy that replaces every pod (RollingUpdate with no partition). A pod that waits its turn is not
//     terminating, so its evidence stays, and so does that of a running pod whose label was edited.
//
// Anything that leaves the answer open keeps the evidence, as before: a failed or refused read (counted in
// replacedTemplateCheckErrors), an object gone or with another UID, a missing or unparsable revision, a status
// behind its spec. The read errors are not returned: the owner chain was just verified, and dropping real
// evidence is worse than keeping evidence of a retired template.
func (r *ownerResolver) retiredTemplate(ctx context.Context, owner remediationv1alpha1.ObjectRef,
	podRef metav1.OwnerReference, podLabels labels.Set, deleting bool) bool {
	var (
		retired bool
		why     string
		err     error
	)
	switch {
	case owner.Kind == kindDeployment && podRef.Kind == kindReplicaSet:
		retired, why, err = r.retiredReplicaSet(ctx, owner, podRef, deleting)
	case owner.Kind == kindStatefulSet && podRef.Kind == kindStatefulSet && types.UID(owner.UID) == podRef.UID:
		retired, why, err = r.retiredStatefulSetRevision(ctx, owner, podLabels, deleting)
	default:
		// A bare ReplicaSet, or a chain that stopped below the workload: nothing to compare with.
		return false
	}
	log := logf.FromContext(ctx).V(1)
	switch {
	case err != nil:
		replacedTemplateCheckErrors.WithLabelValues(owner.Kind).Inc()
		log.Info("Kept pod evidence, the state of its workload could not be read", "kind", owner.Kind,
			"error", err.Error())
	case why != "":
		log.Info("Kept pod evidence, the state of its workload leaves its template open", "kind", owner.Kind,
			"cause", why)
	}
	return err == nil && retired
}

// replicaSetState is what one check read about a pod's ReplicaSet and its Deployment.
type replicaSetState struct {
	// gone is true when the ReplicaSet was not found.
	gone bool
	// retired is true when the ReplicaSet is scaled to 0 and its Deployment has moved past its revision.
	retired bool
	// why names the cause when the answer is open.
	why string
}

// retiredReplicaSet is retiredTemplate for a Deployment.
func (r *ownerResolver) retiredReplicaSet(ctx context.Context, owner remediationv1alpha1.ObjectRef,
	podRef metav1.OwnerReference, deleting bool) (bool, string, error) {
	key := ownerKey{kind: kindReplicaSet, namespace: owner.Namespace, name: podRef.Name, uid: podRef.UID}
	var st replicaSetState
	if v, ok := r.templates.Get(key); ok {
		st = v.(replicaSetState)
	} else {
		var err error
		if st, err = r.readReplicaSetState(ctx, owner, podRef); err != nil {
			return false, "", err
		}
		r.templates.Add(key, st, templateCheckTTL)
	}
	if st.gone {
		if deleting {
			return true, "", nil
		}
		return false, "ReplicaSet not found, pod not terminating", nil
	}
	return st.retired, st.why, nil
}

// readReplicaSetState reads the pod's ReplicaSet and, only when it is scaled to 0, its Deployment.
func (r *ownerResolver) readReplicaSetState(ctx context.Context, owner remediationv1alpha1.ObjectRef,
	podRef metav1.OwnerReference) (replicaSetState, error) {
	var rs appsv1.ReplicaSet
	if err := r.getFresh(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: podRef.Name}, &rs); err != nil {
		if apierrors.IsNotFound(err) {
			return replicaSetState{gone: true}, nil
		}
		return replicaSetState{}, err
	}
	if rs.UID != podRef.UID {
		return replicaSetState{why: "ReplicaSet replaced"}, nil
	}
	if p := metav1.GetControllerOfNoCopy(&rs); p == nil || p.Kind != kindDeployment || p.Name != owner.Name ||
		p.UID != types.UID(owner.UID) {
		return replicaSetState{why: "ReplicaSet no longer controlled by the Deployment"}, nil
	}
	if rs.Spec.Replicas == nil || *rs.Spec.Replicas != 0 {
		// Still serving: current, or old during a paused or stuck rollout. No second read.
		return replicaSetState{}, nil
	}
	var d appsv1.Deployment
	if err := r.getFresh(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &d); err != nil {
		if apierrors.IsNotFound(err) {
			return replicaSetState{why: "Deployment not found"}, nil
		}
		return replicaSetState{}, err
	}
	switch {
	case d.UID != types.UID(owner.UID):
		return replicaSetState{why: "Deployment replaced"}, nil
	case d.Status.ObservedGeneration != d.Generation:
		return replicaSetState{why: "Deployment status behind its spec"}, nil
	}
	rsRev, ok1 := revision(rs.Annotations)
	dRev, ok2 := revision(d.Annotations)
	if !ok1 || !ok2 {
		return replicaSetState{why: "revision annotation missing or not a number"}, nil
	}
	return replicaSetState{retired: rsRev < dRev}, nil
}

// statefulSetState is what one check read about a StatefulSet: its update revision when it moves every pod
// to it and its status is current, else why it is not known.
type statefulSetState struct {
	updateRevision string
	why            string
}

// retiredStatefulSetRevision is retiredTemplate for a StatefulSet. The pod's own facts are checked first, so a
// pod that is not terminating costs no read.
func (r *ownerResolver) retiredStatefulSetRevision(ctx context.Context, owner remediationv1alpha1.ObjectRef,
	podLabels labels.Set, deleting bool) (bool, string, error) {
	if !deleting {
		return false, "", nil
	}
	podRev := podLabels[appsv1.ControllerRevisionHashLabelKey]
	if podRev == "" {
		return false, "pod has no revision label", nil
	}
	key := ownerKey{kind: kindStatefulSet, namespace: owner.Namespace, name: owner.Name, uid: types.UID(owner.UID)}
	var st statefulSetState
	if v, ok := r.templates.Get(key); ok {
		st = v.(statefulSetState)
	} else {
		var err error
		if st, err = r.readStatefulSetState(ctx, owner); err != nil {
			return false, "", err
		}
		r.templates.Add(key, st, templateCheckTTL)
	}
	if st.why != "" {
		return false, st.why, nil
	}
	return podRev != st.updateRevision, "", nil
}

// readStatefulSetState reads the StatefulSet.
func (r *ownerResolver) readStatefulSetState(ctx context.Context, owner remediationv1alpha1.ObjectRef) (
	statefulSetState, error) {
	var sts appsv1.StatefulSet
	if err := r.getFresh(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return statefulSetState{why: "StatefulSet not found"}, nil
		}
		return statefulSetState{}, err
	}
	switch {
	case sts.UID != types.UID(owner.UID):
		return statefulSetState{why: "StatefulSet replaced"}, nil
	case sts.Status.ObservedGeneration != sts.Generation || sts.Status.UpdateRevision == "":
		return statefulSetState{why: "StatefulSet status behind its spec"}, nil
	case !replacesEveryPod(sts.Spec.UpdateStrategy):
		return statefulSetState{why: "update strategy keeps older pods"}, nil
	}
	return statefulSetState{updateRevision: sts.Status.UpdateRevision}, nil
}

// getFresh is one get through the API reader with a deadline of its own.
func (r *ownerResolver) getFresh(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.api.Get(ctx, key, obj)
}

// replacesEveryPod reports whether a StatefulSet's update strategy moves every pod to the update revision:
// RollingUpdate (the default) without a partition.
func replacesEveryPod(s appsv1.StatefulSetUpdateStrategy) bool {
	if s.Type != "" && s.Type != appsv1.RollingUpdateStatefulSetStrategyType {
		return false
	}
	return s.RollingUpdate == nil || s.RollingUpdate.Partition == nil || *s.RollingUpdate.Partition == 0
}

// revision parses the deployment controller's revision annotation, a positive decimal number.
func revision(annotations map[string]string) (int64, bool) {
	v, err := strconv.ParseInt(annotations[revisionAnnotation], 10, 64)
	return v, err == nil && v > 0
}
