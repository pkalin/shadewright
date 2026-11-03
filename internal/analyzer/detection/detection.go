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

package detection

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/modules"
)

// Defaults for the zero values of [Config], and bounds.
const (
	// DefaultInterval is how long after a successful run detection runs again.
	DefaultInterval = 10 * time.Minute
	// DefaultTimeout is the deadline of the node list and of the discovery call, each.
	DefaultTimeout = 30 * time.Second
	// MaxNodes is the most nodes read; a cluster with more is taken as generic. AKS allows 5,000 nodes.
	MaxNodes = 5000
	// nodePage is the page size of the node list. A page holds whole Node objects (see listNodes), so it is
	// kept small.
	nodePage = 100
)

// Condition of RemediationSettings status written by the detector.
const (
	// ConditionUnknownModules is True when spec.modules names a module this analyzer does not have.
	ConditionUnknownModules = "UnknownModules"
	// ReasonUnknownModulesEnabled and ReasonAllModulesKnown are its reasons.
	ReasonUnknownModulesEnabled = "UnknownModulesEnabled"
	ReasonAllModulesKnown       = "AllModulesKnown"
	// ConditionDetectionFailed is True when the last detection run failed. Its message is fixed text: no
	// error from the cluster is written to status.
	ConditionDetectionFailed = "DetectionFailed"
	// ReasonDetectionFailed and ReasonDetected are its reasons.
	ReasonDetectionFailed = "DetectionFailed"
	ReasonDetected        = "Detected"
	// FieldOwner is the field manager of the status writes.
	FieldOwner = "shadewright-detection"
)

// GroupVersionsFunc returns the group versions the discovery API serves, in discovery form.
type GroupVersionsFunc func(ctx context.Context) ([]string, error)

// Config configures a [Detector].
type Config struct {
	// Registry holds the compiled-in modules. Required.
	Registry *modules.Registry
	// GroupVersions reads the discovery API. Required; [DiscoveryGroupVersions] in the binary.
	GroupVersions GroupVersionsFunc
	// Interval is how long after a successful run detection runs again.
	Interval time.Duration
	// Timeout bounds the node list and the discovery call, each.
	Timeout time.Duration
}

// Detector detects the platform and the integrations, keeps the active modules and reports them in
// RemediationSettings status.
type Detector struct {
	cfg Config
	// reader reads nodes and settings from the API server; writer writes the settings status.
	reader client.Reader
	writer client.Client

	mu      sync.Mutex
	current modules.Selection
	// last is the last detection that succeeded, valid when haveLast is set.
	last     modules.Detection
	haveLast bool
}

// outcome is how the detection of one run went, for the status.
type outcome struct {
	failed   bool // the run's detection failed
	haveLast bool // a detection succeeded before; the selection uses it
}

