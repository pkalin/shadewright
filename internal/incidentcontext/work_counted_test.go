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

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
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
