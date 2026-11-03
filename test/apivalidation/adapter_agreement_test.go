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
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/github"
	"github.com/pkalin/shadewright/internal/providers/github/auth"
	"github.com/pkalin/shadewright/internal/providers/signer/azurekv"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// settingsField is a RemediationSettings repository field that the GitHub adapter or the signer checks again.
type settingsField int

const (
	fieldURL settingsField = iota
	fieldAppClientID
	fieldTargetBranch
	fieldKeyID
)

// rs256Signer stands in for the Key Vault signer: the adapter checks only its algorithm when it is built.
type rs256Signer struct{}

func (rs256Signer) Algorithm() providers.SignatureAlgorithm { return providers.RS256 }

func (rs256Signer) Sign(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("not used")
}

// adapterAccepts runs the Go check that guards field: auth.ParseRepositoryURL, the App client ID check in
// auth.New, validTargetBranch in github.New, or azurekv.ParseKeyID. The other values are the fixture's.
func adapterAccepts(field settingsField, value string) bool {
	base := fx.RemediationSettings().Spec.Repositories[0]
	tokens := func(clientID string) (*auth.Source, error) {
		return auth.New(auth.Config{
			ClientID:       clientID,
			InstallationID: base.GitHub.InstallationID,
			RepositoryURL:  base.URL,
			Signer:         rs256Signer{},
		})
	}
	switch field {
	case fieldURL:
		_, err := auth.ParseRepositoryURL(value)
		return err == nil
	case fieldAppClientID:
		_, err := tokens(value)
		return err == nil
	case fieldTargetBranch:
		src, err := tokens(base.GitHub.AppClientID)
		Expect(err).NotTo(HaveOccurred(), "the fixture must pass the token source checks")
		_, err = github.New(github.Config{RepositoryURL: base.URL, TargetBranch: value, Tokens: src})
		return err == nil
	case fieldKeyID:
		_, err := azurekv.ParseKeyID(value)
		return err == nil
	}
	Fail("unknown field")
	return false
}

// schemaAccepts reports whether the API server stores the fixture with field set to value.
func schemaAccepts(field settingsField, value string) bool {
	s := fx.RemediationSettings()
	repo := &s.Spec.Repositories[0]
	switch field {
	case fieldURL:
		repo.URL = value
	case fieldAppClientID:
		repo.GitHub.AppClientID = value
	case fieldTargetBranch:
		repo.TargetBranch = v1alpha1.GitBranch(value)
	case fieldKeyID:
		repo.GitHub.Signer.AzureKeyVault.KeyID = value
	}
	err := k8sClient.Create(ctx, s)
	if err == nil {
		deleteAfterSpec(s)
		return true
	}
	Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
	return false
}

