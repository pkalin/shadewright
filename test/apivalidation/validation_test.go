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

package apivalidation

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

var seq int

const githubURLMsg = "a GitHub repository URL must be https://github.com/<owner>/<repo> in lower case, without .git"

// unique returns a fresh object name, so specs never collide.
func unique(prefix string) string {
	seq++
	return fmt.Sprintf("%s-%d", prefix, seq)
}

// expectRefused asserts the API server refuses obj as invalid, with msg in the error.
func expectRefused(obj client.Object, msg string) {
	err := k8sClient.Create(ctx, obj)
	ExpectWithOffset(1, err).To(HaveOccurred(), "the API server accepted an invalid object")
	ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
	ExpectWithOffset(1, err.Error()).To(ContainSubstring(msg))
}

var _ = Describe("RemediationRequest", func() {
	DescribeTable("checks status.source.buildPath",
		func(buildPath string, ok bool) {
			rr := fx.RemediationRequest("default", unique("rr"))
			Expect(k8sClient.Create(ctx, rr)).To(Succeed())
			rr.Status.Source = &v1alpha1.ResolvedSource{
				RepositoryRef: "fleet", TargetBranch: "main", Path: "apps/orders/deployment.yaml",
				BuildPath: v1alpha1.GitDirPath(buildPath), BaseCommit: "0123456789abcdef0123456789abcdef01234567",
			}
			err := k8sClient.Status().Update(ctx, rr)
			if ok {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred(), "the API server accepted an invalid buildPath")
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
			Expect(err.Error()).To(ContainSubstring("status.source.buildPath"))
		},
		Entry("the repository root", ".", true),
		Entry("a nested directory", "apps/orders", true),
		Entry("absolute", "/apps", false),
		Entry("parent reference", "../apps", false),
		Entry("parent reference below", "apps/../..", false),
		Entry("hidden directory", ".github", false),
		Entry("hidden directory below", "apps/.github", false),
		Entry("option-like name", "-rf", false),
		Entry("trailing slash", "apps/", false),
		Entry("empty segment", "apps//orders", false),
		Entry("URL", "https://example.com/apps", false),
	)

	It("accepts the fixture and fills in safe defaults", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		Expect(k8sClient.Create(ctx, rr)).To(Succeed())

		Expect(*rr.Spec.ApprovalRequired).To(BeTrue(), "approval must default to required")
		Expect(rr.Spec.Mode).To(Equal(v1alpha1.RemediationModeGitOps))
		Expect(rr.Spec.Verification.Timeout.Duration.String()).To(Equal("10m0s"))
		Expect(rr.Spec.Verification.StableFor.Duration.String()).To(Equal("3m0s"))
	})

	It("accepts a memory limit action on a Deployment", func() {
		Expect(k8sClient.Create(ctx, fx.MemoryLimitRequest("default", unique("rr")))).To(Succeed())
	})

	It("has no field that chooses the repository, branch or file", func() {
		Expect(reflect.TypeFor[v1alpha1.RemediationRequestSpec]().NumField()).To(Equal(7),
			"a new spec field must not let the author choose where a change is made; see ADR-0012")
		for _, f := range []string{"GitOps", "RepositoryRef", "TargetBranch", "Path", "BaseCommit"} {
			_, found := reflect.TypeFor[v1alpha1.RemediationRequestSpec]().FieldByName(f)
			Expect(found).To(BeFalse(), f)
		}
	})

	It("refuses any change to the plan after creation", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		Expect(k8sClient.Create(ctx, rr)).To(Succeed())

		rr.Spec.Action.AdjustKedaMaxReplicas.DesiredValue = 100
		err := k8sClient.Update(ctx, rr)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec is immutable"))
	})

	It("refuses an action type outside the catalog", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.Type = "RunShellCommand"
		expectRefused(rr, "spec.action.type")
	})

	It("refuses parameters that do not match the action type", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.SetContainerMemoryLimit = fx.MemoryLimitAction().SetContainerMemoryLimit
		expectRefused(rr, "setContainerMemoryLimit must be set if and only if type is SetContainerMemoryLimit")
	})

	It("refuses an action without parameters", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.AdjustKedaMaxReplicas = nil
		expectRefused(rr, "adjustKedaMaxReplicas must be set if and only if type is AdjustKedaMaxReplicas")
	})

	It("refuses a no-op KEDA change", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.AdjustKedaMaxReplicas.DesiredValue = 10
		expectRefused(rr, "desiredValue must differ from previousValue")
	})

	It("refuses KEDA values out of range", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Action.AdjustKedaMaxReplicas.DesiredValue = 5000
		expectRefused(rr, "desiredValue")
	})

	It("refuses a no-op memory change written differently", func() {
		rr := fx.MemoryLimitRequest("default", unique("rr"))
		rr.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("268435456") // 256Mi in bytes
		expectRefused(rr, "desired must differ from previous")
	})

	DescribeTable("refuses memory limits that are not whole bytes in range",
		func(desired, msg string) {
			rr := fx.MemoryLimitRequest("default", unique("rr"))
			rr.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse(desired)
			expectRefused(rr, msg)
		},
		Entry("zero", "0", "desired must be between 1Mi and 1Ti"),
		Entry("negative", "-1Gi", "desired must be between 1Mi and 1Ti"),
		Entry("one byte", "1", "desired must be between 1Mi and 1Ti"),
		Entry("a fraction of a byte", "1m", "previous and desired must be whole bytes"),
		Entry("above 1Ti", "2Ti", "desired must be between 1Mi and 1Ti"),
	)

	It("refuses a container name that is not a DNS label", func() {
		rr := fx.MemoryLimitRequest("default", unique("rr"))
		rr.Spec.Action.SetContainerMemoryLimit.Container = "-worker"
		expectRefused(rr, "spec.action.setContainerMemoryLimit.container")
	})

	DescribeTable("refuses an action on a kind it does not support",
		func(mutate func(*v1alpha1.RemediationRequest), msg string) {
			rr := fx.RemediationRequest("default", unique("rr"))
			mutate(rr)
			expectRefused(rr, msg)
		},
		Entry("KEDA action on a Deployment", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.TargetRef.APIVersion, rr.Spec.TargetRef.Kind = "apps/v1", "Deployment"
		}, "AdjustKedaMaxReplicas can only target a keda.sh ScaledObject"),
		Entry("KEDA action on an HPA", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.TargetRef.APIVersion, rr.Spec.TargetRef.Kind = "autoscaling/v2", "HorizontalPodAutoscaler"
		}, "AdjustKedaMaxReplicas can only target a keda.sh ScaledObject"),
		Entry("memory action on a ScaledObject", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.Action = fx.MemoryLimitAction()
		}, "SetContainerMemoryLimit can only target an apps/v1 Deployment or StatefulSet"),
		Entry("memory action on a DaemonSet", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.Action = fx.MemoryLimitAction()
			rr.Spec.TargetRef.APIVersion, rr.Spec.TargetRef.Kind = "apps/v1", "DaemonSet"
		}, "SetContainerMemoryLimit can only target an apps/v1 Deployment or StatefulSet"),
	)

	It("refuses a target without a UID", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.TargetRef.UID = ""
		expectRefused(rr, "targetRef.uid is required")
	})

	It("refuses a cluster-scoped target", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.TargetRef.Namespace = ""
		expectRefused(rr, "targetRef.namespace is required")
	})

	DescribeTable("refuses malformed target references",
		func(mutate func(*v1alpha1.RemediationRequest), field string) {
			rr := fx.RemediationRequest("default", unique("rr"))
			mutate(rr)
			expectRefused(rr, field)
		},
		Entry("target UID", func(rr *v1alpha1.RemediationRequest) { rr.Spec.TargetRef.UID = "not-a-uid" },
			"spec.targetRef.uid"),
		Entry("incident UID", func(rr *v1alpha1.RemediationRequest) { rr.Spec.IncidentRef.UID = "x" },
			"spec.incidentRef.uid"),
		Entry("namespace", func(rr *v1alpha1.RemediationRequest) { rr.Spec.TargetRef.Namespace = "Orders" },
			"spec.targetRef.namespace"),
		Entry("target name with markdown", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.TargetRef.Name = "worker\n## Approved by policy"
		}, "spec.targetRef.name"),
		Entry("incident name with backticks", func(rr *v1alpha1.RemediationRequest) {
			rr.Spec.IncidentRef.Name = "`x`"
		}, "spec.incidentRef.name"),
		Entry("kind", func(rr *v1alpha1.RemediationRequest) { rr.Spec.TargetRef.Kind = "scaled object" },
			"spec.targetRef.kind"),
		Entry("apiVersion", func(rr *v1alpha1.RemediationRequest) { rr.Spec.TargetRef.APIVersion = "keda.sh/latest" },
			"spec.targetRef.apiVersion"),
	)

	DescribeTable("refuses verification that is too lax or never ends",
		func(timeout, stableFor, msg string) {
			rr := fx.RemediationRequest("default", unique("rr"))
			d := func(s string) *metav1.Duration {
				v, err := time.ParseDuration(s)
				Expect(err).NotTo(HaveOccurred())
				return &metav1.Duration{Duration: v}
			}
			rr.Spec.Verification.Timeout, rr.Spec.Verification.StableFor = d(timeout), d(stableFor)
			expectRefused(rr, msg)
		},
		Entry("stable for zero", "10m", "0s", "stableFor must be at least 1m"),
		Entry("timeout of days", "100h", "3m", "timeout must be at most 24h"),
		Entry("timeout shorter than stability", "2m", "3m", "timeout must be longer than stableFor"),
	)

	It("refuses an unknown verification check", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Verification.Checks = []v1alpha1.VerificationCheck{"AlwaysPass"}
		expectRefused(rr, "spec.verification.checks")
	})

	It("refuses a mode other than GitOps", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.Mode = "Direct"
		expectRefused(rr, "spec.mode")
	})
})

