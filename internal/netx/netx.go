// Package netx holds the small networking helpers lmnt needs on the host:
// finding free TCP ports for forwarded services and parsing user-supplied
// port-forwarding specs.
package netx

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"
)

// Loopback is the address every forwarded port binds to unless the user asks
// for something else.
var Loopback = netip.MustParseAddr("127.0.0.1")

// FreePort asks the kernel for an unused TCP port on the loopback interface.
// The port is released before returning, so it is a hint, not a reservation.
func FreePort() (uint16, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen on an ephemeral port: %w", err)
	}

	addr, ok := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()
	if !ok || addr.Port <= 0 || addr.Port > 0xffff {
		return 0, fmt.Errorf("unexpected listener address %v", ln.Addr())
	}

	return uint16(addr.Port), nil
}

// FreePortRange finds n consecutive free TCP ports on ip, starting the search
// at from and walking upwards. It returns the first port of the run. Like
// FreePort it probes rather than reserves.
func FreePortRange(ip netip.Addr, from uint16, n int) (uint16, error) {
	if n <= 0 {
		return 0, errors.New("port range length must be positive")
	}

	return searchRange(from, n, func(p uint16) (bool, error) { return portFree(ip, p) })
}

// searchRange is FreePortRange with the probe injected, for tests. The loop
// counter is an int so the walk cannot wrap around at the top of the range.
func searchRange(from uint16, n int, free func(uint16) (bool, error)) (uint16, error) {
	for start := int(from); start+n-1 <= 0xffff; start++ {
		ok, err := runFree(start, n, free)
		if err != nil {
			return 0, err
		}

		if ok {
			return uint16(start), nil //nolint:gosec // start+n-1 <= 0xffff by the loop condition
		}
	}

	return 0, fmt.Errorf("no run of %d free ports at or above %d", n, from)
}

func runFree(start, n int, free func(uint16) (bool, error)) (bool, error) {
	for p := start; p < start+n; p++ {
		ok, err := free(uint16(p)) //nolint:gosec // p < start+n <= 0x10000, checked by the caller
		if err != nil {
			return false, fmt.Errorf("probe port %d: %w", p, err)
		}

		if !ok {
			return false, nil
		}
	}

	return true, nil
}

func portFree(ip netip.Addr, port uint16) (bool, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, syscall.EACCES) {
			return false, nil
		}

		return false, err
	}

	return true, ln.Close()
}

// Forward maps a TCP port on the host onto a TCP port inside the guest.
type Forward struct {
	Host      netip.AddrPort
	GuestPort uint16
}

// String renders the forward in the same form ParseForward accepts.
func (f Forward) String() string {
	return f.Host.String() + ":" + strconv.Itoa(int(f.GuestPort))
}

// ParseForward accepts "<host port>:<guest port>" or
// "<host ip>:<host port>:<guest port>". IPv6 host addresses must be written
// in brackets. Without an address the forward binds to loopback.
func ParseForward(s string) (Forward, error) {
	hostPart, guestPart, ok := cutLast(s, ':')
	if !ok {
		return Forward{}, fmt.Errorf("%q: want <host port>:<guest port> or <ip>:<host port>:<guest port>", s)
	}

	guestPort, err := parsePort(guestPart)
	if err != nil {
		return Forward{}, fmt.Errorf("%q: guest port: %w", s, err)
	}

	// A bare number is a host port on loopback.
	if hostPort, err := parsePort(hostPart); err == nil {
		return Forward{Host: netip.AddrPortFrom(Loopback, hostPort), GuestPort: guestPort}, nil
	}

	host, err := netip.ParseAddrPort(hostPart)
	if err != nil {
		return Forward{}, fmt.Errorf("%q: host address: %w", s, err)
	}

	if host.Port() == 0 {
		return Forward{}, fmt.Errorf("%q: host port must not be zero", s)
	}

	return Forward{Host: host, GuestPort: guestPort}, nil
}

func parsePort(s string) (uint16, error) {
	p, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%q is not a port number", s)
	}

	if p == 0 {
		return 0, errors.New("port must not be zero")
	}

	return uint16(p), nil
}

func cutLast(s string, sep byte) (before, after string, ok bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}

	return s, "", false
}
