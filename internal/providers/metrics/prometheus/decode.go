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

package prometheus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// errBodyTooLarge is returned by capReader once more than the cap has been read.
var errBodyTooLarge = errors.New("body too large")

// capReader fails as soon as more than left bytes have been read, instead of truncating silently.
// It records the first read error of the underlying reader other than io.EOF, so that a body cut short by
// the connection can be told apart from a malformed one.
type capReader struct {
	r       io.Reader
	left    int64
	readErr error
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errBodyTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return 0, errBodyTooLarge
	}
	if err != nil && !errors.Is(err, io.EOF) && c.readErr == nil {
		c.readErr = err
	}
	return n, err
}

// labelValue is what a label value of a result must look like: a plain name, never free text.
var labelValue = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,253}$`)

// maxSkipDepth bounds the nesting of an unknown field that is skipped.
const maxSkipDepth = 16

// windowSlackMs is how far a sample time may lie outside the time asked for, for rounding by the server.
const windowSlackMs = 1000

// decoder walks a query response token by token and enforces every limit while it reads.
type decoder struct {
	dec    *json.Decoder
	win    window
	labels []string
	points int
	series []providers.Series
	// keys holds the labels of each series read, the cluster label left out, to refuse a second series of the
	// same container from another cluster.
	keys map[string]struct{}
	// vector and matrix record which element shapes were seen ("value" and "values").
	vector, matrix bool
	// partial records a warning that the data may be incomplete; the warning text is never kept.
	partial bool
}

// decode reads a response of the Prometheus query API. A successful response that carries warnings is
// returned as Partial.
func decode(r io.Reader, win window, labels []string) (providers.MetricsResult, error) {
	d := &decoder{dec: json.NewDecoder(r), win: win, labels: labels, keys: map[string]struct{}{}}
	d.dec.UseNumber()
	err := d.top()
	switch {
	case errors.Is(err, errBodyTooLarge):
		return providers.MetricsResult{}, fmt.Errorf("prometheus response is larger than %d bytes: %w", MaxBodyBytes,
			providers.ErrTooLarge)
	case errors.Is(err, providers.ErrTooLarge), errors.Is(err, ErrInvalidResponse):
		return providers.MetricsResult{}, err
	case err != nil:
		// A syntax error or a failed read: the text of either may quote the response.
		return providers.MetricsResult{}, fmt.Errorf("prometheus response cannot be decoded: %w", ErrInvalidResponse)
	}
	return providers.MetricsResult{Series: d.series, Partial: d.partial}, nil
}

func invalid(msg string) error {
	return fmt.Errorf("prometheus response %s: %w", msg, ErrInvalidResponse)
}

func (d *decoder) top() error {
	if err := d.delim('{'); err != nil {
		return err
	}
	var status, resultType string
	var sawData bool
	seen := map[string]bool{}
	for d.dec.More() {
		key, err := d.key(seen)
		if err != nil {
			return err
		}
		switch key {
		case "status":
			if status, err = d.str(); err != nil {
				return err
			}
		case "data":
			sawData = true
			if resultType, err = d.data(); err != nil {
				return err
			}
		case "warnings":
			if d.partial, err = d.warns(); err != nil {
				return err
			}
		default: // infos, errorType, error: never returned. Infos annotate the query, they do not cut the data.
			if err := d.skip(0); err != nil {
				return err
			}
		}
	}
	if err := d.delim('}'); err != nil {
		return err
	}
	if _, err := d.dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return invalid("has data after the end")
	}
	if status != "success" || !sawData {
		return invalid("is not a successful result with data")
	}
	// The element shape must match the declared type, whichever came first in the object.
	switch {
	case d.win.instant && resultType == "vector" && !d.matrix:
	case !d.win.instant && resultType == "matrix" && !d.vector:
	default:
		return invalid("is not of the expected result type")
	}
	return nil
}

func (d *decoder) data() (string, error) {
	if err := d.delim('{'); err != nil {
		return "", err
	}
	var resultType string
	var sawResult bool
	seen := map[string]bool{}
	for d.dec.More() {
		key, err := d.key(seen)
		if err != nil {
			return "", err
		}
		switch key {
		case "resultType":
			if resultType, err = d.str(); err != nil {
				return "", err
			}
		case "result":
			sawResult = true
			if err := d.result(); err != nil {
				return "", err
			}
		default:
			if err := d.skip(0); err != nil {
				return "", err
			}
		}
	}
	if !sawResult {
		return "", invalid("has no result")
	}
	return resultType, d.delim('}')
}

func (d *decoder) result() error {
	if err := d.delim('['); err != nil {
		return err
	}
	for d.dec.More() {
		if len(d.series) == MaxSeries {
			return fmt.Errorf("prometheus response has more than %d series: %w", MaxSeries, providers.ErrTooLarge)
		}
		s, err := d.element()
		if err != nil {
			return err
		}
		// Every template keeps the cluster label apart and metric drops it, so two series with the same labels
		// are one container in two clusters (or a source that labels only some of its series): refused rather
		// than returned as two answers for one container.
		key := seriesKey(s.Labels)
		if _, dup := d.keys[key]; dup {
			return invalid("has two series of one container from different clusters")
		}
		d.keys[key] = struct{}{}
		d.series = append(d.series, s)
	}
	return d.delim(']')
}

// element reads one series: {"metric": {...}, "values": [[t, "v"], ...]} or {"metric": {...}, "value": [t, "v"]}.
func (d *decoder) element() (providers.Series, error) {
	if err := d.delim('{'); err != nil {
		return providers.Series{}, err
	}
	var s providers.Series
	var sawMetric, sawSamples bool
	seen := map[string]bool{}
	for d.dec.More() {
		key, err := d.key(seen)
		if err != nil {
			return s, err
		}
		switch key {
		case "metric":
			sawMetric = true
			if s.Labels, err = d.metric(); err != nil {
				return s, err
			}
		case "values":
			d.matrix, sawSamples = true, true
			if s.Samples, err = d.values(); err != nil {
				return s, err
			}
		case "value":
			// One sample per series: MaxSeries bounds these, well below MaxPoints.
			d.vector, sawSamples = true, true
			p, err := d.sample()
			if err != nil {
				return s, err
			}
			d.points++
			s.Samples = []providers.Sample{p}
		default: // "histogram", "histograms" and anything else: no template returns them.
			return s, invalid("has an unexpected field in a series")
		}
	}
	if !sawMetric || !sawSamples {
		return s, invalid("has a series without labels or samples")
	}
	return s, d.delim('}')
}

func (d *decoder) metric() (map[string]string, error) {
	if err := d.delim('{'); err != nil {
		return nil, err
	}
	labels := map[string]string{}
	var sawCluster bool
	for d.dec.More() {
		key, err := d.key(nil)
		if err != nil {
			return nil, err
		}
		if key == clusterLabel {
			// Kept apart by every template, checked, and dropped: the caller asked about one cluster.
			v, err := d.str()
			if err != nil {
				return nil, err
			}
			if sawCluster || !clusterValue.MatchString(v) {
				return nil, invalid("has a cluster label that is repeated or not a plain name")
			}
			sawCluster = true
			continue
		}
		if !slices.Contains(d.labels, key) {
			return nil, invalid("has a label the template does not produce")
		}
		if _, dup := labels[key]; dup {
			return nil, invalid("repeats a label")
		}
		v, err := d.str()
		if err != nil {
			return nil, err
		}
		if !labelValue.MatchString(v) {
			return nil, invalid("has a label value that is not a plain name")
		}
		labels[key] = v
	}
	return labels, d.delim('}')
}

// seriesKey is a canonical form of a label set. Names and values are plain names, so neither holds the
// separators.
func seriesKey(labels map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		b.WriteString(k + "\x00" + labels[k] + "\x01")
	}
	return b.String()
}

func (d *decoder) values() ([]providers.Sample, error) {
	if err := d.delim('['); err != nil {
		return nil, err
	}
	var out []providers.Sample
	for d.dec.More() {
		if len(out) == MaxPointsPerSeries {
			return nil, fmt.Errorf("prometheus response has more than %d points in a series: %w",
				MaxPointsPerSeries, providers.ErrTooLarge)
		}
		if d.points == MaxPoints {
			return nil, fmt.Errorf("prometheus response has more than %d points: %w", MaxPoints, providers.ErrTooLarge)
		}
		p, err := d.sample()
		if err != nil {
			return nil, err
		}
		if len(out) > 0 && !p.Time.After(out[len(out)-1].Time) {
			return nil, invalid("has samples out of order")
		}
		d.points++
		out = append(out, p)
	}
	return out, d.delim(']')
}

// sample reads [<seconds>, "<value>"] and checks the time against the window.
func (d *decoder) sample() (providers.Sample, error) {
	if err := d.delim('['); err != nil {
		return providers.Sample{}, err
	}
	tok, err := d.dec.Token()
	if err != nil {
		return providers.Sample{}, err
	}
	num, ok := tok.(json.Number)
	if !ok {
		return providers.Sample{}, invalid("has a sample time that is not a number")
	}
	secs, err := num.Float64()
	if err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return providers.Sample{}, invalid("has a sample time that is not a number")
	}
	ms := math.Round(secs * 1000)
	if ms < float64(d.win.fromMs-windowSlackMs) || ms > float64(d.win.toMs+windowSlackMs) {
		return providers.Sample{}, invalid("has a sample outside the time asked for")
	}
	raw, err := d.str()
	if err != nil {
		return providers.Sample{}, err
	}
	// ParseFloat accepts "NaN", "+Inf" and "-Inf", which Prometheus sends.
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return providers.Sample{}, invalid("has a sample value that is not a number")
	}
	if err := d.delim(']'); err != nil {
		return providers.Sample{}, err
	}
	return providers.Sample{Time: time.UnixMilli(int64(ms)).UTC(), Value: v}, nil
}

// key reads an object key. With seen, a repeated key is refused, so a later duplicate cannot replace a
// checked value.
func (d *decoder) key(seen map[string]bool) (string, error) {
	tok, err := d.dec.Token()
	if err != nil {
		return "", err
	}
	k, ok := tok.(string)
	if !ok {
		return "", invalid("is malformed")
	}
	if seen != nil {
		if seen[k] {
			return "", invalid("repeats a field")
		}
		seen[k] = true
	}
	return k, nil
}

func (d *decoder) str() (string, error) {
	tok, err := d.dec.Token()
	if err != nil {
		return "", err
	}
	s, ok := tok.(string)
	if !ok {
		return "", invalid("has a value of the wrong type")
	}
	return s, nil
}

func (d *decoder) delim(want json.Delim) error {
	tok, err := d.dec.Token()
	if err != nil {
		return err
	}
	if got, ok := tok.(json.Delim); !ok || got != want {
		return invalid("is malformed")
	}
	return nil
}

// infoPrefix starts the PromQL annotations that older Prometheus releases put into warnings, which later
// releases send as infos (for example "PromQL info: metric might not be a counter"). They describe the query,
// not missing data.
const infoPrefix = "PromQL info: "

// warns reads the warnings field and reports whether it says the data may be incomplete: any entry that is not
// an info annotation. null and an empty list say nothing. A value of another shape (from a proxy that does not
// follow the API) is taken as a warning, never refused, so a usable answer is kept but marked. The text is
// compared with a fixed prefix only and never kept.
func (d *decoder) warns() (bool, error) {
	tok, err := d.dec.Token()
	if err != nil {
		return false, err
	}
	switch tok {
	case nil:
		return false, nil
	case json.Delim('['):
	case json.Delim('{'):
		return true, d.skipRest('{', 1)
	default:
		return true, nil
	}
	partial := false
	for d.dec.More() {
		tok, err := d.dec.Token()
		if err != nil {
			return false, err
		}
		switch v := tok.(type) {
		case string:
			partial = partial || !strings.HasPrefix(v, infoPrefix)
		case json.Delim:
			partial = true
			if err := d.skipRest(v, 1); err != nil {
				return false, err
			}
		default:
			partial = true
		}
	}
	_, err = d.dec.Token()
	return partial, err
}

// skip reads and drops one value, refusing nesting deeper than maxSkipDepth.
func (d *decoder) skip(depth int) error {
	tok, err := d.dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	return d.skipRest(delim, depth)
}

// skipRest drops the rest of an object or array whose opening delimiter was read at the given depth.
func (d *decoder) skipRest(delim json.Delim, depth int) error {
	if delim != '{' && delim != '[' {
		return invalid("is malformed")
	}
	if depth == maxSkipDepth {
		return invalid("is nested too deeply")
	}
	for d.dec.More() {
		if delim == '{' {
			if _, err := d.key(nil); err != nil {
				return err
			}
		}
		if err := d.skip(depth + 1); err != nil {
			return err
		}
	}
	_, err := d.dec.Token()
	return err
}
