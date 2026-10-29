package node

import (
	"net"
	"strconv"
)

func localIPs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch a := addr.(type) {
			case *net.IPNet:
				ip = a.IP
			case *net.IPAddr:
				ip = a.IP
			}
			ip = ip.To16()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

func hostports(ips []net.IP, port int, extra string, loopback bool) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	p := strconv.Itoa(port)
	for _, ip := range ips {
		add(net.JoinHostPort(ip.String(), p))
	}
	if extra != "" {
		if _, _, err := net.SplitHostPort(extra); err == nil {
			add(extra)
		} else if ip := net.ParseIP(extra); ip != nil {
			add(net.JoinHostPort(ip.String(), p))
		}
	}
	if loopback || len(out) == 0 {
		add(net.JoinHostPort("127.0.0.1", p))
	}
	return out
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// usableObserved reports whether an address learned from discovery is safe to
// keep. Loopback and unspecified values show up when a Docker proxy answers
// after a restart; storing them would replace a real address and point peers
// at themselves.
func usableObserved(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified()
}
