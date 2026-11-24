//go:build redactwork

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

package incidentcontext

import (
	"fmt"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// metricsKeyScene returns sources whose every metric template answers series per template that each carry key as
// a label name (empty value) and their own pod name: the shape of the FuzzDiffBuild input
// testdata/fuzz/FuzzDiffBuild/81e201e983440dcc (a 40,960 byte label name made of repeated key words, a look-alike
// colon and format characters, the same series answered for each of the eight templates the target asks).
func metricsKeyScene(key string, series int) sources {
	s := webSources()
	for _, p := range metricPlans {
		var ss []providers.Series
		for i := range series {
			ss = append(ss, providers.Series{
				Labels:  map[string]string{"pod": fmt.Sprintf("%s-%d", crashPod, i), key: ""},
				Samples: []providers.Sample{{Time: testNow, Value: 1}},
			})
		}
		s.metrics.Set(p.template, ss)
	}
	return s
}

// metricsKey is the label name of the FuzzDiffBuild input, its key words repeated n times.
func metricsKey(n int) string {
	return strings.Repeat("aws_secret_access_key", n) + string([]rune{0xfe55, 0xfeff}) + "\xbf" + string(rune(0x200b))
}

// TestBuildReadsEachValueOnce counts the work of Build on a context whose metric series carry one long label name
// (P3 of #845; the input FuzzDiffBuild timed out on). The decision of a section reads each value with the reading
// of the release before once, as that release read it (two passes, redact.Redactor.ReleasedText); ordering the
// series lines as that release did may read a value once more only where there are lines to order, and once per
// distinct value of an answer, not once per line. The rule's own work stays within its bound per byte and grows
// linearly at n, 2n and 4n.
func TestBuildReadsEachValueOnce(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	templates := 0
	for _, p := range metricPlans {
		if !p.hpa {
			templates++
		}
	}
	workOf := func(key string, series int) (own, released int64) {
		b := newBuilder(t, reader(t, webObjects()...), metricsKeyScene(key, series), nil)
		total, rel := redact.WorkOfReleased(func() { build(t, b, incident(deploymentRef())) })
		return total - rel, rel
	}
	for _, series := range []int{1, 2} {
		baseOwn, baseRel := workOf("k", series)
		var lastOwn int64
		for _, n := range []int{64, 128, 256} {
			key := metricsKey(n)
			_, once := redact.WorkOfReleased(func() { red.ReleasedText(key) })
			own, rel := workOf(key, series)
			occurrences := int64(templates * series)
			// One reading per occurrence (the decision), and one per answer for the order when it has lines to order.
			reads := occurrences
			if series > 1 {
				reads += int64(templates)
			}
			if limit := baseRel + reads*once + reads*once/50; rel > limit {
				t.Errorf("series %d, n %d: the reading of the release before took %d steps, over %d (%d readings of the %d byte label, "+
					"%d steps each): the value is read %.1f times", series, n, rel, limit, reads, len(key), once, float64(rel-baseRel)/float64(once))
			}
			bytes := occurrences * int64(len(key))
			if perByte := (own - baseOwn) / bytes; perByte > 768 {
				t.Errorf("series %d, n %d: own work %d steps per byte of the label, over 768", series, n, perByte)
			}
			if lastOwn > 0 && own-baseOwn > 2*(lastOwn-baseOwn)+(lastOwn-baseOwn)/10 {
				t.Errorf("series %d, n %d: own work %d is more than twice %d at half the size", series, n, own-baseOwn, lastOwn-baseOwn)
			}
			lastOwn = own
		}
	}
}

// gluedFieldsUnit is the unit the label value of the FuzzDiffBuild input bb930588237836fd repeats.
const gluedFieldsUnit = "SECRETPROBE0005TOKEN=\"a\",b,\ufe55"

// buildShapes are the two shapes of #873 as the value of a metric label, which the context builder hands to
// redaction whole: the label value of the FuzzDiffBuild input bb930588237836fd (a key name, a quoted value and
// fields glued to it by commas, repeated), and the staircase of the issue (a block scalar under a bare short key
// on every line, each line one column deeper).
var buildShapes = map[string]func(n int) string{
	"glued fields": func(n int) string { return strings.Repeat(gluedFieldsUnit, n/len(gluedFieldsUnit)+1) },
	"staircase": func(n int) string {
		var b strings.Builder
		for i := 0; b.Len() < n; i++ {
			b.WriteString(strings.Repeat(" ", i) + "pass: |\n")
		}
		return b.String()
	},
}

// buildWorkPerByte bounds the work of a whole Build, per byte of the label value in the answers of the metric
// templates (each answers the series with that value), on the shapes of #873: the bound of package redact on
// one reading (maxWorkPerByte). Measured on the build of #873: 278 to 283 on the glued fields and 136 to 138 on
// the staircase, flat from 64 KiB to 1 MiB; before it, 18,203 on the glued fields at 64 KiB, and 1,075, 2,039
// and 3,969 on the staircase.
const buildWorkPerByte = 1024

// The work of a whole Build on the shapes of #873 stays within buildWorkPerByte per byte of what the metric
// sources answered, at 64 KiB, 256 KiB and 1 MiB, and does not grow with the size (#873).
func TestBuildWorkIsLinear(t *testing.T) {
	templates := 0
	for _, p := range metricPlans {
		if !p.hpa {
			templates++
		}
	}
	for _, name := range []string{"glued fields", "staircase"} {
		first := 0.0
		for _, n := range []int{64 << 10, 256 << 10, 1 << 20} {
			value := buildShapes[name](n)
			s := webSources()
			for _, p := range metricPlans {
				s.metrics.Set(p.template, []providers.Series{{
					Labels:  map[string]string{"pod": crashPod, "key": value},
					Samples: []providers.Sample{{Time: testNow, Value: 1}},
				}})
			}
			b := newBuilder(t, reader(t, webObjects()...), s, nil)
			total, release := redact.WorkOfReleased(func() { build(t, b, incident(deploymentRef())) })
			size := templates * len(value)
			perByte := float64(total) / float64(size)
			t.Logf("%s at %d bytes of value: %.1f steps per byte, the release's reading %.1f", name, len(value), perByte,
				float64(release)/float64(size))
			if perByte > buildWorkPerByte {
				t.Errorf("%s: Build takes %.1f steps per byte at %d bytes of value, over %d", name, perByte, len(value), buildWorkPerByte)
				break
			}
			if first == 0 {
				first = perByte
			} else if perByte > 1.125*first+0.5 {
				t.Errorf("%s: Build takes %.1f steps per byte at %d bytes of value, %.1f at the smallest: more than linear", name,
					perByte, len(value), first)
			}
		}
	}
}

// repeatedLabelWorkPerByte bounds the work of a whole Build per byte of one label name that every metric template
// answers in its one series (the shape of FuzzDiffBuild input 81e201e983440dcc, metricsKey): each expression of
// the rules reads a long window of the label once, alone and in its answer (redact's windows.go), and the
// release's reading reads the label at two passes, not at each of 16 (#873, second round). The test logs the
// steps per byte it measures; only this bound is asserted. Measured 897 before the controls reading and 907 with
// it (a text without controls costs that reading one step per byte at each pass that looks for them); the figure
// is the same at 40 KiB and 400 KiB (864, 860), so the bound has room and the growth stays linear.
//
// The label ends with a byte that is not valid UTF-8, so the reading of the text as it is shown (#1100) reads it
// once more: measured 1013 at 4 KiB and 969 at 40 KiB with it.
const repeatedLabelWorkPerByte = 1100

// The work of a whole Build on one label name repeated by every metric template stays within
// repeatedLabelWorkPerByte per byte of the label at 4 KiB, 40 KiB and 80 KiB (#873, second round). A larger label
// makes the metrics section longer than redact.LargestEntry, which is withheld before any work (#1106).
func TestBuildWorkOfARepeatedLabel(t *testing.T) {
	for _, size := range []int{4 << 10, 40 << 10, 80 << 10} {
		key := metricsKey(size / len("aws_secret_access_key"))
		b := newBuilder(t, reader(t, webObjects()...), metricsKeyScene(key, 1), nil)
		total, release := redact.WorkOfReleased(func() { build(t, b, incident(deploymentRef())) })
		perByte := float64(total) / float64(len(key))
		t.Logf("%d bytes of label: %.0f steps per byte, the release's reading %.0f", len(key), perByte,
			float64(release)/float64(len(key)))
		if total == 0 {
			t.Errorf("a %d byte label: no work was counted, so the section was not decided", len(key))
		}
		if perByte > repeatedLabelWorkPerByte {
			t.Errorf("a %d byte label repeated by every template: Build takes %.0f steps per byte, over %d", len(key),
				perByte, repeatedLabelWorkPerByte)
		}
	}
}

// shortWindowLabel is the label name of FuzzDiffBuild input 22905eb52eae1385 (#873, third round): short key words,
// separators and values repeated, which no expression of the rules reads as one long window.
func shortWindowLabel(n int) string {
	const unit = "TOKEN=\"a\",b,DB_PASSWORD: |+\n  SECRETPROBE0001"
	return strings.Repeat(unit, n/len(unit)+1)
}

// shortWindowsExpressionBytes bounds the bytes a whole Build hands to the regular expressions of the rules, per
// byte of one label name of short windows that every metric template answers in its one series (shortWindowLabel,
// the label of FuzzDiffBuild input 22905eb52eae1385): each expression reads the label alone and the answer that
// writes it in eight lines, and reads a chunk of short windows it read before once (redact's windows.go); before
// #873 every copy was read by each expression. The test logs the bytes per byte it measures; only this bound is
// asserted.
const shortWindowsExpressionBytes = 128

// The bytes a whole Build hands to the regular expressions on one label name of short windows repeated by every
// metric template stay within shortWindowsExpressionBytes per byte of the label at 40 KiB and 400 KiB (#873,
// third round).
func TestBuildWorkOfARepeatedLabelOfShortWindows(t *testing.T) {
	for _, size := range []int{40 << 10, 400 << 10} {
		key := shortWindowLabel(size)
		b := newBuilder(t, reader(t, webObjects()...), metricsKeyScene(key, 1), nil)
		total, release, expr := redact.WorkOfExpressions(func() { build(t, b, incident(deploymentRef())) })
		perByte := float64(expr) / float64(len(key))
		t.Logf("%d bytes of label: %.0f steps per byte, %.0f bytes read by expressions, the release's reading %.0f", len(key),
			float64(total)/float64(len(key)), perByte, float64(release)/float64(len(key)))
		if perByte > shortWindowsExpressionBytes {
			t.Errorf("a %d byte label of short windows repeated by every template: Build hands %.0f bytes per byte of it to "+
				"the expressions, over %d", len(key), perByte, shortWindowsExpressionBytes)
		}
	}
}
