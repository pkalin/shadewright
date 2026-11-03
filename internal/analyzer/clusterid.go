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

package analyzer

import (
	"fmt"
	"regexp"
)

// clusterID is what --cluster-id accepts: 1 to 63 letters, digits, dots, underscores and hyphens, starting
// and ending with a letter or digit.
var clusterID = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

// ParseClusterID validates the value of --cluster-id. The ID is hashed into every fingerprint, so Incidents
// from two clusters never merge, and changing it starts every Incident afresh. It is set by whoever installs
// the analyzer, not read from the cluster, so it needs no permission.
func ParseClusterID(flag string) (string, error) {
	if flag == "" {
		return "", fmt.Errorf("no cluster ID: set --cluster-id")
	}
	if !clusterID.MatchString(flag) {
		return "", fmt.Errorf("cluster ID %q is not valid: use 1 to 63 letters, digits, '.', '_' or '-', "+
			"starting and ending with a letter or digit", flag)
	}
	return flag, nil
}
