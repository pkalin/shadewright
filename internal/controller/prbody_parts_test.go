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
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// partBound stands in for the size bound of the redactor of #873 (redact.MaxEntry there): an entry longer than
// it is withheld whole, as that redactor does, and anything shorter is redacted with the built-in rules.
const partBound = 4096

// boundedRedact is a redaction that withholds any entry over partBound, as the work budget of #873 does.
func boundedRedact(s string, keep ...string) (string, bool) {
	if len(s) > partBound {
		return withheldValue, false
	}
	return redact.StringKeeping(s, keep...), true
}

// wholeBody is the body as main built it: every value written as it is, then the composed text redacted once,
// and the marker added after.
func wholeBody(rr *remediationv1alpha1.RemediationRequest, incident string,
	failed *remediationv1alpha1.RemediationRequest) string {
	plain := composeBody(rr, incident, failed, func(s string, _ ...string) (string, bool) { return s, true })
	text := strings.TrimSuffix(plain, marker(rr)+"\n")
	keep := []string{incident, keptRequestName(rr)}
	if failed != nil {
		keep = append(keep, keptRequestName(failed))
	}
	return redact.StringKeeping(text, keep...) + marker(rr) + "\n"
}

// skeleton returns a pattern that matches the bodies composed for rr (no revert) whatever its values redact to: the
// operator's text exactly, in order, with any text in the place of each value.
func skeleton(rr *remediationv1alpha1.RemediationRequest, incident string) *regexp.Regexp {
	n := 0
	tmpl := composeBody(rr, incident, nil, func(string, ...string) (string, bool) {
		n++
		return fmt.Sprintf("\x00%d\x00", n), true
	})
	fixed := regexp.MustCompile("\x00[0-9]+\x00").Split(tmpl, -1)
	for i := range fixed {
		fixed[i] = regexp.QuoteMeta(fixed[i])
	}
	return regexp.MustCompile(`^` + strings.Join(fixed, `(?s:.*?)`) + `$`)
}

// fixedSections are the operator's sections of a request's body that hold no value of the request.
var fixedSections = []string{
	"**Risk:** the change is bounded by the policy's limits and touches one field. Any other push to this branch " +
		"stops the automation.\n\n",
	"**Rollback:** revert this pull request; the GitOps controller applies the previous value.\n\n",
	"**Approval required:** yes.\n\n",
	". Nothing else in this pull request is part of the plan.\n\n",
	"(decision `WithinApprovedBounds`), plan digest `",
}

func TestPullRequestBodyWithheldValueKeepsTheFixedSections(t *testing.T) {
	rr := bodyRequest()
	big := "apps/" + strings.Repeat("q", 2*partBound) + "/deployment.yaml"
	rr.Status.Source.Path = remediationv1alpha1.GitFilePath(big)

	body := composeBody(rr, "", nil, boundedRedact)

	for _, s := range fixedSections {
		if !strings.Contains(body, s) {
			t.Errorf("the body lost the fixed text %q:\n%s", s, body)
		}
	}
	if !strings.Contains(body, "**Scope:** one value in `"+withheldValue+"` on `"+mainBranch+"`.") {
		t.Errorf("the withheld value is not the marker in its own place:\n%s", body)
	}
	if !strings.Contains(body, bodyWithheld) {
		t.Errorf("the body does not say a value was withheld:\n%s", body)
	}
	if strings.Contains(body, strings.Repeat("q", 64)) {
		t.Errorf("text of the withheld value reached the body")
	}
	if !hasMarker(body, rr) {
		t.Errorf("the body lost its marker:\n%s", body)
	}
	if !skeleton(rr, "").MatchString(strings.Replace(body, bodyWithheld, "", 1)) {
		t.Errorf("the operator's text changed:\n%s", body)
	}
	if n := utf8.RuneCountInString(body); n > 65536 {
		t.Errorf("the body has %d characters, over the code host's 65,536", n)
	}
}

func TestPullRequestBodyWithoutWithheldValueSaysNothingWithheld(t *testing.T) {
	rr := bodyRequest()
	body := composeBody(rr, "", nil, boundedRedact)
	if strings.Contains(body, "**Withheld:**") {
		t.Errorf("a body with no withheld value says one was withheld:\n%s", body)
	}
	if body != pullRequestBody(rr, "", nil) {
		t.Errorf("the bounded redaction changed a body it withholds nothing of")
	}
}