var _ = Describe("Incident", func() {
	It("accepts the fixture", func() {
		Expect(k8sClient.Create(ctx, fx.Incident("default", unique("inc")))).To(Succeed())
	})

	It("refuses a changed fingerprint", func() {
		inc := fx.Incident("default", unique("inc"))
		Expect(k8sClient.Create(ctx, inc)).To(Succeed())
		inc.Spec.Fingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		err := k8sClient.Update(ctx, inc)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec is immutable"))
	})

	It("refuses a fingerprint in the wrong format", func() {
		inc := fx.Incident("default", unique("inc"))
		inc.Spec.Fingerprint = "abc"
		expectRefused(inc, "spec.fingerprint")
	})
})

var _ = Describe("RemediationPolicy", func() {
	It("accepts the fixture with safe defaults", func() {
		p := fx.RemediationPolicy(unique("policy"))
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		Expect(p.Spec.Approval).To(Equal(v1alpha1.ApprovalRequired))
		Expect(p.Spec.RequestNamespaces).To(ConsistOf(v1alpha1.Name("shadewright-system")))
		b := p.Spec.Actions[0].AdjustKedaMaxReplicas
		Expect(b.Min).To(BeEquivalentTo(1))
		Expect(b.MaxIncreasePercent).To(BeEquivalentTo(50))
		Expect(*b.MaxDecreasePercent).To(BeEquivalentTo(0), "decreases must be refused unless allowed")
	})

	It("accepts optional approval and memory bounds", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Approval = v1alpha1.ApprovalOptional
		p.Spec.Actions = append(p.Spec.Actions, fx.MemoryLimitPolicyAction())
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		Expect(*p.Spec.Actions[1].SetContainerMemoryLimit.MaxDecreasePercent).To(BeEquivalentTo(0))
	})

	It("refuses KEDA bounds with min above max", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Actions[0].AdjustKedaMaxReplicas.Min = 30
		expectRefused(p, "min must not be greater than max")
	})

	DescribeTable("refuses memory bounds that are not whole bytes in range",
		func(minQ, maxQ, msg string) {
			p := fx.RemediationPolicy(unique("policy"))
			a := fx.MemoryLimitPolicyAction()
			a.SetContainerMemoryLimit.Min, a.SetContainerMemoryLimit.Max = resource.MustParse(minQ), resource.MustParse(maxQ)
			p.Spec.Actions = []v1alpha1.PolicyAction{a}
			expectRefused(p, msg)
		},
		Entry("zero min", "0", "2Gi", "min must be at least 1Mi"),
		Entry("negative max", "128Mi", "-1Gi", "min must not be greater than max"),
		Entry("milli bytes", "128Mi", "1500m", "min and max must be whole bytes"),
		Entry("max above 1Ti", "128Mi", "2Ti", "max must be at most 1Ti"),
	)

	It("refuses an unbounded decrease", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Actions[0].AdjustKedaMaxReplicas.MaxDecreasePercent = ptr.To[int32](100)
		expectRefused(p, "maxDecreasePercent")
	})

	It("falls back to shadewright-system when request namespaces are left empty", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.RequestNamespaces = []v1alpha1.Name{}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		Expect(p.Spec.RequestNamespaces).To(ConsistOf(v1alpha1.Name("shadewright-system")))
	})

	It("refuses a request namespace that is not a DNS label", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.RequestNamespaces = []v1alpha1.Name{"Team_A"}
		expectRefused(p, "spec.requestNamespaces[0]")
	})

	It("refuses a name longer than a DNS label", func() {
		p := fx.RemediationPolicy("p" + strings.Repeat("x", 63))
		expectRefused(p, "name must be at most 63 characters")
	})

	It("refuses a policy without namespaces", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Targets.Namespaces = nil
		expectRefused(p, "spec.targets.namespaces")
	})

	It("refuses bounds that do not match the action type", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Actions[0].Type = v1alpha1.ActionSetContainerMemoryLimit
		expectRefused(p, "setContainerMemoryLimit must be set if and only if type is SetContainerMemoryLimit")
	})

	It("refuses two entries for the same action type", func() {
		p := fx.RemediationPolicy(unique("policy"))
		p.Spec.Actions = append(p.Spec.Actions, p.Spec.Actions[0])
		expectRefused(p, "Duplicate value")
	})

	DescribeTable("accepts narrow path patterns",
		func(pattern string) {
			p := fx.RemediationPolicy(unique("policy"))
			p.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{v1alpha1.GitPathPattern(pattern)}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		},
		Entry("one segment wildcard", "apps/*/scaledobject.yaml"),
		Entry("recursive under a directory", "apps/**/*.yaml"),
	)

	DescribeTable("refuses path patterns that are too wide or escape the repository",
		func(pattern string) {
			p := fx.RemediationPolicy(unique("policy"))
			p.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{v1alpha1.GitPathPattern(pattern)}
			expectRefused(p, "spec.gitOps.allowedPaths[0]")
		},
		Entry("whole repository", "**"),
		Entry("every YAML file", "**/*.yaml"),
		Entry("wildcard first segment", "*/scaledobject.yaml"),
		Entry("absolute", "/etc/passwd"),
		Entry("parent reference", "apps/../../x.yaml"),
		Entry("parent reference inside a segment", "apps/a..b/x.yaml"),
		Entry("hidden directory", ".github/workflows/*.yml"),
		Entry("hidden directory below", "apps/.github/x.yaml"),
		Entry("option-like name", "-rf.yaml"),
		Entry("shell characters", "apps/$(id).yaml"),
		Entry("empty segment", "apps//x.yaml"),
		Entry("trailing slash", "apps/"),
	)
})

