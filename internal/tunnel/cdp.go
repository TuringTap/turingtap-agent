package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/turingtap/agent/internal/browser"
)

// serveCDP bridges one solver (web or mobile) to the local Chromium for a
// human handoff.
//
// Wire format, both directions: bare JSON envelopes {"method","params"} with
// no delimiter or length prefix. The relay maps one solver WebSocket message
// to one yamux DATA frame and vice versa (services/relay/server.py
// Relay._pump), so every outbound envelope is written with a single Write
// (one Write ≤ send window == one DATA frame == one WS message; the relay
// grants a 1 GiB window on stream open). Inbound envelopes are decoded with a
// streaming json.Decoder, which tolerates frames coalescing in the yamux
// read buffer.
//
// The relay prefixes the stream with the bare handoff id (its own DATA
// frame, after the channel tag byte already consumed by handleStream).
//
// Inbound:  Input.dispatchMouseEvent / dispatchKeyEvent / dispatchTouchEvent
//           → CDP Input.*; Page.screencastFrameAck (dropped — Chromium is
//           acked locally by browser.StartScreencast); TuringTap.done → end
//           the handoff and revert headless (unless the user opened the
//           browser themselves).
// Outbound: Page.screencastFrame {data, sessionId, metadata:{deviceWidth,
//           deviceHeight}}; TuringTap.dismiss {message} (sent by dismissCDP
//           when the AI calls dismiss_human).
func (t *Tunnel) serveCDP(c net.Conn) error {
	if t.browser == nil {
		return errors.New("cdp: browser not configured")
	}

	// First frame is the bare handoff id; anything from the first '{' on is
	// already a solver envelope (frames can coalesce in the read buffer).
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		return fmt.Errorf("cdp: read handoff id: %w", err)
	}
	raw := buf[:n]
	hid := string(raw)
	var rem []byte
	if i := bytes.IndexByte(raw, '{'); i >= 0 {
		hid, rem = string(raw[:i]), raw[i:]
	}
	slog.Info("tunnel: handoff bridge connected", "handoff_id", hid)

	// The headed flip + banner normally happened at ask_human time via the
	// browser.handoff RPC; ensure it if the solver connected without one.
	if !t.browser.Headed() {
		t.cdpMu.Lock()
		prompt := t.handoffPrompt
		t.cdpMu.Unlock()
		if prompt == "" {
			prompt = "A human is assisting your AI session."
		}
		t.setHandoffRevert(true)
		if err := t.browser.SetHeaded(prompt); err != nil {
			// No display (or launch failure): screencast works headless in
			// CDP, so keep going.
			slog.Warn("tunnel: headed flip failed — continuing headless", "err", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := make(chan browser.Frame, 1)
	if err := t.browser.StartScreencast(ctx, frames); err != nil {
		return fmt.Errorf("cdp: start screencast: %w", err)
	}

	conn := &cdpConn{c: c}
	t.addCDP(conn)
	reason := "closed"
	defer func() {
		t.removeCDP(conn)
		cancel() // stops the screencast
		if t.takeHandoffRevert() {
			if err := t.browser.SetHeaded(""); err != nil {
				slog.Warn("tunnel: headless revert failed", "err", err)
			}
		}
		slog.Info("tunnel: handoff bridge closed", "handoff_id", hid, "reason", reason)
		if cb := t.onHandoffEnd; cb != nil && !conn.dismissed() {
			cb(reason)
		}
	}()

	// Frames → solver.
	go func() {
		for {
			select {
			case f := <-frames:
				if err := conn.sendJSON("Page.screencastFrame", screencastParams{
					Data:      f.Data,
					SessionID: f.SessionID,
					Metadata:  screencastMetadata{f.DeviceWidth, f.DeviceHeight},
				}); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Solver → browser.
	dec := json.NewDecoder(io.MultiReader(bytes.NewReader(rem), c))
	for {
		var env struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&env); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch env.Method {
		case "Page.screencastFrameAck":
			// Chromium is acked locally; the solver's ack only paces its own
			// pipe — drop it.
		case "Input.dispatchMouseEvent", "Input.dispatchKeyEvent", "Input.dispatchTouchEvent":
			if err := t.browser.DispatchInput(env.Method, env.Params); err != nil {
				slog.Debug("tunnel: input dispatch failed", "method", env.Method, "err", err)
			}
		case "TuringTap.done":
			reason = "user_stopped"
			slog.Info("tunnel: handoff ended by user", "handoff_id", hid)
			return nil
		default:
			slog.Debug("tunnel: cdp envelope ignored", "method", env.Method)
		}
	}
}

// dismissCDP notifies live solver bridges that the AI dismissed the handoff
// and tears them down. Called from the browser.handoff RPC (rpc.go).
func (t *Tunnel) dismissCDP(message string) {
	t.cdpMu.Lock()
	conns := make([]*cdpConn, 0, len(t.cdpConns))
	for s := range t.cdpConns {
		conns = append(conns, s)
	}
	t.cdpMu.Unlock()

	params := map[string]any{}
	if message != "" {
		params["message"] = message
	}
	for _, s := range conns {
		s.markDismissed()
		_ = s.sendJSON("TuringTap.dismiss", params)
		_ = s.c.Close() // unblocks serveCDP's decoder → its cleanup runs
	}
	if len(conns) > 0 {
		if cb := t.onHandoffEnd; cb != nil {
			cb("dismissed")
		}
	}
}

func (t *Tunnel) addCDP(s *cdpConn) {
	t.cdpMu.Lock()
	if t.cdpConns == nil {
		t.cdpConns = make(map[*cdpConn]struct{})
	}
	t.cdpConns[s] = struct{}{}
	t.cdpMu.Unlock()
}

func (t *Tunnel) removeCDP(s *cdpConn) {
	t.cdpMu.Lock()
	delete(t.cdpConns, s)
	t.cdpMu.Unlock()
}

func (t *Tunnel) setHandoffRevert(v bool) {
	t.cdpMu.Lock()
	t.handoffRevert = v
	t.cdpMu.Unlock()
}

// takeHandoffRevert returns the revert flag and clears it.
func (t *Tunnel) takeHandoffRevert() bool {
	t.cdpMu.Lock()
	defer t.cdpMu.Unlock()
	v := t.handoffRevert
	t.handoffRevert = false
	return v
}

// cdpConn serialises writes to one CH_CDP stream (frame pump + dismiss can
// race) and remembers whether the AI dismissed it.
type cdpConn struct {
	mu   sync.Mutex
	c    net.Conn
	dism bool
}

// sendJSON marshals one envelope and writes it with a single Write so it
// arrives at the solver as exactly one WebSocket message.
func (s *cdpConn) sendJSON(method string, params any) error {
	body, err := json.Marshal(struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}{method, params})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.c.Write(body)
	return err
}

func (s *cdpConn) markDismissed() {
	s.mu.Lock()
	s.dism = true
	s.mu.Unlock()
}

func (s *cdpConn) dismissed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dism
}

// screencastParams matches what the web (app/solve/page.tsx) and mobile
// (lib/services/relay_ws.dart) solvers read. Data marshals to base64.
type screencastParams struct {
	Data      []byte             `json:"data"`
	SessionID int                `json:"sessionId"`
	Metadata  screencastMetadata `json:"metadata"`
}

type screencastMetadata struct {
	DeviceWidth  float64 `json:"deviceWidth"`
	DeviceHeight float64 `json:"deviceHeight"`
}
