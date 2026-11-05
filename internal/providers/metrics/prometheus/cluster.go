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

package prometheus

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/pkalin/shadewright/internal/providers"
)

// ErrClusterNotFound means the source holds no series of [Config.Cluster] from kube-state-metrics, or none
// from cAdvisor: the value is mistyped, or the label is not stored on the series (an external label added
// only on the way out, for example). Every template would then answer with nothing, which reads as evidence
// of absence. It is permanent until the source or the configuration changes.
var ErrClusterNotFound = errors.New("no series carries the configured cluster label")

// clusterProbeMetrics are one metric of each family the templates read: kube-state-metrics and cAdvisor. The
// cluster label is usually added per scrape job, so a label on one job only would leave half of the templates
// empty; the probe asks for both.
var clusterProbeMetrics = [2]string{"kube_pod_container_status_restarts_total", "container_memory_working_set_bytes"}

// clusterProbe is the one fixed query of the check: 1 when both families hold a series of the cluster, no
// sample otherwise. group() keeps no label, so the answer says nothing about any namespace, pod or count.
func clusterProbe(cluster string) string {
	return "group(" + clusterProbeMetrics[0] + "{" + eq(clusterLabel, cluster) + "}) and group(" +
		clusterProbeMetrics[1] + "{" + eq(clusterLabel, cluster) + "})"
}

// CheckCluster checks that the source holds series of [Config.Cluster] from both kube-state-metrics and
// cAdvisor, with one fixed instant query that is not a template and returns no label. It returns nil when it
// does, and also when no cluster is configured (there is nothing to check); [ErrClusterNotFound] when it
// does not; and the errors of [Source.Query] when the source cannot answer. An empty answer with a warning is
// not knowing, a [providers.RetryableError], never ErrClusterNotFound.
func (s *Source) CheckCluster(ctx context.Context) error {
	if s.cluster == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	server, err := serverTimeout(ctx)
	if err != nil {
		return err
	}
	end := s.now().UnixMilli()
	form := url.Values{}
	form.Set("query", clusterProbe(s.cluster))
	form.Set("time", formatMs(end))
	form.Set("timeout", strconv.FormatInt(server.Milliseconds(), 10)+"ms")
	res, err := s.do(ctx, "api/v1/query", form, window{fromMs: end, toMs: end, instant: true}, nil)
	switch {
	case err != nil:
		return err
	case len(res.Series) > 0:
		return nil
	case res.Partial:
		return &providers.RetryableError{Err: errors.New("prometheus gave a partial answer to the cluster check")}
	default:
		return fmt.Errorf("prometheus cluster check: %w", ErrClusterNotFound)
	}
}
