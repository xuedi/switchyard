package web

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// proxies are the reverse proxies whose X-Forwarded-For and X-Forwarded-Proto are believed; a
// request from anyone else is taken as it comes.
type proxies []netip.Prefix

// parseProxies reads the config's trustedProxies, addresses or CIDR ranges; none is the loopback.
func parseProxies(list []string) (proxies, error) {
	if len(list) == 0 {
		list = []string{"127.0.0.1", "::1"}
	}
	var p proxies
	for _, s := range list {
		if pre, err := netip.ParsePrefix(s); err == nil {
			p = append(p, pre.Masked())
		} else if a, err := netip.ParseAddr(s); err == nil {
			p = append(p, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		} else {
			return nil, fmt.Errorf("trustedProxies: %q is not an address or a CIDR range", s)
		}
	}
	return p, nil
}

func (p proxies) trusted(a netip.Addr) bool {
	for _, pre := range p {
		if pre.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

func peer(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	return a.Unmap(), err == nil
}

// client is who sent the request: the peer, or behind a trusted proxy the rightmost
// X-Forwarded-For entry that is not itself a trusted proxy (the ones to its left were written
// by the client and prove nothing).
func (p proxies) client(r *http.Request) string {
	a, ok := peer(r)
	if !ok {
		return r.RemoteAddr
	}
	if !p.trusted(a) {
		return a.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if h == "" {
			continue
		}
		x, err := netip.ParseAddr(h)
		if err != nil {
			return a.String()
		}
		if !p.trusted(x) {
			return x.Unmap().String()
		}
	}
	return a.String()
}

// https: the browser is on HTTPS, directly or as a trusted proxy's last X-Forwarded-Proto says.
func (p proxies) https(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	a, ok := peer(r)
	if !ok || !p.trusted(a) {
		return false
	}
	protos := strings.Split(strings.Join(r.Header.Values("X-Forwarded-Proto"), ","), ",")
	return strings.TrimSpace(protos[len(protos)-1]) == "https"
}
