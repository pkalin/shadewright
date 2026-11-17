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

package redact

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// Every built-in rule has a sample in package redacttest, in rule order, which that rule alone redacts: the
// tests of other packages that take their secrets from there cover every rule, and a new rule fails here until
// it has one.
func TestEveryRuleHasASample(t *testing.T) {
	rules, samples := builtinRules(), redacttest.Samples()
	if len(samples) != len(rules) {
		t.Fatalf("%d samples for %d built-in rules: add one to package redacttest for each new rule", len(samples), len(rules))
	}
	for i, s := range samples {
		if s.Rule != i || !strings.Contains(s.Text, s.Key+s.Secret) {
			t.Errorf("sample %d: rule %d, or its text %q does not hold its key and secret", i, s.Rule, s.Text)
			continue
		}
		alone := (&Redactor{rules: rules[i : i+1]}).String(s.Text)
		if strings.Contains(alone, s.Secret) || alone == s.Text {
			t.Errorf("rule %d (%s) alone leaves sample %q as %q", i, rules[i].kind, s.Text, alone)
		}
		if got := String(s.Text); strings.Contains(got, s.Secret) {
			t.Errorf("rule %d (%s): the built-in rules leave the secret of %q in %q", i, rules[i].kind, s.Text, got)
		}
	}
}
