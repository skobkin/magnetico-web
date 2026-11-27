// Package netpolicy decides which remote addresses the crawler may send DHT
// datagrams to or open BitTorrent connections to.
//
// DHT nodes hand out addresses of other nodes and peers, and some of them are
// private, reserved or otherwise not globally reachable. Traffic to such
// destinations leaves the host towards the provider's network and is commonly
// reported as port scanning. A Policy refuses them twice: when an address is
// learned (so it never enters a routing table or peer list) and at the egress
// chokepoints (the UDP send and the TCP dial), so no code path can bypass it.
//
// The policy covers only DHT and BitTorrent traffic. The crawler's HTTP client
// for the ingest service does not consult it: ingest is often reached over a
// private network.
package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
)

// ErrRefused is returned for a destination the policy does not allow.
var ErrRefused = errors.New("destination address refused by the egress policy")

// Options configures New.
type Options struct {
	// Allow, when not empty, switches to allowlist mode (a private DHT): only
	// these ranges are allowed and the built-in list is not applied.
	Allow []netip.Prefix
	// Deny lists ranges that are always refused; it wins over Allow.
	Deny []netip.Prefix
	// AllowNonGlobal disables the built-in list of non-globally-reachable
	// ranges outside allowlist mode. Invalid, unspecified, multicast and
	// broadcast addresses stay refused.
	AllowNonGlobal bool
}

// Policy is immutable after New and safe for concurrent use. Its counters are
// updated atomically. A nil *Policy applies the built-in rules only and counts
// nothing.
type Policy struct {
	allow, deny    []netip.Prefix
	allowNonGlobal bool

	learned, udp, tcp atomic.Uint64
}

// New builds a Policy. Prefixes are masked and IPv4-mapped IPv6 prefixes are
// converted to IPv4.
func New(o Options) *Policy {
	return &Policy{
		allow:          normalize(o.Allow),
		deny:           normalize(o.Deny),
		allowNonGlobal: o.AllowNonGlobal,
	}
}

// Default is the policy without operator settings: the built-in list only.
func Default() *Policy { return New(Options{}) }

// AllowNonGlobal reports whether the built-in list is disabled.
func (p *Policy) AllowNonGlobal() bool { return p != nil && p.allowNonGlobal }

// AllowlistMode reports whether an allowlist (a private DHT) is configured.
func (p *Policy) AllowlistMode() bool { return p != nil && len(p.allow) > 0 }

// Allowed reports whether a may be a destination. An IPv6 address that
// carries an IPv4 address (NAT64 well-known prefix, 6to4) is allowed only if
// the IPv4 address is allowed too.
func (p *Policy) Allowed(a netip.Addr) bool {
	a = a.WithZone("").Unmap()
	if !p.allowedOne(a) {
		return false
	}
	if v4, ok := embeddedIPv4(a); ok {
		return p.allowedOne(v4)
	}
	return true
}

// AllowedIP is Allowed for a net.IP; a nil or malformed IP is refused.
func (p *Policy) AllowedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return ok && p.Allowed(a)
}

// AdmitLearned is AllowedIP for an address learned from the network (a DHT
// node or a peer); a refusal is counted as "learned".
func (p *Policy) AdmitLearned(ip net.IP) bool {
	if p.AllowedIP(ip) {
		return true
	}
	if p != nil {
		p.learned.Add(1)
	}
	return false
}

// CheckUDP returns ErrRefused, counted as "udp", for a datagram destination
// the policy does not allow.
func (p *Policy) CheckUDP(ip net.IP) error {
	if p.AllowedIP(ip) {
		return nil
	}
	if p != nil {
		p.udp.Add(1)
	}
	return ErrRefused
}

// ControlContext is a net.Dialer.ControlContext hook: it sees the final
// resolved address before the connection is attempted and refuses it,
// counted as "tcp", when the policy does not allow it.
func (p *Policy) ControlContext(_ context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err == nil && p.Allowed(ap.Addr()) {
		return nil
	}
	if p != nil {
		p.tcp.Add(1)
	}
	return fmt.Errorf("%w: %s", ErrRefused, address)
}

// Counts returns the refusals so far: learned addresses, UDP sends, TCP dials.
func (p *Policy) Counts() (learned, udp, tcp uint64) {
	if p == nil {
		return 0, 0, 0
	}
	return p.learned.Load(), p.udp.Load(), p.tcp.Load()
}

// Summary renders the counters for the periodic crawl summary line.
func (p *Policy) Summary() string {
	learned, udp, tcp := p.Counts()
	return fmt.Sprintf("egress refused learned=%d udp=%d tcp=%d", learned, udp, tcp)
}

func (p *Policy) allowedOne(a netip.Addr) bool {
	if !a.IsValid() || a.IsUnspecified() || a.IsMulticast() || a == limitedBroadcast {
		return false
	}
	if p == nil {
		return !NonGlobal(a)
	}
	if containsAny(p.deny, a) {
		return false
	}
	if len(p.allow) > 0 {
		return containsAny(p.allow, a)
	}
	return p.allowNonGlobal || !NonGlobal(a)
}

// NonGlobal reports whether a is in the built-in list of destinations that are
// not globally reachable. a must be unmapped and without a zone.
func NonGlobal(a netip.Addr) bool {
	if a.Is4() {
		return containsAny(nonGlobalIPv4, a)
	}
	if !globalUnicastIPv6.Contains(a) && !nat64WellKnown.Contains(a) {
		return true
	}
	return containsAny(nonGlobalIPv6, a)
}

// ParsePrefixes parses a list of CIDRs; empty items are skipped. A bare
// address is taken as a single-host prefix.
func ParsePrefixes(items []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		p, err := netip.ParsePrefix(item)
		if err != nil {
			a, aerr := netip.ParseAddr(item)
			if aerr != nil || a.Zone() != "" {
				return nil, fmt.Errorf("invalid CIDR %q: %w", item, err)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Is4In6() && p.Bits() < 96 {
			return nil, fmt.Errorf("invalid CIDR %q: an IPv4-mapped prefix must be at least /96", item)
		}
		out = append(out, p)
	}
	return normalize(out), nil
}

// PrefixesFromIPNets converts net.IPNet values (as produced by
// net.ParseCIDR) to normalized prefixes; malformed ones are skipped.
func PrefixesFromIPNets(nets []net.IPNet) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(nets))
	for _, n := range nets {
		a, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ones, bits := n.Mask.Size()
		if bits == 0 {
			continue
		}
		if bits == 32 && a.Is4In6() {
			a = a.Unmap()
		}
		out = append(out, netip.PrefixFrom(a, ones))
	}
	return normalize(out)
}

func normalize(in []netip.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(in))
	for _, p := range in {
		if !p.IsValid() {
			continue
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out
}

func containsAny(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// embeddedIPv4 returns the IPv4 address carried by a NAT64 well-known-prefix
// (RFC 6052) or 6to4 (RFC 3056) address.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	switch {
	case nat64WellKnown.Contains(a):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}
