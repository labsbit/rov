package docker

import (
	"slices"
	"strconv"
	"strings"
)

// maxRange caps the expansion of one published port range, so a malformed
// or absurd entry can't allocate without bound.
const maxRange = 4096

// ParsePorts parses the Ports field of docker ps, keeping the host side of
// published ports only:
//
//	127.0.0.1:6379->6379/tcp      → 127.0.0.1 6379 tcp
//	[::]:8123->8123/tcp           → :: 8123 tcp
//	:::8123->8123/tcp             → :: 8123 tcp      (older docker)
//	0.0.0.0:5000-5002->5000-5002/tcp → 5000, 5001, 5002
//	9000/tcp                      → (unpublished, ignored)
//
// The same port bound on 0.0.0.0 and :: yields two entries; see
// [UniquePorts]. Exact duplicates are dropped, order is kept. Malformed
// entries are skipped.
func ParsePorts(s string) []Port {
	var out []Port
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		host, ctr, ok := strings.Cut(entry, "->")
		if !ok {
			continue // unpublished
		}
		proto := "tcp"
		if _, p, ok := strings.Cut(ctr, "/"); ok && p != "" {
			proto = p
		}
		i := strings.LastIndexByte(host, ':')
		if i < 0 {
			continue
		}
		addr, ports := host[:i], host[i+1:]
		addr = strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
		switch addr {
		case "":
			addr = "0.0.0.0"
		case "::", ":":
			addr = "::"
		}
		lo, hi, ok := parseRange(ports)
		if !ok {
			continue
		}
		for p := lo; p <= hi; p++ {
			pt := Port{Port: p, Addr: addr, Proto: proto}
			if !slices.Contains(out, pt) {
				out = append(out, pt)
			}
		}
	}
	return out
}

// parseRange parses "8080" or "5000-5002" into valid port bounds.
func parseRange(s string) (lo, hi int, ok bool) {
	a, b, isRange := strings.Cut(s, "-")
	lo, err := strconv.Atoi(a)
	if err != nil || lo < 1 || lo > 65535 {
		return 0, 0, false
	}
	if !isRange {
		return lo, lo, true
	}
	hi, err = strconv.Atoi(b)
	if err != nil || hi < lo || hi > 65535 || hi-lo >= maxRange {
		return 0, 0, false
	}
	return lo, hi, true
}

// UniquePorts returns the distinct port numbers of ps, sorted, regardless of
// address and protocol.
func UniquePorts(ps []Port) []int {
	out := make([]int, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Port)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TCP returns the TCP entries of ps.
func TCP(ps []Port) []Port {
	var out []Port
	for _, p := range ps {
		if p.Proto == "tcp" {
			out = append(out, p)
		}
	}
	return out
}
