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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/pkalin/shadewright/internal/providers"
)

// fakeResolver answers every A and AAAA question with the given addresses, over an in-process stream, so a
// declared public name resolves to an address of the test's choice without any network.
func fakeResolver(t *testing.T, addrs ...netip.Addr) *net.Resolver {
	t.Helper()
	return &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go serveDNS(server, addrs)
		return client, nil
	}}
}

func serveDNS(c net.Conn, addrs []netip.Addr) {
	defer func() { _ = c.Close() }()
	for {
		var n [2]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(n[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		var m dnsmessage.Message
		if err := m.Unpack(q); err != nil || len(m.Questions) != 1 {
			return
		}
		question := m.Questions[0]
		out := dnsmessage.Message{Header: dnsmessage.Header{ID: m.ID, Response: true, Authoritative: true, RecursionAvailable: true},
			Questions: m.Questions}
		for _, a := range addrs {
			h := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 60}
			switch {
			case a.Is4() && question.Type == dnsmessage.TypeA:
				h.Type = dnsmessage.TypeA
				out.Answers = append(out.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AResource{A: a.As4()}})
			case a.Is6() && question.Type == dnsmessage.TypeAAAA:
				h.Type = dnsmessage.TypeAAAA
				out.Answers = append(out.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
			}
		}
		b, err := out.Pack()
		if err != nil || len(b) > 0xffff {
			return
		}
		binary.BigEndian.PutUint16(n[:], uint16(len(b))) // #nosec G115 -- bounded above
		if _, err := c.Write(append(n[:], b...)); err != nil {
			return
		}
	}
}

// errReached stops a dial the guard allowed, before it connects: the test records the address instead.
var errReached = errors.New("test: the guard allowed this address")

// A declared public endpoint whose name resolves into a declared in-cluster range gets no connection: the
// guard refuses each resolved address before connect, so neither the request nor the key leaves the analyzer.
// Without the ranges the same addresses pass the guard (they are public unicast), which the control case shows.
func TestInClusterRangeRefusedAfterResolution(t *testing.T) {
	ranges := []netip.Prefix{netip.MustParsePrefix("11.0.0.0/8"), netip.MustParsePrefix("2a0b:4000::/32")}
	for _, tc := range []struct {
		name  string
		addrs []string
	}{
		{"ipv4", []string{"11.2.3.4"}},
		{"ipv6", []string{"2a0b:4000::10"}},
		{"both families", []string{"11.2.3.4", "2a0b:4000::10"}},
		{"nat64 of an in-cluster ipv4", []string{"64:ff9b::b02:304"}},
		{"6to4 of an in-cluster ipv4", []string{"2002:b02:304::1"}},
	} {
		for _, declared := range []bool{true, false} {
			t.Run(tc.name+map[bool]string{true: "/declared", false: "/undeclared"}[declared], func(t *testing.T) {
				var addrs []netip.Addr
				for _, a := range tc.addrs {
					addrs = append(addrs, netip.MustParseAddr(a))
				}
				var mu sync.Mutex
				var reached []string
				cfg := Config{Endpoint: "https://gateway.example.net/v1", Deployment: testModel,
					KeyFile: keyFile(t, "marker-834-in-cluster-0001"), MaxAttempts: 1}
				if declared {
					cfg.InClusterRanges = ranges
				}
				p, err := newProvider(cfg, testHooks{
					sleep: func(context.Context, time.Duration) error { return nil },
					dialer: func(d *net.Dialer) {
						guard := d.Control
						d.Resolver = fakeResolver(t, addrs...)
						d.Control = func(network, address string, c syscall.RawConn) error {
							if err := guard(network, address, c); err != nil {
								return err
							}
							mu.Lock()
							defer mu.Unlock()
							reached = append(reached, address)
							return errReached
						}
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				_, err = p.Complete(t.Context(), request())
				mu.Lock()
				defer mu.Unlock()
				if !declared {
					if len(reached) == 0 {
						t.Fatalf("control case: the guard refused a public address without ranges: %v", err)
					}
					return
				}
				if len(reached) > 0 {
					t.Fatalf("the guard allowed in-cluster addresses %v", reached)
				}
				if !errors.Is(err, providers.ErrInvalidConfig) || providers.IsRetryable(err) {
					t.Fatalf("want a final refusal, got %v", err)
				}
				if strings.Contains(err.Error(), "11.2.3.4") || strings.Contains(err.Error(), "gateway") {
					t.Fatalf("the error repeats the address or host: %v", err)
				}
			})
		}
	}
}

func TestParseInClusterRanges(t *testing.T) {
	const notCIDR = "not a CIDR"
	got, err := ParseInClusterRanges("198.51.100.0/24,2001:db8:1::/48,11.0.0.0/8,64:ff9b:1::/48")
	if err != nil || len(got) != 4 || got[1] != netip.MustParsePrefix("2001:db8:1::/48") {
		t.Fatalf("valid ranges: %v %v", got, err)
	}
	if got, err := ParseInClusterRanges(""); err != nil || got != nil {
		t.Fatalf("empty flag: %v %v", got, err)
	}
	for _, tc := range []struct{ raw, reason string }{
		{"11.0.0.0", notCIDR},
		{"11.0.0.0/33", notCIDR},
		{"fe80::%eth0/64", notCIDR},
		{"11.0.0.0/8,", notCIDR},
		{" 11.0.0.0/8", notCIDR},
		{"11.0.0.0/8;12.0.0.0/8", notCIDR},
		{"pods", notCIDR},
		{"0.0.0.0/0", "covers every address"},
		{"::/0", "covers every address"},
		{"11.0.0.1/8", "host bits set: write 11.0.0.0/8"},
		{"2001:db8::1/32", "host bits set: write 2001:db8::/32"},
		{"::ffff:11.0.0.0/104", "::ffff:0:0/96"},
		{"::ffff:b00:0/104", "::ffff:0:0/96"},
		{"64:ff9b::198.51.100.0/120", "64:ff9b::/96"},
		{"64:ff9b::c633:6400/120", "64:ff9b::/96"},
		{"::c633:6400/120", "::/96"},
		{"2002:c633:6400::/40", "2002::/16"},
		{"2001:DB8::/32", "canonical form: write 2001:db8::/32"},
		{"2001:0db8::/32", "canonical form: write 2001:db8::/32"},
		{"1::1:1:1:1:1:1/128", "canonical form: write 1:0:1:1:1:1:1:1/128"},
	} {
		_, err := ParseInClusterRanges(tc.raw)
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%q: %v, want %s", tc.raw, err, tc.reason)
		}
		// The refusal names the value (configuration, not a secret), not only a position.
		if err != nil && !strings.Contains(tc.raw, ",") && !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.raw)) {
			t.Errorf("%q: the refusal does not name the value: %v", tc.raw, err)
		}
	}
	if _, err := ParseInClusterRanges(strings.Repeat("11.0.0.0/8,", MaxInClusterRanges) + "11.0.0.0/8"); err == nil {
		t.Error("more than the maximum accepted")
	}
	for _, p := range []string{"11.0.0.1/8", "0.0.0.0/0", "::ffff:11.0.0.0/104"} {
		if _, err := New(Config{Endpoint: endpoint, Deployment: testModel,
			InClusterRanges: []netip.Prefix{netip.MustParsePrefix(p)}}); !errors.Is(err, providers.ErrInvalidConfig) {
			t.Errorf("New accepted %s: %v", p, err)
		}
	}
}

// Declared hosts are DNS names: an IP literal is refused by the host pattern, so no in-cluster range can hold
// a declared endpoint's own address.
func TestDeclarationRefusesIPLiteralHost(t *testing.T) {
	for _, raw := range []string{"https://198.51.100.7/v1", "https://[2001:db8::7]/v1", "https://[::ffff:c633:6407]/v1"} {
		if err := ValidateEndpoints([]Endpoint{{URL: raw, API: APIChatCompletions, Models: []string{testModel}}}, nil); err == nil {
			t.Errorf("IP literal host accepted: %s", raw)
		}
	}
}
