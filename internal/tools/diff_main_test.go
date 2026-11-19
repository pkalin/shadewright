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

package tools

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	eventsv1 "k8s.io/api/events/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/analyzer/refcheck"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
	"github.com/pkalin/shadewright/internal/redact/released"
)

// The read tools of origin/main (87e96974, the release before #845) as far as the differential fuzz targets of
// diff_fuzz_test.go need them: the text each tool rendered and redacted, with the redactor of that release
// (package released) in place of package redact, every call of it recorded (redacttest.Recorder). Each function
// names the origin/main file it comes from; what did not change since (strip, eventTime, eventCount, summary,
// deliveryState, refcheck.Check, incidentcontext.KeepHead and RenderLog with a redactor that has only String) is
// called, not copied.

// mainTools is the redaction of the tools of origin/main, with what it hid recorded.
type mainTools struct {
	rec *redacttest.Recorder
}

// redactorFunc is a Redactor of one function.
type redactorFunc func(string) string

func (f redactorFunc) String(s string) string { return f(s) }

// once is the redactor of origin/main (s.redactor.String there).
func (m mainTools) once(s string) string { return m.rec.Wrap(released.String)(s) }

// redact is Set.redact of origin/main tools.go: incidentcontext.Redact there, redacted, made valid UTF-8 and
// redacted again.
func (m mainTools) redact(s string) string { return m.once(strings.ToValidUTF8(m.once(s), "?")) }

// value is Set.value of origin/main tools.go: incidentcontext.Value there, on redact.
func (m mainTools) value(s string) string {
	return incidentcontext.Shown(m.redact(s))
}

// call is the end of Set.Call of origin/main tools.go: the whole text redacted again and cut to the tool's
// bound, from its end, or for a log excerpt from its oldest lines after the header (FitLog of origin/main
// excerpt.go, which redacted the header and the excerpt before it cut them).
func (m mainTools) call(text string, headLen, maxBytes int) string {
	if headLen > 0 {
		head, tail := m.redact(text[:headLen]), m.redact(text[headLen:])
		out, _, _ := incidentcontext.FitLog(head+tail, len(head), maxBytes)
		return out
	}
	out, _ := incidentcontext.KeepHead(m.redact(text), maxBytes)
	return out
}

// mainLines is the lines type of origin/main tools.go.
type mainLines struct{ b strings.Builder }

func (l *mainLines) add(format string, a ...any) {
	fmt.Fprintf(&l.b, format, a...)
	l.b.WriteByte('\n')
}

// getResource is the text of get_resource on obj in origin/main kube.go (getResource): every string of the
// stripped object redacted as it was read, the object encoded without HTML escapes and indented by one space.
func (m mainTools) getResource(obj client.Object) (string, error) {
	view := reflect.ValueOf(strip(obj))
	mainRedactStrings(view, m.redact)
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(view.Interface()); err != nil {
		return "", err
	}
	return m.call(b.String(), 0, resourceBytes), nil
}

// mainRedactStrings is redactStrings of origin/main kube.go, unchanged.
func mainRedactStrings(v reflect.Value, f func(string) string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		c := reflect.New(v.Elem().Type())
		c.Elem().Set(v.Elem())
		mainRedactStrings(c.Elem(), f)
		if v.CanSet() {
			v.Set(c)
		} else {
			v.Elem().Set(c.Elem())
		}
	case reflect.Struct:
		for _, field := range v.Fields() {
			if field.CanSet() {
				mainRedactStrings(field, f)
			}
		}
	case reflect.Slice:
		if v.IsNil() || !v.CanSet() {
			return
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		reflect.Copy(c, v)
		for i := range c.Len() {
			mainRedactStrings(c.Index(i), f)
		}
		v.Set(c)
	case reflect.Map:
		if v.IsNil() || !v.CanSet() {
			return
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(it.Value())
			mainRedactStrings(e, f)
			c.SetMapIndex(it.Key(), e)
		}
		v.Set(c)
	case reflect.String:
		if v.CanSet() {
			v.SetString(f(v.String()))
		}
	default:
	}
}

// listEvents is the text of list_events in origin/main kube.go (listEvents), for the events of the namespace and
// limit events at most.
func (m mainTools) listEvents(items []eventsv1.Event, limit int) string {
	evs := make([]*eventsv1.Event, 0, len(items))
	for i := range items {
		evs = append(evs, &items[i])
	}
	slices.SortStableFunc(evs, func(x, y *eventsv1.Event) int {
		return cmp.Or(eventTime(y).Compare(eventTime(x)), cmp.Compare(x.Name, y.Name))
	})
	var l mainLines
	l.add("%d events match; the newest %d are shown", len(evs), min(len(evs), limit))
	for _, e := range evs[:min(len(evs), limit)] {
		l.add("%s %s %s %s/%s count %d: %s", eventTime(e).UTC().Format(time.RFC3339), m.value(e.Type),
			m.value(e.Reason), m.value(e.Regarding.Kind), m.value(e.Regarding.Name), eventCount(e), m.value(e.Note))
	}
	return m.call(l.b.String(), 0, eventsBytes)
}

