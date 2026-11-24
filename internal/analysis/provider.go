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

package analysis

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
)

// EventModelProviderChanged is the reason of the Normal event on the Incident whose analysis has the first call
// to a role's model provider that returns having sent a request, since the analyzer started or since the
// settings moved that role to another provider (ADR-0037): it shows where that role's redacted context goes
// from then on. Calls of concurrent analyses are noted in the order they return, so a call that sent earlier
// but returned later does not get it.
const EventModelProviderChanged = "ModelProviderChanged"

// providersUsed is the provider of each role's last call that returned having sent a request.
type providersUsed struct {
	mu   sync.Mutex
	last map[Role]remediationv1alpha1.ModelProvider
}

// swap records p as role's provider and returns the one before, and whether it changed.
func (u *providersUsed) swap(role Role, p remediationv1alpha1.ModelProvider) (remediationv1alpha1.ModelProvider, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	prev := u.last[role]
	if prev == p {
		return prev, false
	}
	u.last[role] = p
	return prev, true
}

// model builds the provider of role's deployment d with the model factory. A refused deployment changes
// nothing: no context goes anywhere for it.
func (a *Analyzer) model(role Role, d *remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
	return a.cfg.Models(role, *d)
}

// sent notes, once a call of inc's analysis has returned, that it may have sent a request to provider p for
// role: every call whose error is not marked [providers.ErrNotSent]. When p is not the provider of the last
// call noted for role, it logs the change and records EventModelProviderChanged on inc. So an analysis that
// reserved no budget unit, was cancelled before its call or whose call sent nothing records nothing, and the
// next call that returns having sent does, which need not be the first to send. Both name only the role and
// the provider names, never an endpoint, a deployment or a credential.
func (a *Analyzer) sent(ctx context.Context, inc *remediationv1alpha1.Incident, role Role,
	p remediationv1alpha1.ModelProvider) {
	prev, changed := a.used.swap(role, p)
	if !changed {
		return
	}
	before := string(prev)
	if before == "" {
		before = "none since the analyzer started"
	}
	log.FromContext(ctx).Info("The model provider of a role changed", "role", role, "provider", p,
		"previous", before, "incident", inc.Name)
	if a.cfg.Recorder != nil {
		a.cfg.Recorder.Eventf(inc, corev1.EventTypeNormal, EventModelProviderChanged,
			"The %s model now uses provider %s (before: %s); its redacted context goes to that provider",
			role, p, before)
	}
}
