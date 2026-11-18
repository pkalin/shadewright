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

package openaicompatible

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Endpoint declares one complete credential destination at installation time (ADR-0042).
// URL, API and Models bind the inference destination, including gateway model aliases.
type Endpoint struct {
	// URL is the exact HTTPS API base, including its account path prefix.
	URL string `json:"url"`
	// API is the declared inference wire format.
	API API `json:"inferenceAPI"`
	// Models is the nonempty list of exact request model names allowed at this base.
	Models []string `json:"models"`
	// KeyFile is the optional mounted credential belonging only to URL.
	KeyFile string `json:"keyFile,omitempty"`
	// Service permits private dialing and describes installation egress.
	Service *Service `json:"service,omitempty"`
}

// Service grants private unicast dialing and describes the matching egress rule.
type Service struct {
	// Namespace must equal the namespace in the Service DNS name.
	Namespace string `json:"namespace"`
	// PodSelector selects only the destination pods.
	PodSelector map[string]string `json:"podSelector"`
	// Port is the pod target TCP port of the egress rule.
	Port int `json:"port"`
}

// ParseEndpoints reads the bounded JSON installation flag without exposing its values in errors.
func ParseEndpoints(raw string) ([]Endpoint, error) {
	if len(raw) > 65536 {
		return nil, errors.New("compatible endpoint declarations exceed the size bound")
	}
	var endpoints []Endpoint
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&endpoints); err != nil {
		return nil, errors.New("compatible endpoints must be a JSON array of declarations")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("compatible endpoints have trailing input")
	}
	if err := ValidateEndpoints(endpoints, nil); err != nil {
		return nil, err
	}
	return endpoints, nil
}

// CanonicalBase parses an HTTPS base, normalizing scheme, DNS case, default port and one trailing slash.
// Encoded unreserved characters normalize through parsing. Encoded separators, dot segments,
// ambiguous paths, userinfo, queries and fragments are refused, never cleaned into an allowed base.
func CanonicalBase(raw string) (string, error) {
	invalid := errors.New("compatible endpoint must be an unambiguous HTTPS base URL")
	if len(raw) > 256 || strings.ContainsAny(raw, "?#") {
		return "", invalid
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil || u.Host == "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	if !endpointHostPattern.MatchString(host) {
		return "", invalid
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", invalid
		}
	}
	// No percent-encoded delimiters: a gateway must see the same path boundaries as this parser.
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return "", invalid
	}
	for segment := range strings.SplitSeq(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", invalid
		}
	}
	if !endpointPathPattern.MatchString(u.Path) {
		return "", invalid
	}
	u.Scheme = "https"
	u.Host = host
	if port != "" && port != "443" {
		u.Host = host + ":" + port
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// ValidateEndpoints refuses invalid, overlapping-provider, duplicate or nested-base declarations at analyzer start.
func ValidateEndpoints(endpoints []Endpoint, foundrySuffixes []string) error {
	if len(endpoints) > 16 {
		return errors.New("at most 16 compatible endpoints may be declared")
	}
	seen := map[string]bool{}
	for _, e := range endpoints {
		base, err := CanonicalBase(e.URL)
		if err != nil {
			return err
		}
		u, _ := url.Parse(base)
		host := u.Hostname()
		if ReservedHost(host) {
			return errors.New("compatible endpoint host belongs to Azure or Anthropic")
		}
		for _, s := range foundrySuffixes {
			if host == strings.TrimPrefix(s, ".") || strings.HasSuffix(host, s) {
				return errors.New("compatible endpoint host matches the Foundry suffix list")
			}
		}
		if seen[base] {
			return errors.New("compatible endpoints contain a duplicate base URL")
		}
		for previous := range seen {
			// Canonical HTTPS bases have no query, fragment or trailing slash. A slash
			// boundary therefore compares path segments on the same authority.
			if strings.HasPrefix(base, previous+"/") || strings.HasPrefix(previous, base+"/") {
				return errors.New("compatible endpoint base is a path prefix of another declaration")
			}
		}
		seen[base] = true
		if len(e.Models) == 0 {
			return errors.New("compatible endpoint must declare a nonempty models list")
		}
		for _, model := range e.Models {
			if !deploymentPattern.MatchString(model) {
				return errors.New("compatible endpoint model name is invalid")
			}
		}
		if e.API != APIChatCompletions && e.API != APIResponses {
			return errors.New("compatible endpoint must declare its inference API")
		}
		if e.KeyFile != "" && (!filepath.IsAbs(e.KeyFile) || filepath.Clean(e.KeyFile) != e.KeyFile) {
			return errors.New("compatible endpoint key file must be an absolute clean path")
		}
		if e.Service != nil {
			s := e.Service
			if !serviceHost(host) || s.Namespace != strings.Split(host, ".")[1] || len(s.PodSelector) == 0 || len(s.PodSelector) > 32 || s.Port < 1 || s.Port > 65535 {
				return errors.New("compatible Service needs its DNS namespace, pod selector and target port")
			}
			for k, v := range s.PodSelector {
				if len(validation.IsQualifiedName(k)) > 0 || len(validation.IsValidLabelValue(v)) > 0 {
					return errors.New("compatible Service selector has an invalid label")
				}
			}
		}
	}
	return nil
}

// MatchEndpoint returns only installation-owned data for a settings base and inference API.
func MatchEndpoint(endpoints []Endpoint, raw string, api API) (Endpoint, bool) {
	base, err := CanonicalBase(raw)
	if err != nil {
		return Endpoint{}, false
	}
	if api == "" {
		api = APIChatCompletions
	}
	for _, e := range endpoints {
		declared, err := CanonicalBase(e.URL)
		if err == nil && base == declared && api == e.API {
			return e, true
		}
	}
	return Endpoint{}, false
}

// MaxInClusterRanges bounds the in-cluster ranges an install may declare.
const MaxInClusterRanges = 32

// ParseInClusterRanges reads the comma-separated in-cluster ranges of the installation flag (ADR-0042): the
// pod and Service ranges of a cluster that are outside the private ranges. The dialer of every compatible
// endpoint refuses an address in them like a private address. Each item must pass CheckInClusterRange.
func ParseInClusterRanges(raw string) ([]netip.Prefix, error) {
	if raw == "" {
		return nil, nil
	}
	items := strings.Split(raw, ",")
	if len(items) > MaxInClusterRanges {
		return nil, fmt.Errorf("at most %d in-cluster ranges may be declared", MaxInClusterRanges)
	}
	ranges := make([]netip.Prefix, 0, len(items))
	for _, item := range items {
		p, err := CheckInClusterRange(item)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, p)
	}
	return ranges, nil
}

