package netpolicy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

// lastAddr returns the highest address of p.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := range b {
		bit := i * 8
		switch {
		case bit >= p.Bits():
			b[i] = 0xff
		case bit+8 > p.Bits():
			b[i] |= 0xff >> (p.Bits() - bit)
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

func TestDefaultRefusesEveryBuiltinBlockEdge(t *testing.T) {
	t.Parallel()
	p := Default()
	for _, block := range append(append([]netip.Prefix{}, nonGlobalIPv4...), nonGlobalIPv6...) {
		for _, a := range []netip.Addr{block.Addr(), lastAddr(block)} {
			if p.Allowed(a) {
				t.Errorf("%s (edge of %s) allowed, want refused", a, block)
			}
		}
	}
}

func TestDefault(t *testing.T) {
	t.Parallel()
	p := Default()
	cases := map[string]bool{
		// public neighbours of refused blocks
		"1.1.1.1":         true,
		"9.255.255.255":   true,
		"11.0.0.0":        true,
		"100.63.255.255":  true,
		"100.128.0.0":     true,
		"172.15.255.255":  true,
		"172.32.0.0":      true,
		"192.0.1.255":     true,
		"192.0.3.0":       true,
		"192.167.255.255": true,
		"192.169.0.0":     true,
		"198.17.255.255":  true,
		"198.20.0.0":      true,
		"223.255.255.255": true,
		// refused
		"0.1.2.3":         false,
		"10.1.2.3":        false,
		"100.64.0.1":      false,
		"100.127.255.254": false,
		"127.0.0.1":       false,
		"169.254.1.1":     false,
		"172.16.0.1":      false,
		"192.168.1.1":     false,
		"198.19.1.1":      false,
		"224.0.0.1":       false,
		"240.0.0.1":       false,
		"255.255.255.255": false,
		// IPv6
		"2a00:1450:4001::1":    true,
		"2620:4f:8000::1":      true,
		"2001:200::1":          true, // first block after 2001::/23
		"::":                   false,
		"::1":                  false,
		"::10.0.0.1":           false, // deprecated IPv4-compatible
		"64:ff9b:1::1":         false,
		"100::1":               false,
		"5f00::1":              false,
		"fc00::1":              false,
		"fd12:3456::1":         false,
		"fe80::1":              false,
		"fe80::1%eth0":         false,
		"fec0::1":              false,
		"ff02::1":              false,
		"2001::1":              false, // Teredo
		"2001:db8::1":          false,
		"3fff::1":              false,
		"4000::1":              false, // outside 2000::/3
		"::ffff:10.0.0.1":      false, // IPv4-mapped, unmapped first
		"::ffff:8.8.8.8":       true,
		"64:ff9b::8.8.8.8":     true,
		"64:ff9b::10.0.0.1":    false,
		"64:ff9b::100.64.0.1":  false,
		"2002:0808:0808::1":    true,  // 6to4 of 8.8.8.8
		"2002:c0a8:0101::1":    false, // 6to4 of 192.168.1.1
		"2002:7f00:0001::1":    false, // 6to4 of 127.0.0.1
		"2002:6440:0001::1":    false, // 6to4 of 100.64.0.1
		"2003:e0:1234::1":      true,
		"2a01:4f8:c0c:1234::2": true,
	}
	for s, want := range cases {
		if got := p.Allowed(netip.MustParseAddr(s)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	t.Parallel()
	p := New(Options{AllowNonGlobal: true})
	if p.Allowed(netip.Addr{}) {
		t.Error("zero Addr allowed")
	}
	if p.AllowedIP(nil) || p.AllowedIP(net.IP{1, 2, 3}) {
		t.Error("nil or malformed net.IP allowed")
	}
	if !p.AllowedIP(net.ParseIP("10.0.0.1")) { // 16-byte form of an IPv4 address
		t.Error("16-byte IPv4 net.IP refused with AllowNonGlobal")
	}
}

func TestHardFloorSurvivesOptOutAndAllowlist(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]*Policy{
		"opt-out":   New(Options{AllowNonGlobal: true}),
		"allowlist": New(Options{Allow: mustPrefixes("0.0.0.0/0", "::/0")}),
	} {
		for _, s := range []string{"0.0.0.0", "::", "224.0.0.1", "239.255.255.250", "ff02::1", "255.255.255.255"} {
			if p.Allowed(netip.MustParseAddr(s)) {
				t.Errorf("%s: %s allowed, want refused", name, s)
			}
		}
	}
	// Only the limited broadcast address is recognised; a subnet-directed
	// broadcast of a public network is indistinguishable from unicast.
	if !Default().Allowed(netip.MustParseAddr("8.8.8.255")) {
		t.Error("8.8.8.255 refused; only 255.255.255.255 is treated as broadcast")
	}
	optOut := New(Options{AllowNonGlobal: true})
	for _, s := range []string{"10.0.0.1", "127.0.0.1", "::1", "100.64.0.1"} {
		if !optOut.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("opt-out: %s refused, want allowed", s)
		}
	}
}

func TestAllowlistMode(t *testing.T) {
	t.Parallel()
	p := New(Options{Allow: mustPrefixes("192.168.1.0/24", "::ffff:10.9.0.0/112")})
	cases := map[string]bool{
		"192.168.1.10": true,
		"192.168.2.10": false,
		"10.9.1.1":     true, // mapped allow prefix normalized to 10.9.0.0/16
		"8.8.8.8":      false,
	}
	for s, want := range cases {
		if got := p.Allowed(netip.MustParseAddr(s)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestDenyWins(t *testing.T) {
	t.Parallel()
	deny := mustPrefixes("8.8.8.0/24", "2a00:1450::/32")
	for name, p := range map[string]*Policy{
		"default":   New(Options{Deny: deny}),
		"opt-out":   New(Options{Deny: deny, AllowNonGlobal: true}),
		"allowlist": New(Options{Deny: deny, Allow: mustPrefixes("8.0.0.0/8")}),
	} {
		for _, s := range []string{"8.8.8.8", "::ffff:8.8.8.8", "64:ff9b::8.8.8.8", "2002:0808:0808::1"} {
			if p.Allowed(netip.MustParseAddr(s)) {
				t.Errorf("%s: %s allowed, want refused", name, s)
			}
		}
		if name != "allowlist" && p.Allowed(netip.MustParseAddr("2a00:1450:4001::1")) {
			t.Errorf("%s: denied IPv6 allowed", name)
		}
	}
	if !New(Options{Deny: deny}).Allowed(netip.MustParseAddr("8.8.9.8")) {
		t.Error("address outside the deny list refused")
	}
}

func TestCounters(t *testing.T) {
	t.Parallel()
	p := Default()
	if !p.AdmitLearned(net.ParseIP("8.8.8.8")) || p.AdmitLearned(net.ParseIP("10.0.0.1")) {
		t.Fatal("AdmitLearned decisions wrong")
	}
	if err := p.CheckUDP(net.ParseIP("8.8.8.8")); err != nil {
		t.Fatalf("CheckUDP(public) = %v", err)
	}
	if err := p.CheckUDP(net.ParseIP("100.64.0.1")); !errors.Is(err, ErrRefused) {
		t.Fatalf("CheckUDP(CGNAT) = %v, want ErrRefused", err)
	}
	if err := p.ControlContext(context.Background(), "tcp4", "8.8.8.8:6881", nil); err != nil {
		t.Fatalf("ControlContext(public) = %v", err)
	}
	for _, addr := range []string{"127.0.0.1:6881", "[fe80::1%eth0]:6881", "garbage"} {
		if err := p.ControlContext(context.Background(), "tcp", addr, nil); !errors.Is(err, ErrRefused) {
			t.Fatalf("ControlContext(%s) = %v, want ErrRefused", addr, err)
		}
	}
	if l, u, tc := p.Counts(); l != 1 || u != 1 || tc != 3 {
		t.Fatalf("Counts() = %d, %d, %d; want 1, 1, 3", l, u, tc)
	}
	if got, want := p.Summary(), "egress refused learned=1 udp=1 tcp=3"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

func TestParsePrefixes(t *testing.T) {
	t.Parallel()
	got, err := ParsePrefixes([]string{"", " 10.1.2.3/8 ", "203.0.113.7", "::ffff:192.0.2.0/120", "2001:db8::1/32"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "203.0.113.7/32", "192.0.2.0/24", "2001:db8::/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"10.0.0.0/33", "not-an-ip", "fe80::1%eth0", "::ffff:0:0/80"} {
		if _, err := ParsePrefixes([]string{bad}); err == nil {
			t.Errorf("ParsePrefixes(%q) succeeded, want an error", bad)
		}
	}
}

func TestPrefixesFromIPNets(t *testing.T) {
	t.Parallel()
	var nets []net.IPNet
	for _, s := range []string{"192.168.1.0/24", "fd00::/8"} {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		nets = append(nets, *n)
	}
	// A 16-byte IPv4 IP with a 32-bit mask, as some callers construct.
	nets = append(nets, net.IPNet{IP: net.ParseIP("10.0.0.0"), Mask: net.CIDRMask(8, 32)})
	got := PrefixesFromIPNets(nets)
	want := []string{"192.168.1.0/24", "fd00::/8", "10.0.0.0/8"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestNilPolicyAppliesBuiltinRules(t *testing.T) {
	t.Parallel()
	var p *Policy
	if !p.AllowedIP(net.ParseIP("8.8.8.8")) || p.AdmitLearned(net.ParseIP("10.0.0.1")) {
		t.Fatal("nil policy decisions wrong")
	}
	if p.CheckUDP(net.ParseIP("127.0.0.1")) == nil || p.ControlContext(context.Background(), "tcp", "127.0.0.1:1", nil) == nil {
		t.Fatal("nil policy allowed loopback")
	}
	if p.AllowlistMode() || p.AllowNonGlobal() || p.Summary() != "egress refused learned=0 udp=0 tcp=0" {
		t.Fatal("nil policy state wrong")
	}
}
