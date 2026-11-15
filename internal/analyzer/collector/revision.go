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

// replacedTemplateDropped counts pod evidence the collector did not turn into an observation because the pod
// belongs to a template its workload has replaced (#724). Its one label is the workload kind, Deployment or
// StatefulSet, a closed set.
var replacedTemplateDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_replaced_template_evidence_dropped_total",
	Help: "Pod evidence not turned into an observation because the pod's template is no longer its workload's current one.",
}, []string{labelKind})

func init() {
	metrics.Registry.MustRegister(replacedTemplateDropped)
}

// replacedTemplate reports whether the pod, whose controller reference is podRef and whose labels are
// podLabels, belongs to a template that its verified workload owner has replaced. It is true only when that
// is established from objects read now, never from the owner cache, whose facts may predate the rollout:
//
//   - Deployment: the pod's ReplicaSet, still controlled by owner, has a revision annotation lower than the
//     Deployment's. The deployment controller sets the Deployment's revision to that of its newest
//     ReplicaSet, and a rollback gives the revived ReplicaSet a new, higher revision, so a lower one is a
//     template the Deployment moved away from. Revisions are compared, not templates: a template compared
//     field by field can differ only through defaults an API server upgrade added, which would drop real
//     evidence, and while the controller has not yet caught up the revisions say nothing is replaced.
//   - StatefulSet: the pod's controller-revision-hash label differs from status.updateRevision, read while
//     status.observedGeneration equals the generation, and the update strategy replaces every pod
//     (RollingUpdate with no partition). With OnDelete or a partition, pods of the older revision are kept
//     on purpose and their evidence is real.
//
// Anything that leaves the answer open is false, so the evidence is kept as before: a failed or refused read,
// an object gone or with another UID, a missing or unparsable revision, a StatefulSet whose status is behind
// its spec. The read errors are not returned: the owner chain was just verified, and dropping real evidence
// silently is worse than keeping evidence of a replaced template.
func (r *ownerResolver) replacedTemplate(ctx context.Context, owner remediationv1alpha1.ObjectRef,
	podRef metav1.OwnerReference, podLabels labels.Set) bool {
	var (
		replaced bool
		why      string
		err      error
	)
	switch {
	case owner.Kind == kindDeployment && podRef.Kind == kindReplicaSet:
		replaced, why, err = r.replacedReplicaSet(ctx, owner, podRef)
	case owner.Kind == kindStatefulSet && podRef.Kind == kindStatefulSet && types.UID(owner.UID) == podRef.UID:
		replaced, why, err = r.replacedStatefulSetRevision(ctx, owner, podLabels)
	default:
		// A bare ReplicaSet, or a chain that stopped below the workload: nothing to compare with.
		return false
	}
	log := logf.FromContext(ctx).V(1)
	switch {
	case err != nil:
		log.Info("Kept pod evidence, the current revision of its workload could not be read",
			"kind", owner.Kind, "error", err.Error())
	case replaced:
		replacedTemplateDropped.WithLabelValues(owner.Kind).Inc()
		log.Info("Dropped pod evidence, the pod's template is no longer its workload's current one", "kind", owner.Kind)
	case why != "":
		log.Info("Kept pod evidence, the current revision of its workload is not known", "kind", owner.Kind,
			"cause", why)
	}
	return replaced
}

// replacedReplicaSet is replacedTemplate for a Deployment. why names the cause when the answer is open.
func (r *ownerResolver) replacedReplicaSet(ctx context.Context, owner remediationv1alpha1.ObjectRef,
	podRef metav1.OwnerReference) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var rs appsv1.ReplicaSet
	if err := r.api.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: podRef.Name}, &rs); err != nil {
		return false, "", err
	}
	if rs.UID != podRef.UID {
		return false, "ReplicaSet replaced", nil
	}
	if p := metav1.GetControllerOfNoCopy(&rs); p == nil || p.Kind != kindDeployment || p.Name != owner.Name ||
		p.UID != types.UID(owner.UID) {
		return false, "ReplicaSet no longer controlled by the Deployment", nil
	}
	var d appsv1.Deployment
	if err := r.api.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &d); err != nil {
		return false, "", err
	}
	if d.UID != types.UID(owner.UID) {
		return false, "Deployment replaced", nil
	}
	rsRev, ok1 := revision(rs.Annotations)
	dRev, ok2 := revision(d.Annotations)
	if !ok1 || !ok2 {
		return false, "revision annotation missing or not a number", nil
	}
	return rsRev < dRev, "", nil
}

// replacedStatefulSetRevision is replacedTemplate for a StatefulSet. why names the cause when the answer is
// open.
func (r *ownerResolver) replacedStatefulSetRevision(ctx context.Context, owner remediationv1alpha1.ObjectRef,
	podLabels labels.Set) (bool, string, error) {
	podRev := podLabels[appsv1.ControllerRevisionHashLabelKey]
	if podRev == "" {
		return false, "pod has no revision label", nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var sts appsv1.StatefulSet
	if err := r.api.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &sts); err != nil {
		return false, "", err
	}
	switch {
	case sts.UID != types.UID(owner.UID):
		return false, "StatefulSet replaced", nil
	case sts.Status.ObservedGeneration != sts.Generation || sts.Status.UpdateRevision == "":
		return false, "StatefulSet status behind its spec", nil
	case !replacesEveryPod(sts.Spec.UpdateStrategy):
		return false, "update strategy keeps older pods", nil
	}
	return podRev != sts.Status.UpdateRevision, "", nil
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