func TestPullRequestBodyWithheldRevertKeepsItsRollback(t *testing.T) {
	failed, revert := revertForBody(t)
	revert.Status.PolicyRevision = strings.Repeat("p", 2*partBound)

	body := composeBody(revert, "", failed, boundedRedact)

	for _, s := range []string{
		"**Rollback:** do not revert this pull request: that applies `384Mi` again, the value of the failed request. " +
			"To keep the current value, close this pull request without merging it.\n\n",
		"**Reverts:** RemediationRequest `",
		"pull request #71. It ended `Failed` with the reason `OOMKilled`.",
		"policy `" + withheldValue + "` (decision `WithinApprovedBounds`)",
		bodyWithheld,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("the body lost %q:\n%s", s, body)
		}
	}
	if !hasMarker(body, revert) {
		t.Errorf("the body lost its marker")
	}
}

func TestPullRequestBodySecretInAValueLeavesTheFixedText(t *testing.T) {
	secret := "hunter2" + "hunter2hunter2"
	for _, tc := range []struct {
		name string
		set  func(rr *remediationv1alpha1.RemediationRequest, v string)
		v    string
		// after is the operator's text right after the value, which a key in the value must not take.
		after string
	}{
		// A value that names a secret key: on main the redactor took the operator's text after it for the
		// key's value and removed it.
		{"policy ref", func(rr *remediationv1alpha1.RemediationRequest, v string) {
			rr.Spec.PolicyRef = remediationv1alpha1.Name(v)
		}, "password=" + secret, "`. The policy bounds the step; it does not say the step is the right size."},
		{"policy revision", func(rr *remediationv1alpha1.RemediationRequest, v string) {
			rr.Status.PolicyRevision = v
		}, "token: " + secret, "` (decision `WithinApprovedBounds`), plan digest `"},
		{"container", func(rr *remediationv1alpha1.RemediationRequest, v string) {
			rr.Spec.Action.SetContainerMemoryLimit.Container = v
		}, "password=" + secret, " (`SetContainerMemoryLimit`) of Deployment `"},
		{"open key", func(rr *remediationv1alpha1.RemediationRequest, v string) {
			rr.Spec.PolicyRef = remediationv1alpha1.Name(v)
		}, "password=", "`. The policy bounds the step; it does not say the step is the right size."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := bodyRequest()
			tc.set(rr, tc.v)
			body := pullRequestBody(rr, "", nil)
			if strings.Contains(body, secret) {
				t.Errorf("the secret reached the body:\n%s", body)
			}
			if !skeleton(rr, "").MatchString(body) {
				t.Errorf("the operator's text changed:\n%s", body)
			}
			if !strings.Contains(body, tc.after) {
				t.Errorf("the key in the value took the operator's text %q:\n%s", tc.after, body)
			}
			for _, s := range fixedSections {
				if !strings.Contains(body, s) {
					t.Errorf("the body lost the fixed text %q:\n%s", s, body)
				}
			}
			if !hasMarker(body, rr) {
				t.Errorf("the body lost its marker")
			}
		})
	}
}