// The schema and the Go checks must agree on every entry, so a rule changed on one side alone fails here.
// adapterOK is what the Go check returns. stricter is empty when the schema must agree; otherwise it names the
// documented schema rule that refuses a value the adapter accepts.
var _ = Describe("RemediationSettings and the GitHub adapter", func() {
	DescribeTable("agree on repository entries",
		func(field settingsField, value string, adapterOK bool, stricter string) {
			Expect(adapterAccepts(field, value)).To(Equal(adapterOK), "the Go check changed")
			schemaOK := schemaAccepts(field, value)
			if stricter == "" {
				Expect(schemaOK).To(Equal(adapterOK), "the schema and the Go check disagree")
				return
			}
			Expect(adapterOK).To(BeTrue(), "a stricter-schema entry must be one the adapter accepts")
			Expect(schemaOK).To(BeFalse(), "the schema no longer refuses this (%s)", stricter)
		},
		// Repository URL.
		Entry(nil, fieldURL, "https://github.com/example/fleet", true, ""),
		Entry(nil, fieldURL, "https://github.com/"+strings.Repeat("o", 39)+"/fleet", true, ""),
		Entry(nil, fieldURL, "https://github.com/example/"+strings.Repeat("n", 100), true, ""),
		Entry(nil, fieldURL, "https://github.com/e/f", true, ""),
		Entry(nil, fieldURL, "https://github.com/1-example/fleet_repo.v2", true, ""),
		Entry(nil, fieldURL, "https://github.com/example/.github", true, ""),
		Entry(nil, fieldURL, "https://github.com/example/..x", true, ""),
		Entry(nil, fieldURL, "https://github.com/example/...", true, ""),
		Entry(nil, fieldURL, "https://github.com/example/.", false, ""),
		Entry(nil, fieldURL, "https://github.com/example/..", false, ""),
		Entry(nil, fieldURL, "https://github.com/./fleet", false, ""),
		Entry(nil, fieldURL, "https://github.com/example/.git", false, ""),
		Entry(nil, fieldURL, "https://github.com/example/fleet.git", false, ""),
		Entry(nil, fieldURL, "https://github.com/-example/fleet", false, ""),
		Entry(nil, fieldURL, "https://github.com/ex.ample/fleet", false, ""),
		Entry(nil, fieldURL, "https://github.com/"+strings.Repeat("o", 40)+"/fleet", false, ""),
		Entry(nil, fieldURL, "https://github.com/example/"+strings.Repeat("n", 101), false, ""),
		Entry(nil, fieldURL, "https://github.com/example/fleet/tree/main", false, ""),
		Entry(nil, fieldURL, "https://github.com/example/fleet/", false, ""),
		Entry(nil, fieldURL, "https://github.com/example/fl~eet", false, ""),
		Entry(nil, fieldURL, "https://github.com:443/example/fleet", false, ""),
		Entry(nil, fieldURL, "https://github.com/example/fleet?x=1", false, ""),
		Entry(nil, fieldURL, "https://evil.example.com/example/fleet", false, ""),
		Entry(nil, fieldURL, "http://github.com/example/fleet", false, ""),
		Entry(nil, fieldURL, "https://github.com/Example/fleet", true, "lower case, to match the Flux source URL"),
		Entry(nil, fieldURL, "https://github.com/example/Fleet", true, "lower case, to match the Flux source URL"),
		// App client ID.
		Entry(nil, fieldAppClientID, "Iv1.0123456789abcdef", true, ""),
		Entry(nil, fieldAppClientID, "Iv23li.A_b-9", true, ""),
		Entry(nil, fieldAppClientID, strings.Repeat("a", 64), true, ""),
		Entry(nil, fieldAppClientID, "", false, ""),
		Entry(nil, fieldAppClientID, strings.Repeat("a", 65), false, ""),
		Entry(nil, fieldAppClientID, "Iv23 example", false, ""),
		Entry(nil, fieldAppClientID, "Iv23/example", false, ""),
		Entry(nil, fieldAppClientID, "Iv23:example", false, ""),
		Entry(nil, fieldAppClientID, "Iv23éxample", false, ""),
		// Target branch.
		Entry(nil, fieldTargetBranch, "main", true, ""),
		Entry(nil, fieldTargetBranch, "b"+strings.Repeat("x", 199), true, ""),
		Entry(nil, fieldTargetBranch, "release/v1.2_rc", true, ""),
		Entry(nil, fieldTargetBranch, "release/_next", true, ""),
		Entry(nil, fieldTargetBranch, "2026-release", true, ""),
		Entry(nil, fieldTargetBranch, "b"+strings.Repeat("x", 200), false, ""),
		Entry(nil, fieldTargetBranch, "_main", false, ""),
		Entry(nil, fieldTargetBranch, "-main", false, ""),
		Entry(nil, fieldTargetBranch, ".main", false, ""),
		Entry(nil, fieldTargetBranch, "main.", false, ""),
		Entry(nil, fieldTargetBranch, "release/v1.", false, ""),
		Entry(nil, fieldTargetBranch, "release/.hidden", false, ""),
		Entry(nil, fieldTargetBranch, "release/../main", false, ""),
		Entry(nil, fieldTargetBranch, "release//main", false, ""),
		Entry(nil, fieldTargetBranch, "main/", false, ""),
		Entry(nil, fieldTargetBranch, "main.lock", false, ""),
		Entry(nil, fieldTargetBranch, "shadewright/next", false, ""),
		Entry(nil, fieldTargetBranch, "HEAD", true, "GitBranch refuses HEAD"),
		Entry(nil, fieldTargetBranch, "refs/heads/main", true, "GitBranch refuses refs/"),
		Entry(nil, fieldTargetBranch, "a.lock/b", true, "GitBranch refuses a .lock segment"),
		// Key Vault key ID.
		Entry(nil, fieldKeyID, "https://kv-example.vault.azure.net/keys/github-app", true, ""),
		Entry(nil, fieldKeyID, "https://kv1.vault.azure.net/keys/github-app", true, ""),
		Entry(nil, fieldKeyID, "https://k"+strings.Repeat("v", 23)+".vault.azure.net/keys/github-app", true, ""),
		Entry(nil, fieldKeyID, "https://kv-example.vault.azure.net/keys/"+strings.Repeat("k", 125)+"--", true, ""),
		Entry(nil, fieldKeyID,
			"https://kv-example.vault.azure.net/keys/github-app/0123456789abcdef0123456789abcdef", true, ""),
		Entry(nil, fieldKeyID, "https://kv.vault.azure.net/keys/github-app", false, ""),
		Entry(nil, fieldKeyID, "https://k"+strings.Repeat("v", 24)+".vault.azure.net/keys/github-app", false, ""),
		Entry(nil, fieldKeyID, "https://1kv-example.vault.azure.net/keys/github-app", false, ""),
		Entry(nil, fieldKeyID, "https://-kv-example.vault.azure.net/keys/github-app", false, ""),
		Entry(nil, fieldKeyID, "https://kv-example-.vault.azure.net/keys/github-app", false, ""),
		Entry(nil, fieldKeyID, "https://kv--example.vault.azure.net/keys/github-app", false, ""),
		Entry(nil, fieldKeyID, "https://kv-example.vault.azure.net/keys/"+strings.Repeat("k", 128), false, ""),
		Entry(nil, fieldKeyID,
			"https://kv-example.vault.azure.net/keys/github-app/0123456789ABCDEF0123456789ABCDEF", false, ""),
		Entry(nil, fieldKeyID, "https://kv-example.vault.azure.net/keys/github_app", false, ""),
		Entry(nil, fieldKeyID, "https://kv-example.vault.azure.net:443/keys/github-app", false, ""),
		Entry(nil, fieldKeyID, "https://evil.example.com/keys/github-app", false, ""),
	)
})