// New validates cfg and returns a detector whose selection is generic only.
func New(cfg Config) (*Detector, error) {
	if cfg.Registry == nil {
		return nil, errors.New("detection: a module registry is required")
	}
	if cfg.GroupVersions == nil {
		return nil, errors.New("detection: a discovery function is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	d := &Detector{cfg: cfg}
	d.current = cfg.Registry.Select(modules.Enabled{}, modules.Detection{})
	return d, nil
}

// Selection returns the active modules as of the last run. It is safe for concurrent use.
func (d *Detector) Selection() modules.Selection {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

func (d *Detector) store(s modules.Selection) {
	d.mu.Lock()
	d.current = s
	d.mu.Unlock()
}

// SetupWithManager registers the controller. It watches RemediationSettings through the cache (list and
// watch are granted) but reads the object and the nodes with the API reader.
func (d *Detector) SetupWithManager(mgr ctrl.Manager) error {
	d.reader, d.writer = mgr.GetAPIReader(), mgr.GetClient()
	err := ctrl.NewControllerManagedBy(mgr).
		Named("detection").
		For(&remediationv1alpha1.RemediationSettings{}, builder.WithPredicates(
			predicate.GenerationChangedPredicate{},
			predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetName() == remediationv1alpha1.SettingsName }),
		)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(d)
	if err != nil {
		return fmt.Errorf("setting up platform detection: %w", err)
	}
	return nil
}

// Reconcile runs detection and reports it (see the package documentation).
func (d *Detector) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != remediationv1alpha1.SettingsName {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)
	det, derr := d.detect(ctx)
	if err := ctx.Err(); err != nil {
		// Stopping (or a lost leadership): write nothing.
		return ctrl.Result{}, err
	}
	d.mu.Lock()
	if derr == nil {
		d.last, d.haveLast = det, true
	}
	det, o := d.last, outcome{failed: derr != nil, haveLast: d.haveLast}
	d.mu.Unlock()

	sel, found, err := d.refresh(ctx, det, o)
	if derr != nil {
		return ctrl.Result{}, errors.Join(fmt.Errorf("detecting the platform and integrations: %w", derr), err)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		// No settings: nothing to report and no periodic run. The watch starts a run when they are created.
		return ctrl.Result{}, nil
	}
	log.V(1).Info("Detected platform and integrations", "detected", sel.DetectedPlatform(),
		"active", sel.ActivePlatform())
	return ctrl.Result{RequeueAfter: d.cfg.Interval}, nil
}

// refresh reads the settings, works out the selection from their spec and det, stores it and writes the
// status if it changed. A conflict reads the settings again. Settings that do not exist give generic only and
// found false; settings that cannot be read keep the previous selection and return the error.
func (d *Detector) refresh(ctx context.Context, det modules.Detection, o outcome) (sel modules.Selection, found bool, err error) {
	sel = d.Selection()
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var s remediationv1alpha1.RemediationSettings
		key := client.ObjectKey{Name: remediationv1alpha1.SettingsName}
		if err := d.reader.Get(ctx, key, &s); err != nil {
			if apierrors.IsNotFound(err) {
				sel, found = d.cfg.Registry.Select(modules.Enabled{}, det), false
				d.store(sel)
				return nil
			}
			return fmt.Errorf("reading RemediationSettings: %w", err)
		}
		found = true
		sel = d.cfg.Registry.Select(enabledOf(s.Spec.Modules), det)
		d.store(sel)
		want := statusFor(s, sel, o)
		if equality.Semantic.DeepEqual(s.Status, want) {
			return nil
		}
		s.Status = want
		if err := d.writer.Status().Update(ctx, &s, client.FieldOwner(FieldOwner)); err != nil {
			if apierrors.IsConflict(err) {
				return err
			}
			return fmt.Errorf("writing RemediationSettings status: %w", err)
		}
		return nil
	})
	return sel, found, err
}

// enabledOf converts spec.modules.
func enabledOf(m *remediationv1alpha1.Modules) modules.Enabled {
	var e modules.Enabled
	if m == nil {
		return e
	}
	for _, n := range m.Platforms {
		e.Platforms = append(e.Platforms, string(n))
	}
	for _, n := range m.Integrations {
		e.Integrations = append(e.Integrations, string(n))
	}
	return e
}

// statusFor returns s's status with the detector's fields set from sel and o. Conditions of other types are
// kept. When no detection has succeeded yet, it reports only what is known: generic active, nothing detected,
// no integrations, and DetectionFailed True, so an earlier report does not stay in place.
func statusFor(s remediationv1alpha1.RemediationSettings, sel modules.Selection, o outcome) remediationv1alpha1.RemediationSettingsStatus {
	st := *s.Status.DeepCopy()
	st.ObservedGeneration = s.Generation
	p := sel.PlatformStatus()
	st.Platform = &p
	st.Integrations = sel.IntegrationStatus()
	failed := metav1.Condition{
		Type:               ConditionDetectionFailed,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonDetected,
		Message:            "The last detection run succeeded",
		ObservedGeneration: s.Generation,
	}
	switch {
	case o.failed && !o.haveLast:
		st.Platform = &remediationv1alpha1.PlatformStatus{Active: remediationv1alpha1.GenericPlatform}
		st.Integrations = nil
		failed.Status, failed.Reason = metav1.ConditionTrue, ReasonDetectionFailed
		failed.Message = "Detection has not succeeded since the analyzer started: only the generic platform is " +
			"active, and nothing is reported as detected"
	case o.failed:
		failed.Status, failed.Reason = metav1.ConditionTrue, ReasonDetectionFailed
		failed.Message = "The last detection run failed: platform and integrations are reported from the last " +
			"run that succeeded"
	}
	meta.SetStatusCondition(&st.Conditions, failed)
	c := metav1.Condition{
		Type:               ConditionUnknownModules,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonAllModulesKnown,
		Message:            "Every enabled module is compiled into this analyzer",
		ObservedGeneration: s.Generation,
	}
	if msg := sel.UnknownMessage(); msg != "" {
		c.Status, c.Reason, c.Message = metav1.ConditionTrue, ReasonUnknownModulesEnabled, msg
	}
	meta.SetStatusCondition(&st.Conditions, c)
	return st
}

