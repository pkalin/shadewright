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

package redact_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// This file compiles on origin/main as well (with entrypoints_tools_test.go and a defaultRedactor), so that
// connStringMainTable can be written there: copy both files into a checkout of the commit the table names,
// add a file with defaultRedactor, and run TestWriteConnStringMainTable with CONNSTRING_MAIN_TABLE set to the
// output path. Regenerate the table only from that commit.

// connStringMainTable records, for each input of connStringCorpus and each entry point of connEntryPoints,
// the secret parts that origin/main (b0241d55) kept out of every output.
const connStringMainTable = "testdata/connstring-main.jsonl"

// connCase is one generated text holding a connection string, with the parts of its user and password.
type connCase struct {
	in      string
	secrets []string
}

// The pieces of connStringCorpus. Every special piece is a character the connection-string rule reads, or
// an escape a writer turns it into: raw, JSON, HTML entity, percent and backslash forms.
var (
	connSchemes  = []string{"postgres", "postgresql", "mysql", "mongodb+srv", "redis", "amqps", "https", "sftp"}
	connSpecials = []string{
		"<", ">", "&", `"`, "'", "<<", "><", `<`, `>`, `&`, "&lt;", "&gt;", "&amp;", "&quot;",
		"&#39;", `\"`, "%3C", "%3E",
	}
	connHosts    = []string{"db", "db.prod.svc", "10.0.0.7", "[::1]"}
	connTails    = []string{"", ":5432", ":5432/app", "/app?sslmode=require", ":6379/0"}
	connContexts = []string{
		"%s", "--dsn=%s", "DATABASE_URL=%s", "dsn: %s", `"%s"`, "'%s'", "<%s>", "(%s)", "[%s]",
		"connect to %s failed: timeout", `{"url":"%s"}`, `msg="dial %s"`, "%s,next", "url=%s next=x",
	}
)

// connWord returns a part of a secret: six letters and digits, from its own alphabet so that no part shows
// up in the text around it or in a marker.
func connWord(rng *rand.Rand) string {
	const alphabet = "BCDFGHJKLMNPQRSTVWXZ23456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}

// connSecret builds a user name or password from two or three parts with special pieces at the start, the
// end, between the parts or doubled, and returns it with its parts.
func connSecret(rng *rand.Rand, special bool) (string, []string) {
	one := func() string { return connSpecials[rng.IntN(len(connSpecials))] }
	a, b := connWord(rng), connWord(rng)
	if !special {
		return a + b, []string{a + b}
	}
	switch rng.IntN(5) {
	case 0:
		return one() + a + b, []string{a + b}
	case 1:
		return a + b + one(), []string{a + b}
	case 2:
		return a + one() + one() + b, []string{a, b}
	case 3:
		c := connWord(rng)
		return a + one() + b + one() + c, []string{a, b, c}
	default:
		return a + one() + b, []string{a, b}
	}
}

// connStringCorpus returns n texts, each with one connection string whose user, password or both hold the
// special pieces, in a context: a flag, an environment variable, YAML, quotes, brackets of the markup around
// it, prose, JSON and logfmt. The host gets special pieces too but is not a secret.
func connStringCorpus(n int) []connCase {
	rng := rand.New(rand.NewPCG(840, 4)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	out := make([]connCase, 0, n)
	for range n {
		user, us := connSecret(rng, rng.IntN(2) == 0)
		pass, ps := connSecret(rng, true)
		host := connHosts[rng.IntN(len(connHosts))]
		if rng.IntN(4) == 0 {
			host += connSpecials[rng.IntN(len(connSpecials))] + "x"
		}
		userinfo := user + ":" + pass
		secrets := append(us, ps...) //nolint:gocritic // a new slice; us keeps its length, all case 1 reads of it
		switch rng.IntN(4) {
		case 0: // a user name alone may be a token
			userinfo, secrets = pass, ps
		case 1:
			userinfo, secrets = user+":", us
		}
		url := connSchemes[rng.IntN(len(connSchemes))] + "://" + userinfo + "@" + host +
			connTails[rng.IntN(len(connTails))]
		ctx := connContexts[rng.IntN(len(connContexts))]
		out = append(out, connCase{in: strings.Replace(ctx, "%s", url, 1), secrets: secrets})
	}
	return out
}

// connStringCorpusSize is the number of inputs of the differential test.
const connStringCorpusSize = 4000

// connEntryPoints are the entry points that exist on origin/main as well, plus the redactor itself.
func connEntryPoints() map[string]func(in string) []string {
	return map[string]func(in string) []string{
		"redact.String": func(in string) []string { return []string{redact.String(in)} },
		"fingerprint.Normalize": func(in string) []string {
			return []string{fingerprint.Normalize(fingerprint.Observation{Reason: toolReason, Note: in})}
		},
		"fingerprint.Symptom": func(in string) []string {
			return []string{fingerprint.Symptom(fingerprint.Observation{Reason: toolReason, Note: in})}
		},
		"fingerprint.Value":  func(in string) []string { return []string{fingerprint.Value(in)} },
		"modules.BoundValue": func(in string) []string { return []string{modules.BoundValue(in)} },
		"incidentcontext.Value": func(in string) []string {
			return []string{incidentcontext.Value(redact.String, in)}
		},
		"incidentcontext.Value of the symptom": func(in string) []string {
			return []string{incidentcontext.Value(redact.String, fingerprint.Symptom(fingerprint.Observation{Note: in}))}
		},
		tools.NameGetResource: toolText(tools.NameGetResource,
			map[string]any{argKind: toolKind, argNamespace: toolNS, argName: toolPod}),
		tools.NameListEvents: toolText(tools.NameListEvents,
			map[string]any{argNamespace: toolNS, argKind: toolKind, argName: toolPod, "limit": 5}),
		tools.NameGetPodLogs: toolText(tools.NameGetPodLogs,
			map[string]any{argNamespace: toolNS, "pod": toolPod, "container": toolContainer}),
	}
}

// hiddenParts returns the secrets that no output holds.
func hiddenParts(outs []string, secrets []string) []string {
	hidden := []string{}
	for _, s := range secrets {
		found := false
		for _, o := range outs {
			if strings.Contains(o, s) {
				found = true
				break
			}
		}
		if !found {
			hidden = append(hidden, s)
		}
	}
	return hidden
}

// connMainRow is one line of connStringMainTable: the hash of the input and, per entry point, the secret
// parts no output of it held.
type connMainRow struct {
	Hash   string              `json:"h"`
	Hidden map[string][]string `json:"hidden"`
}

func connHash(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:8])
}

// TestWriteConnStringMainTable writes connStringMainTable from the code it runs with, when
// CONNSTRING_MAIN_TABLE names the output path; otherwise it is skipped.
func TestWriteConnStringMainTable(t *testing.T) {
	path := os.Getenv("CONNSTRING_MAIN_TABLE")
	if path == "" {
		t.Skip("CONNSTRING_MAIN_TABLE is not set")
	}
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	eps := connEntryPoints()
	for _, c := range connStringCorpus(connStringCorpusSize) {
		row := connMainRow{Hash: connHash(c.in), Hidden: map[string][]string{}}
		for name, ep := range eps {
			row.Hidden[name] = hiddenParts(ep(c.in), c.secrets)
		}
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
