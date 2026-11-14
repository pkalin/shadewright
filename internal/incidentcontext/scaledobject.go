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

package incidentcontext

import (
	"context"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/pkalin/shadewright/internal/kedaready"
)

// scaledObjectConditions are the conditions of a ScaledObject the status section shows, in this order
// (ADR-0036). HPAActive is left out: it is read by the readiness rule, not shown.
var scaledObjectConditions = []string{kedaready.ConditionReady, kedaready.ConditionActive,
	kedaready.ConditionFallback, kedaready.ConditionPaused}

// resolveScaledObject reads a keda.sh/v1alpha1 ScaledObject subject with one get from the API server, the
// object the KEDA module's role lets the analyzer get (ADR-0036 decision 2). It looks up nothing else: not the
// scale target, not the HPA, no list. A ScaledObject has no pods of its own here.
func (b *Builder) resolveScaledObject(ctx context.Context, t *target) error {
	so := &unstructured.Unstructured{}
	so.SetAPIVersion(kedaready.APIVersion)
	so.SetKind(kedaready.KindScaledObject)
	found, err := b.get(ctx, b.objects, t.ref.Namespace, t.ref.Name, types.UID(t.ref.UID), so, kedaready.KindScaledObject)
	if err != nil {
		return err
	}
	if !found {
		t.state = targetGone
		return nil
	}
	t.scaledObject = so
	return nil
}

// scaledObjectOf returns a deep copy of the ScaledObject a resolved target holds, as the API server returned
// it, or nil ([Context.ScaledObject]).
func scaledObjectOf(t *target) map[string]any {
	if t.state != targetResolved || t.scaledObject == nil {
		return nil
	}
	return runtime.DeepCopyJSON(t.scaledObject.Object)
}

// scaledObjectLines describes a ScaledObject from the allow-list of ADR-0036 decision 3: its name, its replica
// bounds, its scale target's kind and name, the conditions Ready, Active, Fallback and Paused, the metric
// names KEDA reports, and the type of each trigger. Nothing else is read: not a trigger's metadata (a server
// address, a query), not its authentication reference, not the annotations or labels. Every string is a
// value ([Builder.value]): redacted, then left out whole when it is over the bound, never cut. Numbers are
// written by code and are not quotable, like the replica counts of a workload.
func (b *Builder) scaledObjectLines(l *lines, so *unstructured.Unstructured) {
	l.add("ScaledObject %s/%s", l.data(b.value(so.GetNamespace())), l.data(b.value(so.GetName())))
	spec, _ := so.Object["spec"].(map[string]any)
	l.add("  spec: minReplicaCount %s, maxReplicaCount %s", replicaCount(spec, "minReplicaCount"),
		replicaCount(spec, "maxReplicaCount"))
	target, _ := spec["scaleTargetRef"].(map[string]any)
	kind, _ := target["kind"].(string)
	name, _ := target["name"].(string)
	switch {
	case kind == "" && name == "":
		l.add("  scale target: not set")
	case kind == "":
		l.add("  scale target: %s (kind not set: KEDA's default is a Deployment)", l.data(b.value(name)))
	default:
		l.add("  scale target: %s %s", l.data(b.value(kind)), l.data(b.value(name)))
	}
	triggers, _ := spec["triggers"].([]any)
	for i, tr := range triggers {
		trigger, _ := tr.(map[string]any)
		typ, _ := trigger["type"].(string)
		if typ == "" {
			l.add("  trigger %d: type not set", i+1)
			continue
		}
		l.add("  trigger %d: type %s", i+1, l.data(b.value(typ)))
	}
	status, _ := so.Object["status"].(map[string]any)
	if status == nil {
		l.add("  status: none reported yet")
		return
	}
	for _, key := range []string{"externalMetricNames", "resourceMetricNames"} {
		names, _ := status[key].([]any)
		for _, n := range names {
			if s, ok := n.(string); ok && s != "" {
				l.add("  metric name: %s", l.data(b.value(s)))
			}
		}
	}
	for _, typ := range scaledObjectConditions {
		c := kedaready.FirstCondition(status, typ)
		if c == nil {
			l.add("  condition %s: not reported", typ)
			continue
		}
		st, _ := c["status"].(string)
		reason, _ := c["reason"].(string)
		message, _ := c["message"].(string)
		l.add("  condition %s=%s, reason %s: %s", typ, l.data(b.value(st)), l.data(b.value(reason)), l.data(b.value(message)))
	}
}

// replicaCount renders an integer field of a ScaledObject's spec, or "not set" when it is missing or not an
// integer.
func replicaCount(spec map[string]any, key string) string {
	switch v := spec[key].(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case nil:
		return "not set"
	default:
		return "not an integer"
	}
}
