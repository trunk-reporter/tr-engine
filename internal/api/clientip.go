package api

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// TrustedProxies decides whose X-Forwarded-For / X-Real-IP headers are believed.
// Those headers are set by whoever sends the request, so they only identify the
// real client when the request arrived from a reverse proxy we trust. Anything
// else must be judged by the TCP peer address alone — otherwise a client can
// pick its own "IP" and walk around per-IP rate limits.
type TrustedProxies struct {
	prefixes []netip.Prefix
}

var (
	loopbackPrefixes = []string{"127.0.0.0/8", "::1/128"}
	privatePrefixes  = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
)

// ParseTrustedProxies parses a TRUSTED_PROXIES value: a comma-separated list of
// IP addresses, CIDR prefixes, and the keywords "loopback" (127.0.0.0/8, ::1)
// and "private" (RFC 1918 + IPv6 ULA). "none" or an empty string trusts no one.
func ParseTrustedProxies(spec string) (*TrustedProxies, error) {
	tp := &TrustedProxies{}
	for _, raw := range strings.Split(spec, ",") {
		item := strings.TrimSpace(raw)
		switch strings.ToLower(item) {
		case "", "none":
			continue
		case "loopback":
			tp.addAll(loopbackPrefixes)
			continue
		case "private":
			tp.addAll(privatePrefixes)
			continue
		}
		if strings.Contains(item, "/") {
			p, err := netip.ParsePrefix(item)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy prefix %q: %w", item, err)
			}
			tp.prefixes = append(tp.prefixes, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy address %q: %w", item, err)
		}
		a = a.Unmap()
		tp.prefixes = append(tp.prefixes, netip.PrefixFrom(a, a.BitLen()))
	}
	return tp, nil
}

func (tp *TrustedProxies) addAll(prefixes []string) {
	for _, s := range prefixes {
		tp.prefixes = append(tp.prefixes, netip.MustParsePrefix(s))
	}
}

// Trusted reports whether addr belongs to a trusted proxy. A nil receiver trusts no one.
func (tp *TrustedProxies) Trusted(addr netip.Addr) bool {
	if tp == nil || !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	for _, p := range tp.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientIP returns the address of the client that made the request.
//
// Forwarding headers are only consulted when the TCP peer is a trusted proxy.
// X-Forwarded-For is walked from the right (the entry our proxy appended)
// towards the left, skipping further trusted proxies; the first untrusted
// address is the client. Everything to its left was supplied by the client
// and is ignored.
func (tp *TrustedProxies) ClientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	peerAddr, err := netip.ParseAddr(peer)
	if err != nil || !tp.Trusted(peerAddr) {
		return peer
	}

	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(h, ",") {
			if s := strings.TrimSpace(part); s != "" {
				hops = append(hops, s)
			}
		}
	}
	if len(hops) > 0 {
		client := peer
		for i := len(hops) - 1; i >= 0; i-- {
			a, err := netip.ParseAddr(hops[i])
			if err != nil {
				// Garbage in the chain: stop at the last address we could verify.
				break
			}
			client = a.Unmap().String()
			if !tp.Trusted(a) {
				break
			}
		}
		return client
	}

	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if a, err := netip.ParseAddr(xri); err == nil {
			return a.Unmap().String()
		}
	}
	return peer
}
