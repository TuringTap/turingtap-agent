package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/turingtap/agent/internal/browser"
)

// BrowserRPC is the subset of browser.Browser the tunnel needs to serve
// CH_RPC and CH_CDP streams from the relay.
type BrowserRPC interface {
	Goto(ctx context.Context, url string) (finalURL, title string, png []byte, err error)
	Act(ctx context.Context, action browser.Action, selector, value string) (png []byte, err error)
	Screenshot(ctx context.Context) (png []byte, err error)
	SetHeaded(prompt string) error
	Headed() bool
	StartScreencast(ctx context.Context, frameCh chan<- browser.Frame) error
	DispatchInput(method string, params json.RawMessage) error
	ResetSession() error
}

// SetBrowser wires the browser handler used for browser.* RPCs. Until called,
// those RPCs return an error.
func (t *Tunnel) SetBrowser(b BrowserRPC) { t.browser = b }

// serveRPC handles one CH_RPC stream: 4-byte big-endian length + JSON
// {"method","params"} in, same framing with {"result"} or {"error"} out,
// then the stream is closed. Matches services/relay/server.py AgentConn.rpc.
func (t *Tunnel) serveRPC(c io.ReadWriteCloser) error {
	var lb [4]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n > 1<<20 {
		return fmt.Errorf("rpc: request too large (%d)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return err
	}

	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(buf, &req); err != nil {
		return writeRPC(c, nil, fmt.Sprintf("bad json: %v", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	result, err := t.dispatchRPC(ctx, req.Method, req.Params)
	if err != nil {
		slog.Warn("tunnel: rpc failed", "method", req.Method, "err", err)
		return writeRPC(c, nil, err.Error())
	}
	slog.Debug("tunnel: rpc ok", "method", req.Method)
	return writeRPC(c, result, "")
}

func (t *Tunnel) dispatchRPC(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "agent.status":
		return map[string]any{"online": t.Online()}, nil

	case "browser.goto":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if t.browser == nil {
			return nil, fmt.Errorf("browser not configured")
		}
		u, title, png, err := t.browser.Goto(ctx, p.URL)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"current_url":    u,
			"title":          title,
			"screenshot_b64": base64.StdEncoding.EncodeToString(png),
		}, nil

	case "browser.act":
		var p struct {
			Action   string `json:"action"`
			Selector string `json:"selector"`
			Value    string `json:"value"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if t.browser == nil {
			return nil, fmt.Errorf("browser not configured")
		}
		png, err := t.browser.Act(ctx, browser.Action(p.Action), p.Selector, p.Value)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ok":             true,
			"screenshot_b64": base64.StdEncoding.EncodeToString(png),
		}, nil

	case "browser.screenshot":
		if t.browser == nil {
			return nil, fmt.Errorf("browser not configured")
		}
		png, err := t.browser.Screenshot(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"screenshot_b64": base64.StdEncoding.EncodeToString(png),
		}, nil

	case "browser.reset":
		// Fresh browser state for a new MCP session (and evaporation at the
		// end of one): drop the incognito context — cookies, storage, cache,
		// pages — without relaunching Chromium.
		if t.browser == nil {
			return nil, fmt.Errorf("browser not configured")
		}
		if err := t.browser.ResetSession(); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	case "browser.handoff":
		var p struct {
			HandoffID *string `json:"handoff_id"`
			Prompt    *string `json:"prompt"`
			OpenURL   *string `json:"open_url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if t.browser == nil {
			return nil, fmt.Errorf("browser not configured")
		}
		if p.HandoffID != nil && p.Prompt != nil {
			// Handoff start: remember the prompt for late CH_CDP bridges and
			// whether we (not the user) flipped the browser headed.
			t.cdpMu.Lock()
			t.handoffPrompt = *p.Prompt
			t.cdpMu.Unlock()
			if !t.browser.Headed() {
				t.setHandoffRevert(true)
			}
			if err := t.browser.SetHeaded(*p.Prompt); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		}
		// Handoff end (dismiss_human / session close): tell live solvers,
		// then revert headless.
		msg := ""
		if p.Prompt != nil {
			msg = *p.Prompt
		}
		t.dismissCDP(msg)
		t.setHandoffRevert(false)
		if err := t.browser.SetHeaded(""); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
}

func writeRPC(w io.Writer, result any, errMsg string) error {
	var body []byte
	if errMsg != "" {
		body, _ = json.Marshal(map[string]any{"error": errMsg})
	} else {
		body, _ = json.Marshal(map[string]any{"result": result})
	}
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(body)))
	if _, err := w.Write(lb[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}
