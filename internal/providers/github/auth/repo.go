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

package auth

import (
	"fmt"
	"regexp"
	"strings"
)

// Repository is a GitHub repository named by its URL.
type Repository struct {
	// Owner is the user or organization.
	Owner string
	// Name is the repository name.
	Name string
}

// String returns owner/name.
func (r Repository) String() string { return r.Owner + "/" + r.Name }

const repoURLPrefix = "https://github.com/"

var (
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	// clientIDPattern accepts GitHub App client IDs ("Iv1.", "Iv23li..." followed by alphanumerics).
	clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// ParseRepositoryURL accepts only https://github.com/<owner>/<repo>: no other scheme or host, no userinfo,
// port, query, fragment, trailing slash or ".git" suffix. The error never repeats the input.
func ParseRepositoryURL(raw string) (Repository, error) {
	rest, ok := strings.CutPrefix(raw, repoURLPrefix)
	if !ok {
		return Repository{}, fmt.Errorf("repository URL must be %s<owner>/<repo>", repoURLPrefix)
	}
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || !ownerPattern.MatchString(owner) || !namePattern.MatchString(name) ||
		name == "." || name == ".." || strings.HasSuffix(name, ".git") {
		return Repository{}, fmt.Errorf("repository URL must be %s<owner>/<repo>", repoURLPrefix)
	}
	return Repository{Owner: owner, Name: name}, nil
}

func validateClientID(id string) error {
	if !clientIDPattern.MatchString(id) {
		return fmt.Errorf("GitHub App client ID is empty, too long or has invalid characters")
	}
	return nil
}

func validateInstallationID(id int64) error {
	if id < 1 {
		return fmt.Errorf("installation ID must be positive")
	}
	return nil
}
