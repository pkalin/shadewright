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
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/kedaready"
)

const (
	soUID = "uid-so"
	// keyName is the name key of a JSON object.
	keyName = "name"
)

// Fields of the ScaledObject fixture that the status section must never show (ADR-0036 decision 3).
const (
	soServerAddress = "http://prom-secret-host.monitoring.svc:9090"
	soQuery         = "sum(rate(http_requests_total{app=\"never-shown\"}[1m]))"
	soAuthRef       = "keda-trigger-auth-never-shown"
	soAnnotation    = "annotation-value-never-shown"
	soLabel         = "label-value-never-shown"
)

func kcond(typ, status, reason, message string) any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": message}
}

func readyConditions() []any {
	return []any{
		kcond("Ready", "True", "ScaledObjectReady", "ScaledObject is defined correctly and is ready for scaling"),
		kcond("Active", "True", "ScalerActive", "Scaling is performed because triggers are active"),
		kcond("Fallback", "False", "NoFallbackFound", "No fallbacks are active on this scaled object"),
		kcond("Paused", "False", "ScaledObjectUnpaused", "pause annotation removed for ScaledObject"),
	}
}

func scaledObject(uid string, conds []any) *unstructured.Unstructured {
	so := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kedaready.APIVersion, "kind": kedaready.KindScaledObject,
		"metadata": map[string]any{"namespace": testNS, keyName: appName, "uid": uid,
			"annotations": map[string]any{"example.com/note": soAnnotation},
			"labels":      map[string]any{"team": soLabel}},
		"spec": map[string]any{
			"minReplicaCount": int64(1), "maxReplicaCount": int64(2),
			"scaleTargetRef": map[string]any{"kind": "Deployment", keyName: "worker"},
			"triggers": []any{map[string]any{"type": "prometheus",
				"metadata":          map[string]any{"serverAddress": soServerAddress, "query": soQuery, "threshold": "5"},
				"authenticationRef": map[string]any{keyName: soAuthRef}}},
		},
		"status": map[string]any{"externalMetricNames": []any{"s0-prometheus"}, "conditions": conds},
	}}
	return so
}

func scaledObjectRef() remediationv1alpha1.ObjectRef {
	return remediationv1alpha1.ObjectRef{APIVersion: kedaready.APIVersion, Kind: kedaready.KindScaledObject, Namespace: testNS,
		Name: appName, UID: soUID}
}

// A ScaledObject subject is read with one get and shown from the allow-list only: its replica bounds, its scale
// target, the type of each trigger, the metric names and the four conditions. The snapshot is kept for the
// analysis loop.
func TestScaledObjectSubjectIsShownFromTheAllowList(t *testing.T) {
	r := reader(t, scaledObject(soUID, readyConditions()))
	c := build(t, newBuilder(t, r, sources{}, nil), incident(scaledObjectRef()))
	blk := one(t, c, SectionStatus)
	if blk.Provenance != ProvenanceScaledObject || blk.Availability != Available {
		t.Fatalf("block %+v", blk)
	}
	for _, want := range []string{
		"ScaledObject " + testNS + "/" + appName,
		"spec: minReplicaCount 1, maxReplicaCount 2",
		"scale target: Deployment worker",
		"trigger 1: type prometheus",
		"metric name: s0-prometheus",
		"condition Ready=True, reason ScaledObjectReady: ScaledObject is defined correctly and is ready for scaling",
		"condition Active=True, reason ScalerActive",
		"condition Fallback=False, reason NoFallbackFound",
		"condition Paused=False, reason ScaledObjectUnpaused",
	} {
		if !strings.Contains(blk.Text, want) {
			t.Errorf("block lacks %q:\n%s", want, blk.Text)
		}
	}
	all := ""
	for _, b := range c.Blocks {
		all += b.Text
	}
	for _, never := range []string{soServerAddress, "prom-secret-host", soQuery, "http_requests_total", soAuthRef,
		soAnnotation, soLabel, "threshold"} {
		if strings.Contains(all, never) {
			t.Errorf("the context shows %q", never)
		}
	}
	if !slices.Contains(blk.Data(), "ScaledObject is defined correctly and is ready for scaling") {
		t.Errorf("the condition message is not quotable: %q", blk.Data())
	}
	if c.ScaledObject == nil || c.Escalate() {
		t.Fatalf("snapshot %v, escalate %t", c.ScaledObject, c.Escalate())
	}
	spec, _ := c.ScaledObject["spec"].(map[string]any)
	if spec["maxReplicaCount"] != int64(2) {
		t.Errorf("snapshot maxReplicaCount %v", spec["maxReplicaCount"])
	}
}

