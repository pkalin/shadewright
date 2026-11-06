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

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// version is the plugin's version, stamped at build time with -ldflags "-X main.version=<version>" (make
// build-plugin uses git describe). A build without it is "dev".
var version = "dev"

// devVersion is the version of a build that was not stamped.
const devVersion = "dev"

// crdVersionLabel is the label the Helm chart puts on every resource it renders, the CRDs included, with the
// chart's appVersion (the shadewright.labels helper). The kustomize install sets no such label.
const crdVersionLabel = "app.kubernetes.io/version"

// shadewrightCRDs are the CRDs the status reads.
var shadewrightCRDs = []string{
	"incidents.remediation.shadewright.io",
	"remediationrequests.remediation.shadewright.io",
	"remediationsettings.remediation.shadewright.io",
}

// crdVersionWarning returns a one-line warning when a Shadewright CRD in the cluster carries a version label
// other than plugin, or "" when they agree or nothing can be compared: the plugin is a dev build, or no CRD
// could be read (a viewer without get on customresourcedefinitions) or carries the label. It reads only the
// CRDs' metadata, with get.
func crdVersionWarning(ctx context.Context, c client.Reader, plugin string) string {
	if plugin == "" || plugin == devVersion {
		return ""
	}
	var others []string
	for _, name := range shadewrightCRDs {
		m := &metav1.PartialObjectMetadata{}
		m.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
		if err := c.Get(ctx, client.ObjectKey{Name: name}, m); err != nil {
			continue
		}
		v := m.Labels[crdVersionLabel]
		if v == "" || sameVersion(v, plugin) {
			continue
		}
		v = clean(v, 63)
		if !slices.Contains(others, v) {
			others = append(others, v)
		}
	}
	if len(others) == 0 {
		return ""
	}
	return fmt.Sprintf("Warning: the Shadewright CRDs in the cluster are version %s and this plugin is %s; "+
		"fields may be missing or misread", strings.Join(others, ", "), clean(plugin, 63))
}

// sameVersion compares two versions, ignoring a leading "v".
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}
