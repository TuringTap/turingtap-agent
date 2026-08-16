// Package tunnel maintains the outbound WSS connection to relay.turingtap.ai,
// multiplexes it with yamux, and serves relay-initiated streams: CH_SOCKS
// (reverse LAN dials for the cloud proxy), CH_RPC (browser.goto/act/... —
// see rpc.go) and CH_CDP (solver↔Chromium handoff bridge — see cdp.go).
// No inbound ports are opened.
package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Channel tag bytes — first byte the relay writes on a freshly-opened yamux
// stream. Must match services/relay/mux.py CH_*.
const (
	ChSocks = 0x01 // reverse LAN CONNECT (compact host:port codec, not SOCKS5)
	ChCDP   = 0x02 // CDP passthrough for mobile handoff (agent-initiated)
	ChRPC   = 0x03 // JSON-RPC: browser.goto/act/screenshot/handoff, agent.status
)

// Tunnel owns the relay connection lifecycle.
type Tunnel struct {
	relayURL string
	apiKey   string
	allow    []*net.IPNet

	mu      sync.Mutex
	sess    *yamux.Session
	onState func(online bool)
	browser BrowserRPC

	// Handoff (CH_CDP) state — see cdp.go.
	cdpMu         sync.Mutex
	cdpConns      map[*cdpConn]struct{}
	handoffPrompt string       // last prompt from a browser.handoff RPC
	handoffRevert bool         // we flipped the browser headed → revert on end
	onHandoffEnd  func(reason string)

	socksDials atomic.Int64

	// dial is overridable for tests.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New constructs a Tunnel. allow is the parsed lan_allow_cidrs list.
func New(relayURL, apiKey string, allow []*net.IPNet) *Tunnel {
	return &Tunnel{
		relayURL: relayURL,
		apiKey:   apiKey,
		allow:    allow,
		dial:     (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
}

// OnStateChange registers a callback fired when the relay connection goes
// online/offline (used by the tray indicator).
func (t *Tunnel) OnStateChange(f func(online bool)) { t.onState = f }

// OnHandoffEnd registers a callback fired when a human handoff ends; reason
// is "user_stopped" (solver pressed Done), "dismissed" (AI called
// dismiss_human) or "closed" (bridge torn down).
func (t *Tunnel) OnHandoffEnd(f func(reason string)) { t.onHandoffEnd = f }

// Online reports whether a yamux session is currently established.
func (t *Tunnel) Online() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sess != nil && !t.sess.IsClosed()
}

// SocksDials returns the number of CH_SOCKS connect requests served
// (successful or not). Exposed for e2e assertions.
func (t *Tunnel) SocksDials() int64 { return t.socksDials.Load() }

// Run connects to the relay and blocks, reconnecting with backoff until ctx
// is cancelled.
func (t *Tunnel) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if err := t.runOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("tunnel: relay session ended", "err", err, "retry_in", backoff)
		}
		t.setSession(nil)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (t *Tunnel) runOnce(ctx context.Context) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+t.apiKey)

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.DialContext(ctx, t.relayURL, hdr)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	conn := newWSConn(ws)

	cfg := yamux.DefaultConfig()
	// The relay's Python mux does not answer yamux PINGs (WS-layer keepalive
	// only), so the default keepalive would kill the session after ~40 s.
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("yamux: %w", err)
	}
	t.setSession(sess)
	slog.Info("tunnel: connected", "relay", t.relayURL)

	go func() {
		<-ctx.Done()
		_ = sess.Close()
	}()

	for {
		stream, err := sess.Accept()
		if err != nil {
			return err
		}
		go t.handleStream(stream)
	}
}

func (t *Tunnel) setSession(s *yamux.Session) {
	t.mu.Lock()
	t.sess = s
	cb := t.onState
	t.mu.Unlock()
	if cb != nil {
		cb(s != nil)
	}
}

// OpenChannel opens a client-initiated yamux stream to the relay tagged with
// the given kind byte (e.g. ChCDP for CDP passthrough during handoff).
func (t *Tunnel) OpenChannel(kind byte) (net.Conn, error) {
	t.mu.Lock()
	sess := t.sess
	t.mu.Unlock()
	if sess == nil || sess.IsClosed() {
		return nil, errors.New("tunnel: not connected")
	}
	st, err := sess.Open()
	if err != nil {
		return nil, err
	}
	if _, err := st.Write([]byte{kind}); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// handleStream reads the 1-byte channel discriminator and dispatches.
func (t *Tunnel) handleStream(c net.Conn) {
	defer c.Close()
	var tag [1]byte
	if _, err := io.ReadFull(c, tag[:]); err != nil {
		return
	}
	switch tag[0] {
	case ChSocks:
		if err := t.serveConnect(c); err != nil {
			slog.Debug("tunnel: socks stream closed", "err", err)
		}
	case ChCDP:
		if err := t.serveCDP(c); err != nil {
			slog.Debug("tunnel: cdp stream closed", "err", err)
		}
	case ChRPC:
		if err := t.serveRPC(c); err != nil {
			slog.Debug("tunnel: rpc stream closed", "err", err)
		}
	default:
		slog.Warn("tunnel: unknown stream kind", "kind", tag[0])
	}
}

// serveConnect handles the relay's compact CONNECT codec on a CH_SOCKS
// stream: uint8 hostlen, host bytes, uint16be port. Reply is a single status
// byte (0x00 = ok, otherwise a SOCKS5 REP code) followed by bidirectional
// payload. Matches services/relay/socks.py ConnectRequest.encode.
func (t *Tunnel) serveConnect(c net.Conn) error {
	t.socksDials.Add(1)

	var hl [1]byte
	if _, err := io.ReadFull(c, hl[:]); err != nil {
		return err
	}
	hostb := make([]byte, hl[0])
	if _, err := io.ReadFull(c, hostb); err != nil {
		return err
	}
	var pb [2]byte
	if _, err := io.ReadFull(c, pb[:]); err != nil {
		return err
	}
	host := string(hostb)
	port := int(binary.BigEndian.Uint16(pb[:]))

	ips, err := resolve(host)
	if err != nil || !t.allowed(ips) {
		_, _ = c.Write([]byte{repNotAllowed})
		return fmt.Errorf("socks: destination %s not permitted: %v", host, err)
	}

	dst, err := t.dial(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		_, _ = c.Write([]byte{repHostUnreachable})
		return fmt.Errorf("socks: dial %s:%d: %w", host, port, err)
	}
	defer dst.Close()

	if _, err := c.Write([]byte{repSucceeded}); err != nil {
		return err
	}
	slog.Debug("tunnel: LAN connect", "host", host, "port", port)

	errc := make(chan error, 2)
	go proxyCopy(dst, c, errc)
	go proxyCopy(c, dst, errc)
	<-errc
	return nil
}

func resolve(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return net.LookupIP(host)
}
