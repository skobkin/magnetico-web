package netpolicy

import "net/netip"

// The built-in list follows the IANA IPv4 and IPv6 Special-Purpose Address
// Registries (both last updated 2025-10-09): every block whose "Globally
// Reachable" column is False is refused as a whole, so the few globally
// reachable anycast exceptions inside 192.0.0.0/24 and 2001::/23 are refused
// too; none of them is a DHT node or a BitTorrent peer.
//
// https://www.iana.org/assignments/iana-ipv4-special-registry/
// https://www.iana.org/assignments/iana-ipv6-special-registry/
var nonGlobalIPv4 = mustPrefixes(
	"0.0.0.0/8",       // "This network" (RFC 791)
	"10.0.0.0/8",      // Private-Use (RFC 1918)
	"100.64.0.0/10",   // Shared Address Space, carrier-grade NAT (RFC 6598)
	"127.0.0.0/8",     // Loopback (RFC 1122)
	"169.254.0.0/16",  // Link Local (RFC 3927)
	"172.16.0.0/12",   // Private-Use (RFC 1918)
	"192.0.0.0/24",    // IETF Protocol Assignments (RFC 6890)
	"192.0.2.0/24",    // Documentation, TEST-NET-1 (RFC 5737)
	"192.88.99.0/24",  // Deprecated 6to4 Relay Anycast (RFC 7526)
	"192.168.0.0/16",  // Private-Use (RFC 1918)
	"198.18.0.0/15",   // Benchmarking (RFC 2544)
	"198.51.100.0/24", // Documentation, TEST-NET-2 (RFC 5737)
	"203.0.113.0/24",  // Documentation, TEST-NET-3 (RFC 5737)
	"224.0.0.0/4",     // Multicast (RFC 5771)
	"240.0.0.0/4",     // Reserved, including 255.255.255.255 Limited Broadcast (RFC 1112, RFC 919)
)

// IPv6 outside 2000::/3, the only block of the IANA IPv6 Address Space
// registry allocated as Global Unicast, is refused (except the NAT64
// well-known prefix, whose embedded IPv4 address is checked instead). That
// covers ::/128, ::1/128, the IPv4-compatible and IPv4-mapped blocks,
// 64:ff9b:1::/48, 100::/64, 100:0:0:1::/64, 5f00::/16, fc00::/7, fe80::/10,
// the deprecated site-local fec0::/10 and multicast ff00::/8.
var (
	globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")
	nat64WellKnown    = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052
	sixToFour         = netip.MustParsePrefix("2002::/16")    // RFC 3056
)

// nonGlobalIPv6 lists the not globally reachable blocks inside 2000::/3.
var nonGlobalIPv6 = mustPrefixes(
	"2001::/23",     // IETF Protocol Assignments (RFC 2928), including Teredo 2001::/32 and Benchmarking 2001:2::/48
	"2001:db8::/32", // Documentation (RFC 3849)
	"3fff::/20",     // Documentation (RFC 9637)
)

var limitedBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}
