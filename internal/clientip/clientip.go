// Package clientip resolves the address of a client behind trusted reverse
// proxies. It is shared by the web service (rate limits) and the ingest service
// (the last address of a crawler) and has no dependencies on either.
package clientip

import (
	"net/netip"
	"strings"
)

// maxForwardedHops bounds the X-Forwarded-For entries examined.
const maxForwardedHops = 64

func trusted(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// normalizeAddr drops the IPv4-mapped form so a prefix list written in plain
// IPv4 matches dual-stack sockets. Zoned addresses are rejected.
func normalizeAddr(a netip.Addr) (netip.Addr, bool) {
	if !a.IsValid() || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func parseForwardedEntry(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return normalizeAddr(a)
	}
	if ap, err := netip.ParseAddrPort(s); err == nil { // "1.2.3.4:5", "[::1]:5"
		return normalizeAddr(ap.Addr())
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") { // "[::1]"
		if a, err := netip.ParseAddr(s[1 : len(s)-1]); err == nil {
			return normalizeAddr(a)
		}
	}
	return netip.Addr{}, false
}

// Resolve returns the client address of a request (web spec §3.2).
//
// The forwarding header is consulted only when the direct TCP peer is inside
// a trusted-proxy prefix; otherwise the peer is the client and the header is
// ignored, so an untrusted client cannot choose its rate-limit identity.
// When the peer is trusted the chain is walked from the right (nearest hop
// first): trusted hops are skipped and the first untrusted address is the
// client. A malformed entry breaks the chain of custody — nothing to its
// left can be believed — so resolution stops and the peer address is used.
// If every hop is trusted the peer is used as well. The result is the
// zero Addr only when the peer address itself is unparsable.
func Resolve(remoteAddr string, forwardedFor []string, proxies []netip.Prefix) netip.Addr {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	peer, ok := normalizeAddr(ap.Addr())
	if !ok {
		return netip.Addr{}
	}
	if len(proxies) == 0 || !trusted(proxies, peer) {
		return peer
	}

	var hops []string
	for _, v := range forwardedFor {
		hops = append(hops, strings.Split(v, ",")...)
	}
	if len(hops) > maxForwardedHops {
		return peer
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := parseForwardedEntry(hops[i])
		if !ok {
			return peer
		}
		if !trusted(proxies, hop) {
			return hop
		}
	}
	return peer
}
