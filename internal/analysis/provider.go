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

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// EventModelProviderChanged is the reason of the Normal event on the Incident whose analysis is the first to
// use a role's model provider since the analyzer started or since the settings moved that role to another
// provider (ADR-0037): it shows where that role's redacted context goes from then on.
const EventModelProviderChanged = "ModelProviderChanged"

// providersUsed is the provider each role's last built model used.
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

// model builds the provider of role's deployment d with the model factory. When the provider of the
// deployment is not the one role used last, it logs the change and records EventModelProviderChanged on inc.
// Both name only the role and the provider names, never an endpoint, a deployment or a credential. A refused
// deployment changes nothing: no context goes anywhere for it.
func (a *Analyzer) model(ctx context.Context, inc *remediationv1alpha1.Incident, role Role,
	d *remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
	m, err := a.cfg.Models(role, *d)
	if err != nil {
		return nil, err
	}
	p := d.ProviderName()
	prev, changed := a.used.swap(role, p)
	if !changed {
		return m, nil
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
	return m, nil
}
