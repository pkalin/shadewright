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

package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// maskedPassword is what the redactor leaves of "before password=<secret>".
const maskedPassword = "before password=***"

func sample() (Mapping, Candidate) {
	start := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	return Mapping{RepositoryAlias: "payments-repo", ContainerAlias: "worker"}, Candidate{
		IncidentUID: "123e4567-e89b-42d3-a456-426614174000", Fingerprint: "sha256:" + strings.Repeat("a", 64),
		Reason: NonzeroExit, Count: 3, FirstSeen: start, LastSeen: end,
		Workload: Workload{Deployment, "apps", "payments"}, ImageDigest: "sha256:" + strings.Repeat("b", 64),
		MatchedRuleIDs: allRules(),
		Evidence:       []Observation{{NonzeroExit, end}},
		Excerpts: []Excerpt{{CollectedAt: end, Cut: true, Text: "line one\nline two"},
			{CollectedAt: start, Optional: true, Withheld: true, Text: "never release this"}},
		ModelHint: "inspect startup",
	}
}

func encoder(t *testing.T) *Encoder {
	t.Helper()
	enc, err := NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestSnapshotGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	enc := encoder(t)
	for _, test := range []struct {
		name   string
		offset int
	}{{"UTC", 0}, {"same instant in another zone", 3600}} {
		t.Run(test.name, func(t *testing.T) {
			m, c := sample()
			c.FirstSeen = c.FirstSeen.In(time.FixedZone("local", test.offset))
			got, ok := enc.Encode(t.Context(), m, c)
			if !ok || !bytes.Equal(got, want) {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}

func TestSnapshotIdentity(t *testing.T) {
	m, c := sample()
	type field struct {
		name      string
		set       func(string)
		good, bad []string
	}
	name253 := strings.Repeat("abc.", 62) + "abcde"
	alias := []string{"a", "-", "0-api-", strings.Repeat("a", 63)}
	badAlias := []string{"", "API", "a_b", "a.b", "a/b", "api\n", strings.Repeat("a", 64)}
	digest := "sha256:" + strings.Repeat("ab", 32)
	badDigest := []string{digest[7:], digest + "0", digest[:70], strings.ToUpper(digest), "sha256:" + strings.Repeat("z", 64)}
	enc := encoder(t)
	for _, f := range []field{
		{"UID", func(s string) { c.IncidentUID = s }, []string{c.IncidentUID}, []string{"", c.IncidentUID + "0", c.IncidentUID[1:], strings.ReplaceAll(c.IncidentUID, "-", ""), "g" + c.IncidentUID[1:], strings.ToUpper(c.IncidentUID), "123E" + c.IncidentUID[4:]}},
		{"fingerprint", func(s string) { c.Fingerprint = s }, []string{digest}, append(slices.Clone(badDigest), "")},
		{"image digest", func(s string) { c.ImageDigest = s }, []string{"", digest}, badDigest},
		{"reason", func(s string) { c.Reason = Reason(s) }, []string{"NonzeroExit", "ContainerBackoff", "FailedReadiness"}, []string{"", "OOMKilled", "free text"}},
		{"workload kind", func(s string) { c.Workload.Kind = WorkloadKind(s) }, []string{"Deployment", "StatefulSet"}, []string{"", "Pod", "deployment"}},
		{"namespace", func(s string) { c.Workload.Namespace = s }, []string{"a", strings.Repeat("a", 63)}, []string{"", "a.b", "-apps", strings.Repeat("a", 64)}},
		{"workload name", func(s string) { c.Workload.Name = s }, []string{"payments.v2", name253}, []string{"", "Apps", "a..b", name253 + "x"}},
		{"repository alias", func(s string) { m.RepositoryAlias = s }, alias, badAlias},
		{"container alias", func(s string) { m.ContainerAlias = s }, alias, badAlias},
	} {
		t.Run(f.name, func(t *testing.T) {
			for _, ok := range []bool{true, false} {
				values := f.bad
				if ok {
					values = f.good
				}
				for _, v := range values {
					m, c = sample()
					f.set(v)
					body, got := enc.Encode(t.Context(), m, c)
					if got != ok || (body != nil) != ok {
						t.Errorf("%q accepted %t, want %t", v, got, ok)
					}
				}
			}
		})
	}
}

func TestSnapshotRequiredFields(t *testing.T) {
	enc := encoder(t)
	outside := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		edit func(*Candidate)
	}{
		{"no count", func(c *Candidate) { c.Count = 0 }},
		{"missing first time", func(c *Candidate) { c.FirstSeen = time.Time{} }},
		{"invalid last time", func(c *Candidate) { c.LastSeen = outside }},
		{"invalid collection time", func(c *Candidate) { c.Excerpts[0].CollectedAt = outside }},
		{"invalid evidence time", func(c *Candidate) { c.Evidence[0].ObservedAt = outside }},
		{"invalid evidence reason", func(c *Candidate) { c.Evidence[0].Reason = "not a code reason" }},
		{"missing evidence", func(c *Candidate) { c.Evidence = nil }},
		{"no excerpts", func(c *Candidate) { c.Excerpts = nil }},
		{"required excerpt withheld upstream", func(c *Candidate) { c.Excerpts[0].Withheld = true }},
		{"second required excerpt withheld upstream", func(c *Candidate) { c.Excerpts[1].Optional = false }},
		{"second required excerpt invalid UTF-8", func(c *Candidate) {
			c.Excerpts[1] = Excerpt{CollectedAt: c.FirstSeen, Text: "\xff"}
		}},
		{"only optional usable", func(c *Candidate) { c.Excerpts[0].Optional = true }},
		{"blank required excerpt", func(c *Candidate) { c.Excerpts[0].Text = " \t\n" }},
		{"no rules", func(c *Candidate) { c.MatchedRuleIDs = nil }},
		{"unknown rule", func(c *Candidate) { c.MatchedRuleIDs[0] = "free text" }},
		{"third excerpt", func(c *Candidate) { c.Excerpts = append(c.Excerpts, c.Excerpts[0]) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, c := sample()
			tt.edit(&c)
			if body, ok := enc.Encode(t.Context(), m, c); ok || body != nil {
				t.Fatal("released an incomplete snapshot")
			}
		})
	}
}

func TestSnapshotCollections(t *testing.T) {
	enc := encoder(t)
	for _, test := range []struct {
		name string
		edit func(*Mapping, *Candidate)
		ok   bool
	}{
		{"eight distinct rules", func(_ *Mapping, c *Candidate) { c.MatchedRuleIDs = allRules() }, true},
		{"nine rules", func(_ *Mapping, c *Candidate) { c.MatchedRuleIDs = append(allRules(), CurrentFailure) }, false},
		{"duplicate rule", func(_ *Mapping, c *Candidate) { c.MatchedRuleIDs = []RuleID{CurrentFailure, CurrentFailure} }, false},
		{"sixteen lines", func(_ *Mapping, c *Candidate) { c.Evidence = slices.Repeat(c.Evidence, 16) }, true},
		{"seventeen lines", func(_ *Mapping, c *Candidate) { c.Evidence = slices.Repeat(c.Evidence, 17) }, false},
		{"2048 evidence bytes", limitEvidence, true},
		{"2049 evidence bytes", func(m *Mapping, c *Candidate) {
			limitEvidence(m, c)
			c.Evidence[0].ObservedAt = c.Evidence[0].ObservedAt.Add(time.Nanosecond)
		}, false},
		{"oversize required excerpt", func(_ *Mapping, c *Candidate) { c.Excerpts[0].Text = strings.Repeat("x", 4097) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, c := sample()
			test.edit(&m, &c)
			body, ok := enc.Encode(t.Context(), m, c)
			if ok != test.ok || (body != nil) != ok {
				t.Fatalf("accepted %t, want %t", ok, test.ok)
			}
			if test.name == "2048 evidence bytes" && len(strings.Join(contents(t, body).Evidence, "")) != 2048 {
				t.Fatal("wrong evidence total")
			}
		})
	}
}

func allRules() []RuleID {
	return []RuleID{AnalysisEscalated, NoRequest, WorkloadMapped, PolicySourceExcluded, InfrastructureExcluded, DistinctObservations, CurrentFailure, CheckedLogs}
}

func TestSnapshotRuleOrder(t *testing.T) {
	enc := encoder(t)
	m, c := sample()
	c.MatchedRuleIDs = []RuleID{AnalysisEscalated, NoRequest}
	want, ok := enc.Encode(t.Context(), m, c)
	if !ok {
		t.Fatal("withheld valid rules")
	}
	slices.Reverse(c.MatchedRuleIDs)
	got, ok := enc.Encode(t.Context(), m, c)
	if !ok || !bytes.Equal(got, want) {
		t.Fatal("reordering matched rule IDs changed the snapshot")
	}
}

// Each of the sixteen lines has 128 bytes. Its fraction has eight digits, so
// adding one nanosecond to one time adds exactly one byte to the total.
func limitEvidence(m *Mapping, c *Candidate) {
	when := c.LastSeen.Add(123456780 * time.Nanosecond)
	line := "Observed NonzeroExit for container  at " + when.Format(time.RFC3339Nano) + "."
	m.ContainerAlias = strings.Repeat("a", 128-len(line))
	c.Evidence = slices.Repeat([]Observation{{NonzeroExit, when}}, 16)
}

type decoded struct {
	ContainerAlias string
	ModelHint      string
	Evidence       []string
	Excerpts       []struct {
		ContainerAlias, CollectedAt, Text string
		Cut, Withheld                     bool
	}
}

func contents(t *testing.T, body []byte) decoded {
	t.Helper()
	var wire struct{ Untrusted decoded }
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Untrusted
}

func TestSnapshotFreeText(t *testing.T) {
	enc := encoder(t)
	for _, tc := range []struct {
		name, raw, want string
		excerpt         bool
	}{
		{"secret in context", "before password=abcdef", maskedPassword, true},
		{"controls", "left\x00\n\u0085\u2028\u2029right", "leftright", false},
		{"format controls", "left\u202eright", "leftright", false},
		{"joined secret", "before pass\x00word=abcdef", maskedPassword, false},
		{"joined across newline", "before pass\nword=abcdef", maskedPassword, false},
		{"1024 bytes", strings.Repeat("é", 512), strings.Repeat("é", 512), false},
		{"1025 bytes", strings.Repeat("é", 512) + "x", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c := sample()
			c.ModelHint = tc.raw
			if tc.excerpt {
				c.Excerpts[0].Text = tc.raw
			}
			body, ok := enc.Encode(t.Context(), m, c)
			if !ok {
				t.Fatal("withheld snapshot")
			}
			got := contents(t, body)
			if got.ModelHint != tc.want {
				t.Errorf("hint %q, want %q", got.ModelHint, tc.want)
			}
			if tc.excerpt && got.Excerpts[0].Text != tc.want {
				t.Errorf("excerpt %q", got.Excerpts[0].Text)
			}
			if tc.want == "" && bytes.Contains(body, []byte(`"modelHint"`)) {
				t.Fatal("serialized withheld hint")
			}
		})
	}
}

func TestSnapshotExcerptAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		low  bool
	}{
		{"size", 4097, false}, {"work", 4096, true},
	} {
		for _, optional := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: " optional", false: " required"}[optional], func(t *testing.T) {
				enc := encoder(t)
				m, c := sample()
				if tc.low {
					enc.redactor = enc.redactor.WithWorkBudget(65536)
				}
				ex := c.Excerpts[0]
				ex.Text = strings.Repeat("x", tc.size)
				ex.Optional = optional
				if optional {
					c.Excerpts[1] = ex
				} else {
					c.Excerpts[0] = ex
				}
				body, ok := enc.Encode(t.Context(), m, c)
				if ok != optional || (body != nil) != ok {
					t.Fatalf("accepted %t, want %t", ok, optional)
				}
				if optional {
					got := contents(t, body).Excerpts
					if len(got) != 2 || !got[1].Withheld || !got[1].Cut || got[1].Text != "" || got[1].CollectedAt == "" ||
						got[1].ContainerAlias != m.ContainerAlias || bytes.Count(body, []byte(`"text":`)) != 1 {
						t.Fatalf("bad withheld metadata: %+v", got)
					}
				}
			})
		}
	}
}