// TestPullRequestBodyKeyInOneValueHidesNoNeighbour: on main a connection string in the target branch ran on
// through the operator's text into the policy revision, and the redactor then read the key there as text and showed
// the secret after it, while the Evidence section went missing.
func TestPullRequestBodyKeyInOneValueHidesNoNeighbour(t *testing.T) {
	first, second := "hunter2"+"hunter2x", "swordfish"+"swordfish1"
	rr := bodyRequest()
	rr.Status.Source.TargetBranch = remediationv1alpha1.GitBranch("password=" + first)
	rr.Status.PolicyRevision = "password: " + second
	body := pullRequestBody(rr, "", nil)
	for _, secret := range []string{first, second} {
		if strings.Contains(body, secret) {
			t.Errorf("secret %q reached the body:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "**Evidence:** RemediationRequest `") || !skeleton(rr, "").MatchString(body) {
		t.Errorf("the operator's text changed:\n%s", body)
	}
}

// bodyValue is one generated value of a body field. secret is the part that must not reach the body, empty for
// a value that holds none.
type bodyValue struct {
	text, secret string
	// contextual is true for a value that names a secret key: on main the redactor read the text after the key,
	// in the next parts and the operator's text, as the key's value, so the body redacted per value differs from
	// main's there.
	contextual bool
}

// bodyValues are the values the corpus draws from: plain names, values that are a secret by their shape, and
// values that name a secret key, with each separator and glue.
func bodyValues() []bodyValue {
	hex40 := strings.Repeat("3f1c9a7b", 5)
	jwt := "eyJhbGciOiJIUzI1NiJ9" + "." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0" + "." + "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	ghp := "ghp_" + strings.Repeat("A", 36)
	pw := "hunter2" + "hunter2x"
	return []bodyValue{
		{text: appName}, {text: workerContainer}, {text: "apps/web/deployment.yaml"}, {text: mainBranch}, {text: "p-2"},
		{text: "default"}, {text: "release-1.2"}, {text: "a.b-c_d"},
		{text: hex40, secret: hex40}, {text: "remediation-" + hex40, secret: hex40}, {text: "apps/" + hex40 + "/x.yaml", secret: hex40},
		{text: jwt, secret: jwt}, {text: ghp, secret: ghp}, {text: "x-" + ghp, secret: ghp},
		{text: "password=" + pw, secret: pw, contextual: true}, {text: "password: " + pw, secret: pw, contextual: true},
		{text: "token=" + pw + ";", secret: pw, contextual: true}, {text: `secret="` + pw + `"`, secret: pw, contextual: true},
		{text: "api_key:" + pw, secret: pw, contextual: true},
		{text: "password=", contextual: true}, {text: "token:", contextual: true}, {text: "secret: |", contextual: true},
	}
}

// objectName is the shape the schema allows for a namespace or an object name.
var objectName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// generatedBody returns a request whose body fields are drawn from values, the values drawn, and whether the
// request and its Incident are named by the derivations the body keeps.
func generatedBody(rng *rand.Rand, values []bodyValue) (rr *remediationv1alpha1.RemediationRequest, incident string,
	drawn []bodyValue) {
	rr = bodyRequest()
	var names []bodyValue
	for _, v := range values {
		if objectName.MatchString(v.text) {
			names = append(names, v)
		}
	}
	drawFrom := func(values []bodyValue) string {
		v := values[rng.IntN(len(values))]
		drawn = append(drawn, v)
		return v.text
	}
	draw := func() string { return drawFrom(values) }
	// Namespaces and names are drawn from the values the schema allows for them: the body shows each pair as
	// one value.
	drawName := func() string { return drawFrom(names) }
	rr.Spec.Action.SetContainerMemoryLimit.Container = draw()
	rr.Spec.TargetRef.Kind = draw()
	rr.Spec.TargetRef.Namespace = drawName()
	rr.Spec.TargetRef.Name = drawName()
	rr.Spec.PolicyRef = remediationv1alpha1.Name(draw())
	rr.Status.Source.Path = remediationv1alpha1.GitFilePath(draw())
	rr.Status.Source.TargetBranch = remediationv1alpha1.GitBranch(draw())
	rr.Status.PolicyRevision = draw()
	if rng.IntN(2) == 0 {
		// The derived names the body keeps.
		incident = "incident-" + strings.Repeat("9f86d081", 8)
		rr.Spec.IncidentRef.Name = incident
		name, err := remediationv1alpha1.RequestName(rr.Spec)
		if err == nil {
			rr.Name = string(name)
		}
	} else {
		rr.Spec.IncidentRef.Name = draw()
		rr.Name = drawName()
	}
	return rr, incident, drawn
}

// TestPullRequestBodyPartsCorpus checks, on generated bodies, that redacting each value on its own hides every
// secret that redacting the composed body hid on main, keeps every kept name main kept, never changes the
// operator's text, hides every value main hid unless that value redacts to itself, and equals main's body when no
// value names a secret key.
func TestPullRequestBodyPartsCorpus(t *testing.T) {
	values := bodyValues()
	var noKeys []bodyValue
	for _, v := range values {
		if !v.contextual {
			noKeys = append(noKeys, v)
		}
	}
	rng := rand.New(rand.NewPCG(963, 1)) // #nosec G404 -- a seeded corpus, not a secret
	n := 400
	if testing.Short() || raceDetector {
		// The race detector makes each case about twenty times slower.
		n = 60
	}
	var equal, contextual, mainLeaked int
	for i := range n {
		pool := values
		if i%2 == 0 {
			pool = noKeys
		}
		rr, incident, drawn := generatedBody(rng, pool)
		body := pullRequestBody(rr, incident, nil)
		old := wholeBody(rr, incident, nil)
		hasContext := false
		for _, v := range drawn {
			if v.secret != "" && strings.Contains(body, v.secret) {
				t.Fatalf("case %d: secret %q reached the body:\n%s", i, v.secret, body)
			}
			hasContext = hasContext || v.contextual
			if v.secret != "" && strings.Contains(old, v.secret) {
				mainLeaked++
			}
		}
		if !skeleton(rr, incident).MatchString(body) {
			t.Fatalf("case %d: the operator's text changed:\n%s", i, body)
		}
		for _, s := range fixedSections {
			if !strings.Contains(body, s) {
				t.Fatalf("case %d: the body lost the fixed text %q:\n%s", i, s, body)
			}
		}
		for _, k := range []string{incident, keptRequestName(rr)} {
			if k != "" && strings.Contains(old, k) && !strings.Contains(body, k) {
				t.Fatalf("case %d: %q, kept on main, is redacted:\n%s", i, k, body)
			}
		}
		if !hasMarker(body, rr) {
			t.Fatalf("case %d: the body lost its marker", i)
		}
		// Main's body against this one: every value main hid that is a secret on its own stays hidden. A value
		// that redacts to itself, as "default" in "default/<token>", was hidden on main only because the token
		// run went on into it; shown now, it shows nothing the redactor takes for a secret.
		for _, v := range drawn {
			if !strings.Contains(old, v.text) && redact.String(v.text) != v.text && strings.Contains(body, v.text) {
				t.Fatalf("case %d: %q, hidden on main, is shown:\n%s", i, v.text, body)
			}
		}
		if hasContext {
			contextual++
			continue
		}
		if body != old {
			t.Fatalf("case %d: with no value naming a secret key, the body differs from main's:\nnow:\n%s\nmain:\n%s",
				i, body, old)
		}
		equal++
	}
	t.Logf("%d bodies without a secret key, all equal to main's, %d with a secret key, %d in which main's body showed a secret", equal,
		contextual, mainLeaked)
	if equal == 0 || contextual == 0 {
		t.Fatalf("the corpus did not reach both kinds: %d equal to main, %d with an open key", equal, contextual)
	}
}

// TestPullRequestBodyWithheldContainerKeepsTheFieldWords: the container name is the only untrusted part of "the
// memory limit of container <name>". Withheld, it stands as the marker after the fixed words, in the Change and
// the Expected effect lines alike, instead of taking the words with it.
func TestPullRequestBodyWithheldContainerKeepsTheFieldWords(t *testing.T) {
	rr := bodyRequest()
	name := rr.Spec.Action.SetContainerMemoryLimit.Container
	body := composeBody(rr, "", nil, func(s string, keep ...string) (string, bool) {
		if s == name {
			return withheldValue, false
		}
		return redactBodyValue(s, keep...)
	})
	if got := strings.Count(body, "the memory limit of container "+withheldValue); got != 2 {
		t.Errorf("the field words with the marker appear %d times, want 2 (Change, Expected effect):\n%s", got, body)
	}
	if !strings.Contains(body, bodyWithheld) {
		t.Errorf("the body does not say a value was withheld:\n%s", body)
	}
	if !skeleton(rr, "").MatchString(strings.Replace(body, bodyWithheld, "", 1)) {
		t.Errorf("the operator's text changed:\n%s", body)
	}
}

// TestPullRequestBodyValueOverItsBudgetIsWithheld: a value of the body whose redaction uses up its work budget
// (#873, decision of 2026-11-21) is told by the redactor's flag, so the body shows the marker in its place and the
// fixed Withheld paragraph, and no text of the value. The body's values are redacted on the lowered work budget of
// redacttest, which withholds OverLowBudget for its work and decides the short values of the body.
func TestPullRequestBodyValueOverItsBudgetIsWithheld(t *testing.T) {
	rr := bodyRequest()
	rr.Status.Source.Path = remediationv1alpha1.GitFilePath(redacttest.OverLowBudget)
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}

	body := composeBody(rr, "", nil, redacttest.LowBudget(red).StringKeepingChecked)

	if !strings.Contains(body, "**Scope:** one value in `"+withheldValue+"` on `"+mainBranch+"`.") {
		t.Errorf("the withheld value is not the marker in its own place:\n%.2000s", body)
	}
	if !strings.Contains(body, bodyWithheld) {
		t.Errorf("the body does not say a value was withheld:\n%.2000s", body)
	}
	if strings.Contains(body, "--token,}--token") {
		t.Errorf("text of the withheld value reached the body")
	}
	if !skeleton(rr, "").MatchString(strings.Replace(body, bodyWithheld, "", 1)) {
		t.Errorf("the operator's text changed:\n%.2000s", body)
	}
}
