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
	"errors"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
)

// maxRenderEnds is how many ends of the render process in a row that are neither an answer nor a limit
// refusal (semdiff.RenderEnded) the render check of one plan takes before it refuses the change (ADR-0035).
// The same build can end that way on every retry, and the request's deadline does not end the retries once its
// pull request is recorded.
const maxRenderEnds = 5

// maxRenderEndEntries bounds the requests renderEnds keeps a count for. When more than 1,024 requests have a count
// at once, eviction can drop a live count, so five attempts are guaranteed only while the entry survives. Known
// and accepted (ADR-0035).
const maxRenderEndEntries = 1024

// renderEnds counts, per request and plan digest, the ends of the render process in a row that are neither an
// answer nor a limit refusal (#614, ADR-0035). It is the one place that counts them (checkRender). Memory only:
// after a restart or a change of leader the count starts again, and a Pod that the render process takes down
// with it never counts (#714). Entries are dropped when the request ends, is being deleted or is gone (forget).
type renderEnds struct {
	mu sync.Mutex
	m  map[types.NamespacedName]renderEndCount
}

// renderEndCount is one entry of renderEnds: the request's UID and plan digest, and the ends in a row.
type renderEndCount struct {
	uid    types.UID
	digest string
	n      int
}

// note records the outcome err of semdiff.Check for rr and returns what the render check returns: err, or a
// [*renderRefusal] with reason TooLarge at the maxRenderEnds-th end in a row. An answer (nil or a refusal of
// the check) clears the count; any other error, a context that ended among them, leaves it as it is.
func (c *renderEnds) note(rr *remediationv1alpha1.RemediationRequest, err error) error {
	key := client.ObjectKeyFromObject(rr)
	c.mu.Lock()
	defer c.mu.Unlock()
	ended, isEnd := errors.AsType[*semdiff.RenderEnded](err)
	if _, refused := errors.AsType[*semdiff.Error](err); err == nil || refused {
		delete(c.m, key)
		return err
	}
	if !isEnd {
		return err
	}
	e, kept := c.m[key]
	if !kept || e.uid != rr.UID || e.digest != rr.Status.PlanDigest {
		e = renderEndCount{uid: rr.UID, digest: rr.Status.PlanDigest}
	}
	e.n++
	if e.n >= maxRenderEnds {
		delete(c.m, key)
		return &renderRefusal{reason: string(semdiff.ReasonTooLarge), message: fmt.Sprintf(
			"The render process ended without an answer %d times in a row; the last time it %s", maxRenderEnds, ended.Cause)}
	}
	if c.m == nil {
		c.m = map[types.NamespacedName]renderEndCount{}
	}
	if !kept && len(c.m) >= maxRenderEndEntries {
		for k := range c.m { // full: one count, any, starts again
			delete(c.m, k)
			break
		}
	}
	c.m[key] = e
	return err
}

// forget drops the count of the request with key.
func (c *renderEnds) forget(key types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}
