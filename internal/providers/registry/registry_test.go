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

package registry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/providers/github"
	"github.com/pkalin/shadewright/internal/providers/github/auth"
)

const (
	secretURL   = "https://github.com/secret-owner/secret-repo"
	secretKeyID = "https://secret-vault.vault.azure.net/keys/secret-key"
	secretAppID = "Iv1.secretclientid"
	otherBranch = "release"
)

func entry() v1alpha1.GitRepository {
	return v1alpha1.GitRepository{
		Name:         "repo",
		URL:          secretURL,
		TargetBranch: "main",
		Provider:     v1alpha1.GitProviderGitHub,
		GitHub: &v1alpha1.GitHubRepository{
			AppClientID:    secretAppID,
			InstallationID: 424242,
			Signer:         v1alpha1.Signer{AzureKeyVault: &v1alpha1.AzureKeyVaultKey{KeyID: secretKeyID}},
		},
	}
}

type counts struct{ signers, tokens, gits atomic.Int32 }

// fakeBuilders count constructions and use the real validation of the token source and the provider, with a
// fake signer, so nothing reaches Azure or GitHub.
func fakeBuilders(c *counts) Builders {
	return Builders{
		Signer: func(string) (providers.Signer, error) {
			c.signers.Add(1)
			return fake.NewSigner()
		},
		Tokens: func(cfg auth.Config) (github.TokenSource, error) {
			c.tokens.Add(1)
			s, err := auth.New(cfg)
			if err != nil {
				return nil, err
			}
			return s, nil
		},
		Git: func(cfg github.Config) (providers.GitProvider, error) {
			c.gits.Add(1)
			p, err := github.New(cfg)
			if err != nil {
				return nil, err
			}
			return p, nil
		},
	}
}

func TestOpenBuildsOnceAndReuses(t *testing.T) {
	var c counts
	r := &Registry{Builders: fakeBuilders(&c)}
	if c.signers.Load() != 0 {
		t.Fatal("nothing may be built before the first Open")
	}
	a, err := r.Open(context.Background(), entry())
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Open(context.Background(), entry())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("the same entry must return the same provider")
	}
	if c.signers.Load() != 1 || c.tokens.Load() != 1 || c.gits.Load() != 1 {
		t.Errorf("built %d signers, %d token sources, %d providers; want one each",
			c.signers.Load(), c.tokens.Load(), c.gits.Load())
	}
	// The settings name is not part of the key.
	renamed := entry()
	renamed.Name = "other"
	if p, _ := r.Open(context.Background(), renamed); p != a {
		t.Error("the name must not change the provider")
	}
}

func TestOpenRebuildsOnChange(t *testing.T) {
	changes := map[string]func(*v1alpha1.GitRepository){
		"url":            func(g *v1alpha1.GitRepository) { g.URL = "https://github.com/other/repo" },
		"installationID": func(g *v1alpha1.GitRepository) { g.GitHub.InstallationID++ },
		"appClientID":    func(g *v1alpha1.GitRepository) { g.GitHub.AppClientID = "Iv1.other" },
		"keyID": func(g *v1alpha1.GitRepository) {
			g.GitHub.Signer.AzureKeyVault = &v1alpha1.AzureKeyVaultKey{KeyID: secretKeyID + "-2"}
		},
		"targetBranch": func(g *v1alpha1.GitRepository) { g.TargetBranch = otherBranch },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			var c counts
			r := &Registry{Builders: fakeBuilders(&c)}
			first, err := r.Open(context.Background(), entry())
			if err != nil {
				t.Fatal(err)
			}
			e := entry()
			change(&e)
			second, err := r.Open(context.Background(), e)
			if err != nil {
				t.Fatal(err)
			}
			if first == second {
				t.Error("a changed entry must get a new provider")
			}
			if c.gits.Load() != 2 {
				t.Errorf("built %d providers, want 2", c.gits.Load())
			}
		})
	}
}

