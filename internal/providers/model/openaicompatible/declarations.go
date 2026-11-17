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
	"net/netip"
	"net/url"
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
// endpoint refuses an address in them like a private address. See ValidateInClusterRanges.
func ParseInClusterRanges(raw string, endpoints []Endpoint) ([]netip.Prefix, error) {
	if raw == "" {
		return nil, nil
	}
	items := strings.Split(raw, ",")
	if len(items) > MaxInClusterRanges {
		return nil, fmt.Errorf("at most %d in-cluster ranges may be declared", MaxInClusterRanges)
	}
	ranges := make([]netip.Prefix, 0, len(items))
	for i, item := range items {
		p, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, fmt.Errorf("in-cluster range %d is not a CIDR such as 203.0.113.0/24 or 2001:db8::/64", i+1)
		}
		ranges = append(ranges, p)
	}
	if err := ValidateInClusterRanges(ranges, endpoints); err != nil {
		return nil, err
	}
	return ranges, nil
}

// ValidateInClusterRanges refuses a range that is not in its canonical network form (host bits set, or an
// IPv4 range written as IPv4-mapped IPv6), more than MaxInClusterRanges, and a range that contains the literal
// address of a declared endpoint, whose every call would then be refused. Declared hosts are DNS names
// (CanonicalBase), so the last check holds for any endpoint that passes ValidateEndpoints and guards against
// a later change of that rule.
func ValidateInClusterRanges(ranges []netip.Prefix, endpoints []Endpoint) error {
	if len(ranges) > MaxInClusterRanges {
		return fmt.Errorf("at most %d in-cluster ranges may be declared", MaxInClusterRanges)
	}
	for i, p := range ranges {
		if !p.IsValid() || p.Addr().Is4In6() || p.Addr().Zone() != "" || p.Masked() != p {
			return fmt.Errorf("in-cluster range %d is not a network address with its prefix length (host bits must be zero; IPv4 ranges in IPv4 form)", i+1)
		}
	}
	for _, e := range endpoints {
		u, err := url.Parse(e.URL)
		if err != nil {
			continue // ValidateEndpoints refuses it.
		}
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil {
			continue
		}
		for i, p := range ranges {
			if p.Contains(ip.Unmap()) {
				return fmt.Errorf("in-cluster range %d contains the address of a declared endpoint", i+1)
			}
		}
	}
	return nil
}