// embeddedIPv4Forms are the IPv6 ranges whose addresses the dial guard reads as an IPv4 address
// (openaiwire.Guard): IPv4-mapped, IPv4-compatible, the well-known NAT64 prefix and 6to4. A declared IPv4
// range already covers its addresses in these forms, so a range that overlaps them is refused as input.
var embeddedIPv4Forms = []string{"::ffff:0:0/96", "::/96", "64:ff9b::/96", "2002::/16"}

// CheckInClusterRange parses one in-cluster range. The rule, the same as the chart's
// (shadewright.inClusterCIDRProblem): a CIDR in the canonical text form that netip prints (lower case, no
// leading zeros, the longest run of two or more zero groups written as ::), no host bits, not a /0, and no
// IPv6 range that overlaps a form embedding IPv4 addresses (embeddedIPv4Forms): write the IPv4 range instead.
// The error names the value: it is installation configuration, not a secret.
func CheckInClusterRange(item string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(item)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("in-cluster range %q is not a CIDR such as 203.0.113.0/24 or 2001:db8::/64", item)
	}
	if p.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("in-cluster range %q covers every address: no endpoint outside a declared Service could be reached, and the egress rule cannot except it; list the cluster's pod and Service ranges", item)
	}
	if p.Addr().Is6() {
		for _, form := range embeddedIPv4Forms {
			if p.Overlaps(netip.MustParsePrefix(form)) {
				return netip.Prefix{}, fmt.Errorf("in-cluster range %q overlaps %s, a form that embeds IPv4 addresses: write the IPv4 range, which the dial guard also applies to that form", item, form)
			}
		}
	}
	if p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("in-cluster range %q has host bits set: write %s", item, p.Masked())
	}
	if p.String() != item {
		return netip.Prefix{}, fmt.Errorf("in-cluster range %q is not in canonical form: write %s", item, p)
	}
	return p, nil
}

// ValidateInClusterRanges checks parsed ranges against the rule of CheckInClusterRange and MaxInClusterRanges,
// for callers that build the list themselves.
func ValidateInClusterRanges(ranges []netip.Prefix) error {
	if len(ranges) > MaxInClusterRanges {
		return fmt.Errorf("at most %d in-cluster ranges may be declared", MaxInClusterRanges)
	}
	for _, p := range ranges {
		if !p.IsValid() {
			return errors.New("an in-cluster range is not a CIDR")
		}
		if _, err := CheckInClusterRange(p.String()); err != nil {
			return err
		}
	}
	return nil
}

// NamedKeyFile is a key file declared outside the compatible endpoints, such as the Anthropic one.
type NamedKeyFile struct {
	// Name says where the file is declared, for example a flag name; it appears in errors.
	Name string
	// Path is the absolute, clean path of the file.
	Path string
}

// ValidateKeyFiles refuses a key file that belongs to more than one declaration (ADR-0042): two compatible
// endpoints that name one file, or an endpoint that names one of others (the Anthropic key file). Files are
// compared by clean path and, when both exist, by identity (os.SameFile after following links), so a symbolic
// or hard link to the same file counts. A file that does not exist yet is compared by path only; the
// provider checks identity again before each read (Config.OtherKeyFiles, and anthropic.Config.OtherKeyFiles for
// the Anthropic key file). A file whose identity cannot be
// read for another reason stops the start. The error names the two declarations, never a path or content.
func ValidateKeyFiles(endpoints []Endpoint, others []NamedKeyFile) error {
	type named struct {
		name, path string
		compatible bool
	}
	var files []named
	for i, e := range endpoints {
		if e.KeyFile == "" {
			continue
		}
		name := fmt.Sprintf("compatible endpoint %d", i+1)
		if base, err := CanonicalBase(e.URL); err == nil {
			name += " (" + base + ")"
		}
		files = append(files, named{name: name, path: filepath.Clean(e.KeyFile), compatible: true})
	}
	if len(files) == 0 {
		return nil
	}
	for _, o := range others {
		if o.Path != "" {
			files = append(files, named{name: o.Name, path: filepath.Clean(o.Path)})
		}
	}
	infos := make([]os.FileInfo, len(files))
	for i, f := range files {
		st, err := os.Stat(f.path)
		switch {
		case err == nil:
			infos[i] = st
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("the key file of %s cannot be checked against the other declared key files", f.name)
		}
	}
	for i := range files {
		for j := i + 1; j < len(files); j++ {
			if !files[i].compatible && !files[j].compatible {
				continue
			}
			same := files[i].path == files[j].path ||
				infos[i] != nil && infos[j] != nil && os.SameFile(infos[i], infos[j])
			if same {
				return fmt.Errorf("%s and %s name the same key file: a key file belongs to one declaration",
					files[i].name, files[j].name)
			}
		}
	}
	return nil
}