func TestOpenDropsEntriesLeftOutOfTheSettings(t *testing.T) {
	var c counts
	other := entry()
	other.URL = "https://github.com/other/repo"
	current := []v1alpha1.GitRepository{entry(), other}
	r := &Registry{
		Builders: fakeBuilders(&c),
		Current:  func(context.Context) ([]v1alpha1.GitRepository, error) { return current, nil },
	}
	first, _ := r.Open(context.Background(), entry())
	if _, err := r.Open(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 2 {
		t.Fatalf("kept %d providers, want 2", r.Len())
	}
	current = []v1alpha1.GitRepository{entry()}
	if again, _ := r.Open(context.Background(), entry()); again != first {
		t.Error("an entry still in the settings must keep its provider")
	}
	if r.Len() != 1 {
		t.Errorf("kept %d providers after the entry left the settings, want 1", r.Len())
	}
	// A changed entry replaces the old one: only the current key stays.
	changed := entry()
	changed.TargetBranch = otherBranch
	current = []v1alpha1.GitRepository{changed}
	if _, err := r.Open(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 1 {
		t.Errorf("kept %d providers after a change, want 1", r.Len())
	}
}

func TestOpenKeepsProvidersWhenTheSettingsCannotBeRead(t *testing.T) {
	var c counts
	r := &Registry{
		Builders: fakeBuilders(&c),
		Current:  func(context.Context) ([]v1alpha1.GitRepository, error) { return nil, errors.New("down") },
	}
	first, _ := r.Open(context.Background(), entry())
	if again, err := r.Open(context.Background(), entry()); err != nil || again != first {
		t.Errorf("a failed settings read must not drop or fail: %v", err)
	}
}

func TestOpenRefusesBadSettings(t *testing.T) {
	cases := map[string]func(*v1alpha1.GitRepository){
		"not GitHub":      func(g *v1alpha1.GitRepository) { g.Provider = "AzureDevOps" },
		"no github block": func(g *v1alpha1.GitRepository) { g.GitHub = nil },
		"no signer":       func(g *v1alpha1.GitRepository) { g.GitHub.Signer = v1alpha1.Signer{} },
		"bad key ID": func(g *v1alpha1.GitRepository) {
			g.GitHub.Signer.AzureKeyVault.KeyID = "https://evil.example.com/keys/secret-key"
		},
		"bad URL":       func(g *v1alpha1.GitRepository) { g.URL = "https://example.com/secret-owner/secret-repo" },
		"bad client ID": func(g *v1alpha1.GitRepository) { g.GitHub.AppClientID = "not valid/secret" },
		"bad install":   func(g *v1alpha1.GitRepository) { g.GitHub.InstallationID = 0 },
		"bad branch":    func(g *v1alpha1.GitRepository) { g.TargetBranch = "" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			var c counts
			b := fakeBuilders(&c)
			// The environment has no workload identity: bad settings must still be refused as settings,
			// before the signer is built.
			b.Signer = func(string) (providers.Signer, error) {
				c.signers.Add(1)
				return nil, errors.New("no workload identity")
			}
			r := &Registry{Builders: b}
			e := entry()
			change(&e)
			p, err := r.Open(context.Background(), e)
			if err == nil || p != nil {
				t.Fatalf("got %v, %v; want a refusal", p, err)
			}
			if !providers.IsInvalidConfig(err) {
				t.Errorf("the refusal must be permanent (ErrInvalidConfig): %v", err)
			}
			if providers.IsRetryable(err) {
				t.Errorf("the refusal must not be retryable: %v", err)
			}
			noLeak(t, err)
			if r.Len() != 0 {
				t.Error("a refusal must not be cached")
			}
			if n := c.signers.Load(); n != 0 {
				t.Errorf("the signer was built %d times for refused settings", n)
			}
		})
	}
}

func TestSignerFailureIsNotPermanentAndNotCached(t *testing.T) {
	var c counts
	fail := true
	b := fakeBuilders(&c)
	good := b.Signer
	b.Signer = func(id string) (providers.Signer, error) {
		if fail {
			return nil, errors.New("AZURE_CLIENT_ID missing for " + secretKeyID)
		}
		return good(id)
	}
	r := &Registry{Builders: b}
	_, err := r.Open(context.Background(), entry())
	if err == nil || providers.IsInvalidConfig(err) {
		t.Fatalf("an environment failure must be an error worth retrying: %v", err)
	}
	noLeak(t, err)
	fail = false
	if _, err := r.Open(context.Background(), entry()); err != nil {
		t.Errorf("the next call must try again: %v", err)
	}
}

func TestBuilderErrorsAreNotRepeated(t *testing.T) {
	leak := errors.New(secretURL + " " + secretKeyID + " " + secretAppID + " 424242")
	for name, b := range map[string]Builders{
		"tokens": {Tokens: func(auth.Config) (github.TokenSource, error) { return nil, leak }},
		"git":    {Git: func(github.Config) (providers.GitProvider, error) { return nil, leak }},
	} {
		t.Run(name, func(t *testing.T) {
			var c counts
			base := fakeBuilders(&c)
			if b.Tokens == nil {
				b.Tokens = base.Tokens
			}
			b.Signer = base.Signer
			_, err := (&Registry{Builders: b}).Open(context.Background(), entry())
			if err == nil {
				t.Fatal("want an error")
			}
			noLeak(t, err)
		})
	}
}

func TestOpenIsSafeForConcurrentUse(t *testing.T) {
	var c counts
	other := entry()
	other.TargetBranch = otherBranch
	r := &Registry{
		Builders: fakeBuilders(&c),
		Current: func(context.Context) ([]v1alpha1.GitRepository, error) {
			return []v1alpha1.GitRepository{entry(), other}, nil
		},
	}
	var wg sync.WaitGroup
	got := make([]providers.GitProvider, 64)
	for i := range got {
		wg.Go(func() {
			e := entry()
			if i%2 == 1 {
				e = other
			}
			got[i], _ = r.Open(context.Background(), e)
		})
	}
	wg.Wait()
	for i := 2; i < len(got); i++ {
		if got[i] == nil || got[i] != got[i%2] {
			t.Fatalf("call %d returned another provider than call %d", i, i%2)
		}
	}
	if c.gits.Load() != 2 {
		t.Errorf("built %d providers under concurrency, want 2", c.gits.Load())
	}
}

func TestDefaultBuildersNeedWorkloadIdentity(t *testing.T) {
	// No AZURE_* variables: the real signer constructor fails, with no text from Azure and no network call.
	for _, v := range []string{"AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_FEDERATED_TOKEN_FILE"} {
		t.Setenv(v, "")
	}
	_, err := (&Registry{}).Open(context.Background(), entry())
	if err == nil || providers.IsInvalidConfig(err) {
		t.Fatalf("got %v, want a retryable build error", err)
	}
	noLeak(t, err)
}

func noLeak(t *testing.T, err error) {
	t.Helper()
	for _, s := range []string{"secret", "424242", "Iv1", "vault", "github.com", "https://", "AZURE"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error %q holds %q", err, s)
		}
	}
}
