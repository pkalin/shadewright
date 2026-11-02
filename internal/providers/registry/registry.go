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
	"fmt"
	"sync"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/github"
	"github.com/pkalin/shadewright/internal/providers/github/auth"
	"github.com/pkalin/shadewright/internal/providers/signer/azurekv"
)

// Builders construct the parts of a provider. The zero value of a field means the real adapter; tests inject
// fakes so that nothing reaches Azure or GitHub.
type Builders struct {
	// Signer returns the signer for a Key Vault key ID that has already been validated.
	Signer func(keyID string) (providers.Signer, error)
	// Tokens returns the installation token source for the config.
	Tokens func(cfg auth.Config) (github.TokenSource, error)
	// Git returns the provider for the config.
	Git func(cfg github.Config) (providers.GitProvider, error)
}

func (b Builders) withDefaults() Builders {
	if b.Signer == nil {
		b.Signer = func(keyID string) (providers.Signer, error) {
			s, err := azurekv.New(keyID, azurekv.Options{})
			if err != nil {
				return nil, err
			}
			return s, nil
		}
	}
	if b.Tokens == nil {
		b.Tokens = func(cfg auth.Config) (github.TokenSource, error) {
			s, err := auth.New(cfg)
			if err != nil {
				return nil, err
			}
			return s, nil
		}
	}
	if b.Git == nil {
		b.Git = func(cfg github.Config) (providers.GitProvider, error) {
			p, err := github.New(cfg)
			if err != nil {
				return nil, err
			}
			return p, nil
		}
	}
	return b
}

// Registry builds and keeps providers. It is safe for concurrent use.
type Registry struct {
	// Builders construct the parts; the zero value uses the real adapters.
	Builders Builders
	// Current returns the repositories in the settings now. When set, every Open drops the providers whose
	// entry is no longer among them. An error from it skips the dropping, never the Open.
	Current func(ctx context.Context) ([]v1alpha1.GitRepository, error)

	mu        sync.Mutex
	providers map[key]providers.GitProvider
}

// key is everything a provider is built from. The settings name is not part of it: two names with the same
// values would build the same provider.
type key struct {
	url            string
	installationID int64
	appClientID    string
	keyID          string
	targetBranch   string
}

// Fixed refusal texts. They are constants so that no input can reach an error message.
var (
	errNotGitHub    = fmt.Errorf("repository settings: the provider is not GitHub: %w", providers.ErrInvalidConfig)
	errNoGitHub     = fmt.Errorf("repository settings: the github block is missing: %w", providers.ErrInvalidConfig)
	errNoSigner     = fmt.Errorf("repository settings: no signer is configured: %w", providers.ErrInvalidConfig)
	errBadKeyID     = fmt.Errorf("repository settings: the Key Vault key ID is invalid: %w", providers.ErrInvalidConfig)
	errBadTokens    = fmt.Errorf("repository settings: the App client ID, installation ID or URL is invalid: %w", providers.ErrInvalidConfig)
	errBadGit       = fmt.Errorf("repository settings: the URL or target branch is invalid: %w", providers.ErrInvalidConfig)
	errBuildSigner  = errors.New("repository settings: the signer could not be built")
	errBuildNothing = errors.New("repository settings: the provider could not be built")
)

// keyOf validates the entry and returns its key.
func keyOf(repo v1alpha1.GitRepository) (key, error) {
	if repo.Provider != v1alpha1.GitProviderGitHub {
		return key{}, errNotGitHub
	}
	gh := repo.GitHub
	if gh == nil {
		return key{}, errNoGitHub
	}
	if gh.Signer.AzureKeyVault == nil {
		return key{}, errNoSigner
	}
	if _, err := azurekv.ParseKeyID(gh.Signer.AzureKeyVault.KeyID); err != nil {
		return key{}, errBadKeyID
	}
	return key{
		url:            repo.URL,
		installationID: gh.InstallationID,
		appClientID:    gh.AppClientID,
		keyID:          gh.Signer.AzureKeyVault.KeyID,
		targetBranch:   string(repo.TargetBranch),
	}, nil
}

// Open returns the provider for repo, building it on first use. It never returns an error that holds a URL,
// an ID or adapter text. An error matching [providers.ErrInvalidConfig] is permanent; any other is worth a
// retry (a failed build is not cached).
func (r *Registry) Open(ctx context.Context, repo v1alpha1.GitRepository) (providers.GitProvider, error) {
	k, err := keyOf(repo)
	if err != nil {
		return nil, err
	}
	r.prune(ctx, k)

	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.providers[k]; ok {
		return p, nil
	}
	p, err := r.build(k)
	if err != nil {
		return nil, err
	}
	if r.providers == nil {
		r.providers = map[key]providers.GitProvider{}
	}
	r.providers[k] = p
	return p, nil
}

// build constructs the signer, the token source and the provider. No step makes a network call.
func (r *Registry) build(k key) (providers.GitProvider, error) {
	if err := validate(k); err != nil {
		return nil, err
	}
	b := r.Builders.withDefaults()
	signer, err := b.Signer(k.keyID)
	if err != nil || signer == nil {
		// Not a settings error: the settings were validated above, so this is the environment (no workload
		// identity).
		return nil, errBuildSigner
	}
	tokens, err := b.Tokens(auth.Config{
		ClientID:       k.appClientID,
		InstallationID: k.installationID,
		RepositoryURL:  k.url,
		Signer:         signer,
	})
	if err != nil || tokens == nil {
		return nil, errBadTokens
	}
	git, err := b.Git(github.Config{RepositoryURL: k.url, TargetBranch: k.targetBranch, Tokens: tokens})
	if err != nil {
		return nil, errBadGit
	}
	if git == nil {
		return nil, errBuildNothing
	}
	return git, nil
}

// validate runs the real config checks of the token source and the provider, which make no network call, with
// a signer that is never called. It runs before the signer is built, so settings the adapter refuses are
// reported as unusable even when the environment has no workload identity, instead of being retried.
func validate(k key) error {
	tokens, err := auth.New(auth.Config{
		ClientID:       k.appClientID,
		InstallationID: k.installationID,
		RepositoryURL:  k.url,
		Signer:         unusedSigner{},
	})
	if err != nil {
		return errBadTokens
	}
	if _, err := github.New(github.Config{RepositoryURL: k.url, TargetBranch: k.targetBranch, Tokens: tokens}); err != nil {
		return errBadGit
	}
	return nil
}

// unusedSigner stands in for the Key Vault signer while the settings are validated; nothing calls Sign.
type unusedSigner struct{}

func (unusedSigner) Algorithm() providers.SignatureAlgorithm { return providers.RS256 }

func (unusedSigner) Sign(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("the validation signer does not sign")
}

// prune drops the providers whose entry is no longer in the settings. keep is the key being opened, which
// stays even when the settings list was read just before it was added.
func (r *Registry) prune(ctx context.Context, keep key) {
	if r.Current == nil {
		return
	}
	repos, err := r.Current(ctx)
	if err != nil {
		return
	}
	live := map[key]bool{keep: true}
	for _, repo := range repos {
		if k, err := keyOf(repo); err == nil {
			live[k] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.providers {
		if !live[k] {
			delete(r.providers, k)
		}
	}
}

// Len returns the number of providers kept. It is meant for tests and metrics.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.providers)
}