func TestSnapshotBudget(t *testing.T) {
	enc := encoder(t)
	m, c := sample()
	for _, tt := range []struct {
		name  string
		setup func(*testing.T) context.Context
	}{
		{"cancelled", func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}},
		{"heap admission deadline", func(t *testing.T) context.Context {
			red := enc.redactor
			redacttest.HoldHeapLimit(t, func(block func()) {
				red.PiecesFitting([]redact.Piece{{Text: "x", Data: true}}, func([]string) bool { block(); return true })
			}, func() bool {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				_, ok := red.WithContext(ctx).StringTwiceChecked("x", nil)
				return !ok
			})
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			t.Cleanup(cancel)
			return ctx
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup(t)
			withheld := make(chan bool, 1)
			go func() {
				body, ok := enc.Encode(ctx, m, c)
				withheld <- !ok && body == nil
			}()
			select {
			case ok := <-withheld:
				if !ok {
					t.Fatal("released after deadline")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("redactor is still waiting for heap admission after cancellation")
			}
		})
	}
}

func TestSnapshotWireLimit(t *testing.T) {
	// The ordinary fixture includes the escaped newline. Replacing its excerpt
	// leaves room for a known number of six-byte JSON escapes and ASCII bytes.
	golden, err := os.ReadFile("testdata/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	const old = `line one\nline two`
	room := 16384 - len(golden) + len(old)
	text := strings.Repeat("<", room/6) + strings.Repeat("x", room%6)
	escaped := strings.Repeat(`\u003c`, room/6) + strings.Repeat("x", room%6)
	enc := encoder(t)
	for _, tc := range []struct {
		suffix string
		ok     bool
	}{{"", true}, {"x", false}} {
		m, c := sample()
		c.Excerpts[0].Text = text + tc.suffix
		body, ok := enc.Encode(t.Context(), m, c)
		if ok != tc.ok || (body != nil) != ok {
			t.Fatalf("body size %d, accepted %t, want %t", len(body), ok, tc.ok)
		}
		if ok && (len(body) != 16384 || !bytes.Equal(body, bytes.Replace(golden, []byte(old), []byte(escaped), 1))) {
			t.Fatal("wrong bytes at wire limit")
		}
	}
}

var snapshotFields = map[reflect.Type][]string{
	reflect.TypeFor[payload]():     {"schemaVersion", "verdict", "ruleVersion", "repositoryAlias", "untrusted"},
	reflect.TypeFor[untrusted]():   {"incidentUID", "fingerprint", "reason", "count", "firstSeen", "lastSeen", "workload", "containerAlias", "imageDigest", "matchedRuleIDs", "evidence", "excerpts", "modelHint"},
	reflect.TypeFor[Workload]():    {"kind", "namespace", "name"},
	reflect.TypeFor[wireExcerpt](): {"containerAlias", "collectedAt", "cut", "withheld", "text"},
}

func TestSnapshotClosedSchema(t *testing.T) {
	for typ := range snapshotFields {
		if err := checkSnapshotType(typ); err != nil {
			t.Error(err)
		}
	}
}

func checkSnapshotType(typ reflect.Type, parents ...reflect.Type) error {
	marshaler := reflect.TypeFor[json.Marshaler]()
	if typ.Implements(marshaler) || reflect.PointerTo(typ).Implements(marshaler) {
		return fmt.Errorf("%v implements json.Marshaler", typ)
	}
	if slices.Contains(parents, typ) {
		return fmt.Errorf("recursive snapshot type %v", typ)
	}
	parents = append(parents, typ)
	switch typ.Kind() {
	case reflect.Array, reflect.Slice, reflect.Pointer:
		return checkSnapshotType(typ.Elem(), parents...)
	case reflect.Struct:
		names, ok := snapshotFields[typ]
		if !ok || typ.NumField() != len(names) {
			return fmt.Errorf("unexpected struct or field count for %v", typ)
		}
		for i, want := range names {
			field := typ.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name != want {
				return fmt.Errorf("%v.%s has JSON name %q, want %q", typ, field.Name, name, want)
			}
			if err := checkSnapshotType(field.Type, parents...); err != nil {
				return fmt.Errorf("%v.%s: %w", typ, field.Name, err)
			}
		}
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int64:
	default:
		return fmt.Errorf("unexpected snapshot type %v", typ)
	}
	return nil
}

type customJSONString string

func (*customJSONString) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }

func TestSnapshotRejectsOpenTypes(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[[]any](),
		reflect.TypeFor[[]map[string]string](),
		reflect.TypeFor[*map[string]any](),
		reflect.TypeFor[[2]any](),
		reflect.TypeFor[json.RawMessage](),
		reflect.TypeFor[customJSONString](),
		reflect.TypeFor[[]*struct{ Extra string }](),
	} {
		if checkSnapshotType(typ) == nil {
			t.Errorf("schema accepted %v", typ)
		}
	}
}

func TestSnapshotRequiredEntryWithheld(t *testing.T) {
	enc := encoder(t)
	for _, tc := range []struct {
		name  string
		entry string
	}{
		{"identity", "payments"},
		{"evidence", "Observed NonzeroExit for container worker at 2026-11-25T09:01:00Z."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c := sample()
			called := false
			check := func(s string, cut func(string) int) (string, bool) {
				if s == tc.entry {
					called = true
					return "unfinished entry", false
				}
				return enc.redactor.StringTwiceChecked(s, cut)
			}
			if body, ok := encode(t.Context(), m, c, check); ok || body != nil || !called {
				t.Fatal("required entry was not checked and withheld")
			}
		})
	}
}

// The redactor reports an admission failure with the same flag for every
// string. A failure on optional metadata must discard the unchecked value.
func TestSnapshotOptionalEntryWithheld(t *testing.T) {
	enc := encoder(t)
	for _, tc := range []struct {
		name             string
		entry            func(*Candidate) string
		cancel, omitTime bool
	}{
		{"image digest", func(c *Candidate) string { return c.ImageDigest }, false, false},
		{"optional collection time", func(c *Candidate) string {
			c.Excerpts[1].CollectedAt = c.FirstSeen.Add(-time.Minute)
			c.Excerpts[1].Withheld = false
			return c.Excerpts[1].CollectedAt.Format(time.RFC3339Nano)
		}, false, true},
		{"hint", func(c *Candidate) string { return c.ModelHint }, false, false},
		{"cancel during hint", func(c *Candidate) string { return c.ModelHint }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c := sample()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			value := tc.entry(&c)
			check := func(s string, cut func(string) int) (string, bool) {
				if s == value {
					if tc.cancel {
						cancel()
					}
					return "unfinished entry", false
				}
				return enc.redactor.StringTwiceChecked(s, cut)
			}
			body, ok := encode(ctx, m, c, check)
			if tc.cancel {
				if ok || body != nil {
					t.Fatal("released after cancellation")
				}
				return
			}
			if !ok || bytes.Contains(body, []byte(value)) || bytes.Contains(body, []byte("unfinished entry")) {
				t.Fatal("failed to omit only the withheld field")
			}
			if tc.omitTime {
				got := contents(t, body).Excerpts
				if len(got) != 2 || !got[1].Withheld || got[1].Text != "" || bytes.Count(body, []byte(`"collectedAt"`)) != 1 {
					t.Fatal("unchecked excerpt metadata released")
				}
			}
		})
	}
}

func TestSnapshotContainerReferences(t *testing.T) {
	enc := encoder(t)
	for _, alias := range []string{"sidecar-2", "approved-worker"} {
		t.Run(alias, func(t *testing.T) {
			m, c := sample()
			m.ContainerAlias = alias
			c.Excerpts[0].Text = strings.Repeat("x", 4096)
			c.Excerpts[1] = c.Excerpts[0]
			c.Excerpts[1].Optional = true
			c.Excerpts[1].Text = strings.Repeat("y", 4096)
			body, ok := enc.Encode(t.Context(), m, c)
			if !ok {
				t.Fatal("withheld mapped container")
			}
			got := contents(t, body)
			if got.ContainerAlias != alias || !strings.Contains(got.Evidence[0], "container "+alias+" at ") || len(got.Excerpts) != 2 {
				t.Fatal("mapping missing from evidence or metadata")
			}
			for i, ex := range got.Excerpts {
				if ex.ContainerAlias != alias || ex.Text != c.Excerpts[i].Text || ex.Withheld {
					t.Errorf("wrong excerpt %d for %s", i, alias)
				}
			}
		})
	}
}