var _ = Describe("RemediationSettings", func() {
	It("accepts the fixture with automation enabled and conservative limits", func() {
		s := fx.RemediationSettings()
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		Expect(*s.Spec.Paused).To(BeFalse())
		Expect(s.Spec.MaxConcurrentRemediations).To(BeEquivalentTo(1))
		Expect(*s.Spec.MaxAnalysesPerDay).To(BeEquivalentTo(100))
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
	})

	It("refuses any name but default", func() {
		s := fx.RemediationSettings()
		s.Name = "other"
		expectRefused(s, "named default")
	})

	It("refuses a repository URL that is not HTTPS", func() {
		s := fx.RemediationSettings()
		s.Spec.Repositories[0].URL = "git@github.com:example/fleet.git"
		expectRefused(s, "spec.repositories[0].url")
	})

	It("defaults the target branch to main", func() {
		s := fx.RemediationSettings()
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		Expect(s.Spec.Repositories[0].TargetBranch).To(BeEquivalentTo("main"))
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
	})

	DescribeTable("refuses target branches git could misread",
		func(branch string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].TargetBranch = v1alpha1.GitBranch(branch)
			expectRefused(s, "spec.repositories[0].targetBranch")
		},
		Entry("option-like", "-main"),
		Entry("parent reference", "release/../main"),
		Entry("full ref", "refs/heads/main"),
		Entry("HEAD", "HEAD"),
		Entry("lock file", "main.lock"),
		Entry("trailing slash", "main/"),
		Entry("Shadewright's own branch", "shadewright/11111111-1111-4111-8111-111111111111"),
	)

	DescribeTable("refuses repository URLs that carry credentials or point elsewhere",
		func(url, msg string) {
			s := fx.RemediationSettings()
			s.Spec.Repositories[0].URL = url
			expectRefused(s, msg)
		},
		Entry("credentials", "https://x-access-token:ghp_secret@github.com/example/fleet", "spec.repositories[0].url"),
		Entry("query", "https://github.com/example/fleet?token=x", "spec.repositories[0].url"),
		Entry("another host for a GitHub repository", "https://evil.example.com/example/fleet", githubURLMsg),
		Entry("lookalike host", "https://github.com.evil.example/example/fleet", githubURLMsg),
		Entry("extra path", "https://github.com/example/fleet/tree/main", githubURLMsg),
		Entry(".git suffix", "https://github.com/example/fleet.git", githubURLMsg),
		Entry("upper case", "https://github.com/Example/fleet", githubURLMsg),
		Entry("dot owner", "https://github.com/./fleet", "spec.repositories[0].url"),
		Entry("parent repository", "https://github.com/example/..", "spec.repositories[0].url"),
		Entry("dot in owner", "https://github.com/ex.ample/fleet", githubURLMsg),
	)

	It("refuses two repositories with the same URL", func() {
		s := fx.RemediationSettings()
		second := *s.Spec.Repositories[0].DeepCopy()
		second.Name = "fleet-copy"
		s.Spec.Repositories = append(s.Spec.Repositories, second)
		expectRefused(s, "repository URLs must be unique")
	})

	It("refuses a GitHub repository without App settings", func() {
		s := fx.RemediationSettings()
		s.Spec.Repositories[0].GitHub = nil
		expectRefused(s, "github must be set if and only if provider is GitHub")
	})

	It("refuses a signer key outside Azure Key Vault", func() {
		s := fx.RemediationSettings()
		s.Spec.Repositories[0].GitHub.Signer.AzureKeyVault.KeyID = "https://evil.example.com/keys/github-app"
		expectRefused(s, "keyID")
	})
})