// A token placed across the size bound of a condition message is redacted before the bound is applied: the
// message is left out whole and no part of the token reaches the context. Cutting first would keep the
// token's head, too short for the redactor to recognise.
func TestScaledObjectMessageOverTheBoundIsLeftOutWhole(t *testing.T) {
	const token = "ghp_" + "Ab1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0UvWxYz1234"
	filler := strings.Repeat("metrics endpoint said: ", 45)[:maxValueBytes-9] + " "
	if start := len(filler); start >= maxValueBytes || start+len(token) <= maxValueBytes {
		t.Fatalf("the token at %d..%d does not cross the bound %d", start, start+len(token), maxValueBytes)
	}
	conds := readyConditions()
	conds[0] = kcond("Ready", "False", "ScaledObjectCheckFailed", filler+token)
	r := reader(t, scaledObject(soUID, conds))
	c := build(t, newBuilder(t, r, sources{}, nil), incident(scaledObjectRef()))
	blk := one(t, c, SectionStatus)
	if !strings.Contains(blk.Text, "condition Ready=False, reason ScaledObjectCheckFailed: (") ||
		!strings.Contains(blk.Text, "bytes left out)") {
		t.Fatalf("the message is not left out whole:\n%s", blk.Text)
	}
	for _, b := range c.Blocks {
		for i := 0; i+8 <= len(token); i++ {
			if strings.Contains(b.Text, token[i:i+8]) {
				t.Fatalf("block %q shows part of the token: %q", b.Section, token[i:i+8])
			}
		}
		if strings.Contains(b.Text, "metrics endpoint said") {
			t.Fatalf("block %q shows part of the message", b.Section)
		}
	}
}

// forbiddenScaledObject refuses the get of a ScaledObject, as the API server does when the KEDA module's role
// is not bound.
type forbiddenScaledObject struct{ client.Reader }

func (f forbiddenScaledObject) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == kedaready.KindScaledObject {
		return apierrors.NewForbidden(schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}, key.Name, nil)
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// A refused read of the ScaledObject is a failed source, never "not found": the block holds no data, the
// snapshot is absent and the context escalates, as for any failed read of the subject.
func TestScaledObjectReadForbiddenIsAFailedSource(t *testing.T) {
	r := reader(t, scaledObject(soUID, readyConditions()))
	c := build(t, newBuilder(t, r, sources{}, func(c *Config) { c.Objects = forbiddenScaledObject{r} }),
		incident(scaledObjectRef()))
	blk := one(t, c, SectionStatus)
	if c.ScaledObject != nil || blk.Availability != CouldNotFetch || len(blk.Data()) != 0 ||
		strings.Contains(blk.Text, "maxReplicaCount") || strings.Contains(blk.Text, "no longer exists") {
		t.Fatalf("snapshot %v, block %+v", c.ScaledObject, blk)
	}
	if !slices.Contains(c.FailedSources, SectionStatus) || !c.Escalate() {
		t.Errorf("failed sources %v", c.FailedSources)
	}
}

// A ScaledObject of the subject's name under another UID is not the subject: no snapshot.
func TestScaledObjectOfAnotherUIDIsGone(t *testing.T) {
	r := reader(t, scaledObject("uid-other", readyConditions()))
	c := build(t, newBuilder(t, r, sources{}, nil), incident(scaledObjectRef()))
	if blk := one(t, c, SectionStatus); c.ScaledObject != nil || !strings.Contains(blk.Text, "no longer exists") {
		t.Fatalf("snapshot %v, block %+v", c.ScaledObject, blk)
	}
}