// detect lists the nodes and reads discovery, each with its deadline.
func (d *Detector) detect(ctx context.Context) (modules.Detection, error) {
	nodes, err := d.listNodes(ctx)
	if err != nil {
		return modules.Detection{}, err
	}
	gctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	gvs, err := d.cfg.GroupVersions(gctx)
	if err != nil {
		return modules.Detection{}, fmt.Errorf("reading the discovery API: %w", err)
	}
	return d.cfg.Registry.Detect(nodes, gvs), nil
}

// listNodes reads the provider ID and the label keys of every node, in pages. More than MaxNodes give no
// node at all, so the platform is generic: detection on part of a cluster is not certain.
//
// The list reads whole Node objects: a metadata-only list (PartialObjectMetadataList) has the labels but not
// spec.providerID, and there is no server-side projection of one spec field. So pages are small (nodePage),
// each page is reduced at once to the two fields detection reads, and the objects are dropped before the next
// page is read.
func (d *Detector) listNodes(ctx context.Context) ([]modules.Node, error) {
	lctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	var out []modules.Node
	cont := ""
	for {
		var list corev1.NodeList
		if err := d.reader.List(lctx, &list, client.Limit(nodePage), client.Continue(cont)); err != nil {
			return nil, fmt.Errorf("listing nodes: %w", err)
		}
		for i := range list.Items {
			out = append(out, nodeOfForDetection(&list.Items[i]))
		}
		cont = list.Continue
		list = corev1.NodeList{}
		if len(out) > MaxNodes {
			logf.FromContext(ctx).Info("Took the cluster as generic: it has more nodes than detection reads",
				"maxNodes", MaxNodes)
			return nil, nil
		}
		if cont == "" {
			return out, nil
		}
	}
}

// nodeOfForDetection keeps what platform detection reads: the provider ID and the label keys.
func nodeOfForDetection(n *corev1.Node) modules.Node {
	labels := make(map[string]string, len(n.Labels))
	for k := range n.Labels {
		labels[k] = ""
	}
	return modules.Node{ProviderID: n.Spec.ProviderID, Labels: labels}
}

// DiscoveryGroupVersions returns a [GroupVersionsFunc] that reads the discovery API with cfg. The discovery
// client takes no context, so the call runs in its own goroutine and the function returns when the context
// ends; the client's requests also time out with the context's deadline, so the goroutine ends by then too.
func DiscoveryGroupVersions(cfg *rest.Config) GroupVersionsFunc {
	return func(ctx context.Context) ([]string, error) {
		c := rest.CopyConfig(cfg)
		if dl, ok := ctx.Deadline(); ok {
			c.Timeout = time.Until(dl)
			if c.Timeout <= 0 {
				return nil, context.DeadlineExceeded
			}
		}
		dc, err := discovery.NewDiscoveryClientForConfig(c)
		if err != nil {
			return nil, err
		}
		type result struct {
			gvs []string
			err error
		}
		done := make(chan result, 1) // buffered: the goroutine never blocks after the caller left
		go func() {
			groups, err := dc.ServerGroups()
			if err != nil {
				done <- result{err: err}
				return
			}
			var out []string
			for _, g := range groups.Groups {
				for _, v := range g.Versions {
					out = append(out, v.GroupVersion)
				}
			}
			done <- result{gvs: out}
		}()
		select {
		case r := <-done:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return r.gvs, r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
