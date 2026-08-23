package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
)

// Reply codes for the CH_SOCKS status byte. Values match SOCKS5 REP so the
// relay can translate them straight into a CONNECT / SOCKS reply.
const (
	repSucceeded       = 0x00
	repNotAllowed      = 0x02
	repHostUnreachable = 0x04
)

// allowed returns true iff every resolved IP is private/loopback AND falls
// within at least one configured CIDR. Any public IP causes refusal.
func (t *Tunnel) allowed(ips []net.IP) bool {
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsPrivate() && !ip.IsLoopback() {
			return false
		}
		ok := false
		for _, n := range t.allow {
			if n.Contains(ip) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func proxyCopy(dst io.Writer, src io.Reader, errc chan<- error) {
	_, err := io.Copy(dst, src)
	if c, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	}
	errc <- err
}

// LANTest dials host:port through the same allow-list gate the reverse-SOCKS
// handler uses. Exposed for the local MCP tool agent_lan_test.
func (t *Tunnel) LANTest(host string, port int) error {
	ips, err := resolve(host)
	if err != nil {
		return err
	}
	if !t.allowed(ips) {
		return errors.New("destination not permitted by lan_allow_cidrs")
	}
	c, err := t.dial(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return c.Close()
}
