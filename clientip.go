package logger

import (
	"fmt"
	"net/netip"
	"strings"
)

// trustedForwarders is a set of networks whose forwarding headers are trusted.
type trustedForwarders []netip.Prefix

// parseTrustedForwarders parses IP addresses and CIDR ranges, e.g.
// "10.0.0.0/8" or "192.0.2.1".
func parseTrustedForwarders(values []string) (trustedForwarders, error) {
	trusted := make(trustedForwarders, 0, len(values))

	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		if strings.Contains(value, "/") {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted forwarder %q: %w", value, err)
			}

			trusted = append(trusted, prefix.Masked())

			continue
		}

		addr, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted forwarder %q: %w", value, err)
		}

		addr = addr.Unmap()
		trusted = append(trusted, netip.PrefixFrom(addr, addr.BitLen()))
	}

	return trusted, nil
}

// contains reports whether addr belongs to a trusted network.
func (t trustedForwarders) contains(addr netip.Addr) bool {
	addr = addr.Unmap()

	for _, prefix := range t {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// clientIP returns the address of the client that originated the request.
//
// peer is the address of the direct peer (RemoteAddr without the port). When
// the peer is trusted, X-Forwarded-For is walked from right to left, skipping
// trusted forwarders: the first untrusted address is the client. Addresses
// further left were supplied by the client itself and cannot be trusted. If
// every address is trusted, the leftmost one is used. Without
// X-Forwarded-For, X-Real-Ip is used. In every other case, the peer is the
// client.
func (t trustedForwarders) clientIP(peer, forwardedFor, realIP string) string {
	if len(t) == 0 {
		return peer
	}

	if addr, err := netip.ParseAddr(peer); err != nil || !t.contains(addr) {
		return peer
	}

	if forwardedFor == "" {
		if realIP = strings.TrimSpace(realIP); realIP != "" {
			return realIP
		}

		return peer
	}

	hops := strings.Split(forwardedFor, ",")

	var leftmost string

	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}

		addr, ok := parseHop(hop)
		if !ok {
			// Not an address: it cannot be checked, so it is the client
			// as reported by the closest trusted forwarder.
			return hop
		}

		if !t.contains(addr) {
			return addr.String()
		}

		leftmost = addr.String()
	}

	if leftmost == "" {
		return peer
	}

	return leftmost
}

// parseHop parses an X-Forwarded-For entry: an IP address, optionally with a
// port ("192.0.2.1:1234", "[2001:db8::1]:1234").
func parseHop(hop string) (netip.Addr, bool) {
	if addr, err := netip.ParseAddr(hop); err == nil {
		return addr.Unmap(), true
	}

	if addrPort, err := netip.ParseAddrPort(hop); err == nil {
		return addrPort.Addr().Unmap(), true
	}

	return netip.Addr{}, false
}
