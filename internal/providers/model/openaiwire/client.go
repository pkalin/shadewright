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

package openaiwire

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrDialRefused marks a connection refused by Guard.Control.
var ErrDialRefused = errors.New("the address is not a public unicast address")

// PublicControl is the dialer's control: it runs on the resolved address of every connection, so a host name
// that resolves (or is rebound) to an address that is not public unicast gets no connection and no token
// (design 11.1). See publicAddress. It is Guard{}.Control.
func PublicControl(network, address string, c syscall.RawConn) error {
	return Guard{}.Control(network, address, c)
}

// Guard is the address rule of a client's dialer. The zero Guard allows public unicast addresses only.
type Guard struct {
	// AllowPrivateService allows private unicast addresses too, for a Service host the adapter already
	// validated (PrivateServiceControl). InCluster does not apply then.
	AllowPrivateService bool
	// InCluster lists further ranges that count as in-cluster, such as pod or Service ranges outside RFC 1918
	// declared at install (ADR-0042). An address in one is refused like a private address, also when it is
	// embedded in an IPv6 address the way publicAddress reads it.
	InCluster []netip.Prefix
}

// Control runs on the resolved address of every connection the dialer makes, before it connects: every
// address of a name, and again after a rebinding, so nothing of a request reaches a refused address.
func (g Guard) Control(network, address string, c syscall.RawConn) error {
	if g.AllowPrivateService {
		return PrivateServiceControl(network, address, c)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrDialRefused
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !publicAddress(ip, g.InCluster) {
		return ErrDialRefused
	}
	return nil
}

// Dialer returns the guarded dialer of a client: bounded connection setup, Control on every address.
func (g Guard) Dialer() *net.Dialer {
	return &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: g.Control}
}

// publicAddress reports whether ip is a public unicast address. IPv4 (also mapped into IPv6) must be global
// unicast, not private and outside reservedIPv4. An IPv6 address that embeds an IPv4 address the standard way
// (NAT64 64:ff9b::/96, 6to4 2002::/16, IPv4-compatible ::/96) is judged by that IPv4 address, so a DNS64
// answer for a public endpoint still connects while one for 10.0.0.1 does not. Any other IPv6 address must be
// global unicast, not unique-local and outside reservedIPv6, which refuses outright the forms whose IPv4
// cannot be read reliably (local-use NAT64 64:ff9b:1::/48, Teredo 2001::/32, SIIT ::ffff:0:0:0/96). An
// address in inCluster is not public, and neither is an IPv6 address in the forms above whose IPv4 address
// is in inCluster.
func publicAddress(ip netip.Addr, inCluster []netip.Prefix) bool {
	ip = ip.Unmap()
	if inAny(inCluster, ip) {
		return false
	}
	if ip.Is4() {
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !inAny(reservedIPv4, ip)
	}
	b := ip.As16()
	switch {
	case nat64WellKnown.Contains(ip), ipv4Compatible.Contains(ip):
		return publicAddress(netip.AddrFrom4([4]byte(b[12:16])), inCluster)
	case sixToFour.Contains(ip):
		return publicAddress(netip.AddrFrom4([4]byte(b[2:6])), inCluster)
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !inAny(reservedIPv6, ip)
}

func inAny(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Address ranges refused besides those the netip predicates cover (loopback, private, link-local, multicast,
// unspecified, unique-local).
var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	ipv4Compatible = netip.MustParsePrefix("::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
	reservedIPv4   = mustPrefixes(
		"0.0.0.0/8",       // this network
		"100.64.0.0/10",   // shared address space (carrier-grade NAT, some cluster networks)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, and broadcast
	)
	reservedIPv6 = mustPrefixes(
		"::/8",          // reserved, after the embedded forms above: SIIT, local-use NAT64 64:ff9b:1::/48
		"100::/64",      // discard
		"2001::/23",     // IETF protocol assignments, Teredo 2001::/32 included
		"2001:db8::/32", // documentation
		"3fff::/20",     // documentation
		"fec0::/10",     // deprecated site-local
	)
)

func mustPrefixes(s ...string) []netip.Prefix {
	ps := make([]netip.Prefix, len(s))
	for i, p := range s {
		ps[i] = netip.MustParsePrefix(p)
	}
	return ps
}

// NewHTTPClient never follows redirects (the bearer token must reach the configured host only), requires
// TLS 1.2 or later, connects only to the addresses guard allows (Guard.Control, public unicast for the zero
// Guard), and bounds connection setup and the wait for headers (headerTimeout, the attempt timeout of the
// caller). Each attempt has its own deadline on its context.
func NewHTTPClient(headerTimeout time.Duration, guard Guard) *http.Client {
	var rt http.RoundTripper
	{
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		t.TLSHandshakeTimeout = 10 * time.Second
		t.ResponseHeaderTimeout = headerTimeout
		t.DialContext = guard.Dialer().DialContext
		rt = t
	}
	return &http.Client{
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// PrivateServiceControl allows public or private unicast addresses for an explicitly named Service.
// Loopback, metadata/link-local and reserved addresses remain refused, including embedded IPv4 forms.
func PrivateServiceControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrDialRefused
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return ErrDialRefused
	}
	ip = ip.Unmap()
	if ip.IsPrivate() && ip.IsGlobalUnicast() {
		return nil
	}
	if publicAddress(ip, nil) {
		return nil
	}
	return ErrDialRefused
}