// checkReference is the text of check_missing_reference in origin/main kube.go (checkReference).
func (m mainTools) checkReference(res refcheck.Result) string {
	var l mainLines
	l.add("answer: %s", res.Answer)
	l.add("referenced by the pod spec: %s", strconv.FormatBool(res.Referenced))
	if res.Answer == refcheck.Missing {
		l.add("what is missing: %s", res.What)
		l.add("evidence: %s", res.Source)
		if res.Key != "" {
			l.add("key: %s", m.value(res.Key))
		}
		if res.Container != "" {
			l.add("container: %s", m.value(res.Container))
		}
		if res.Volume != "" {
			l.add("volume: %s", m.value(res.Volume))
		}
	}
	return m.call(l.b.String(), 0, refcheckBytes)
}

// queryMetrics is the text of query_metrics in origin/main sources.go (queryMetrics).
func (m mainTools) queryMetrics(t metricTemplate, series []providers.Series) string {
	var l mainLines
	l.add("%s: %d series", t.name, len(series))
	for _, se := range series {
		var lb []string
		for _, k := range slices.Sorted(maps.Keys(se.Labels)) {
			lb = append(lb, m.value(k)+"="+m.value(se.Labels[k]))
		}
		l.add("%s", "{"+strings.Join(lb, ", ")+"} "+summary(se.Samples, t.overWindow))
	}
	return m.call(l.b.String(), 0, metricsBytes)
}

// deliveryStatus is the text of get_delivery_status in origin/main sources.go (deliveryStatus).
func (m mainTools) deliveryStatus(st providers.DeliveryStatus) string {
	var l mainLines
	l.add("delivery: %s", deliveryState(st))
	l.add("applied revision: %s", m.value(st.AppliedRevision))
	l.add("ready: %s", strconv.FormatBool(st.Ready))
	l.add("message: %s", m.value(st.Message))
	return m.call(l.b.String(), 0, deliveryBytes)
}

// podLogs is the text of get_pod_logs in origin/main sources.go (podLogs) for the excerpt ex: the header and the
// excerpt rendered and redacted (RenderLog of origin/main excerpt.go, which is RenderLog now with a redactor that
// has only String and no refit), then fitted again by Call.
func (m mainTools) podLogs(pod, ctr string, previous bool, ex providers.LogExcerpt, maxBytes int) string {
	instance := "current instance"
	if previous {
		instance = "previous instance"
	}
	title := "pod " + m.value(pod) + " container " + m.value(ctr) + ", " + instance
	v := incidentcontext.RenderLog(redactorFunc(m.redact), false, title, ex, nil, maxBytes, logBytes)
	return m.call(v.Text, v.HeadLen, logBytes)
}

// mainExcerpt is Excerpt of origin/main internal/providers/logs/kube/kube.go, with the redactor of that release:
// the whole of data redacted with the part before the cut dropped, made valid UTF-8 and redacted again.
func (m mainTools) excerpt(data []byte, endMissing bool, keep int) (string, bool) {
	if endMissing {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	kept, truncated := mainCut(data, false, keep)
	from := len(data) - len(kept)
	pos := 0
	part := func(n int, f func(start int) string) string {
		start := pos
		pos += n
		if pos <= from {
			return ""
		}
		return f(max(from-start, 0))
	}
	text := released.Replace(string(data),
		func(kind, s string) string {
			return part(len(s), func(skip int) string { return released.Marker(kind, len(s)-skip) })
		},
		func(seg string) string { return part(len(seg), func(skip int) string { return seg[skip:] }) })
	// The first pass's hiding is recorded on the part kept.
	m.rec.Saw(kept, text)
	return m.once(strings.ToValidUTF8(text, "?")), truncated
}

// mainCut is cut of internal/providers/logs/kube/kube.go, unchanged since origin/main 87e96974.
func mainCut(data []byte, endMissing bool, keep int) (string, bool) {
	if endMissing {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	if len(data) <= keep {
		return string(data), false
	}
	from := len(data) - keep
	if data[from-1] != '\n' {
		i := bytes.IndexByte(data[from:], '\n')
		if i < 0 {
			return "", true
		}
		from += i + 1
	}
	return string(data[from:]), true
}
