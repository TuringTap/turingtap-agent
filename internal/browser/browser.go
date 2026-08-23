// Package browser manages the single persistent Chromium instance driven by
// the cloud MCP tools (goto/act) and shared with human handoff (ask_human).
// Chromium is proxied through proxy.turingtap.ai and trusts only the TuringTap
// MITM CA via --ignore-certificate-errors-spki-list.
//
// go-rod downloads a Chromium binary on first launch; that happens at runtime,
// never during `go build`.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Action is the verb accepted by Act.
type Action string

const (
	ActClick Action = "click"
	ActFill  Action = "fill"
	ActPress Action = "press"
)

// Options configures Chromium launch.
type Options struct {
	ProxyURL  string // --proxy-server
	ProxyAuth string // token supplied on 407 (Basic username)
	CASPKI    string // --ignore-certificate-errors-spki-list
	Headless  bool   // default true
	CI        bool   // adds --no-sandbox --disable-dev-shm-usage
}

// Browser wraps a go-rod Browser + its primary Page.
//
// Pages live inside an incognito browser context (inc) so cookies, storage
// and cache are session-scoped: ResetSession disposes the context and mints a
// fresh one without relaunching the (single) Chromium process.
type Browser struct {
	opts Options

	mu       sync.Mutex
	launcher *launcher.Launcher
	rod      *rod.Browser
	inc      *rod.Browser // incognito context holding the primary page
	page     *rod.Page
	headed   bool
	// stopScreencast cancels the active screencast's event loop (nil when no
	// screencast is running). ResetSession fires it before closing the page.
	stopScreencast context.CancelFunc
}

// New constructs a Browser manager. Nothing is launched until Launch or the
// first Goto/Act/Screenshot call.
func New(opts Options) *Browser {
	return &Browser{opts: opts}
}

// Launch starts Chromium now (downloading it on first run). Call this before
// bringing the relay tunnel up so "agent online" implies "browser ready" and
// the first CH_RPC goto doesn't sit behind a 150 MB download.
func (b *Browser) Launch() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ensure()
}

// ensure launches Chromium if not already running, in headless or headed mode
// as currently configured.
func (b *Browser) ensure() error {
	if b.rod != nil {
		return nil
	}
	l := launcher.New().
		Headless(b.opts.Headless && !b.headed).
		Set("proxy-server", b.opts.ProxyURL).
		// Route loopback/link-local through the proxy too — the whole point
		// of the agent is that "LAN" targets transit the cloud MITM.
		Set("proxy-bypass-list", "<-loopback>")
	if b.opts.CASPKI != "" {
		l = l.Set("ignore-certificate-errors-spki-list", b.opts.CASPKI)
	}
	if b.opts.CI {
		l = l.NoSandbox(true).Set("disable-dev-shm-usage").Set("disable-gpu")
	}
	url, err := l.Launch()
	if err != nil {
		return fmt.Errorf("browser: launch: %w", err)
	}
	br := rod.New().ControlURL(url)
	if err := br.Connect(); err != nil {
		return fmt.Errorf("browser: connect: %w", err)
	}
	inc, err := br.Incognito()
	if err != nil {
		_ = br.Close()
		return fmt.Errorf("browser: incognito context: %w", err)
	}
	page, err := inc.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		_ = br.Close()
		return fmt.Errorf("browser: page: %w", err)
	}
	b.launcher = l
	b.rod = br
	b.inc = inc
	b.page = page
	if b.opts.ProxyAuth != "" {
		if err := b.startProxyAuth(); err != nil {
			return fmt.Errorf("browser: proxy auth: %w", err)
		}
	}
	return nil
}

// startProxyAuth enables the CDP Fetch domain browser-wide and answers every
// authRequired challenge with (ProxyAuth, "x"). Chromium can't carry proxy
// credentials on --proxy-server, so this is the only reliable channel.
// FetchEnable also pauses every request; those are continued verbatim.
func (b *Browser) startProxyAuth() error {
	if err := (proto.FetchEnable{HandleAuthRequests: true}).Call(b.rod); err != nil {
		return err
	}
	go b.rod.EachEvent(
		func(e *proto.FetchRequestPaused) {
			_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(b.rod)
		},
		func(e *proto.FetchAuthRequired) {
			_ = proto.FetchContinueWithAuth{
				RequestID: e.RequestID,
				AuthChallengeResponse: &proto.FetchAuthChallengeResponse{
					Response: proto.FetchAuthChallengeResponseResponseProvideCredentials,
					Username: b.opts.ProxyAuth,
					Password: "x",
				},
			}.Call(b.rod)
		},
	)()
	return nil
}

// Close shuts the browser down.
func (b *Browser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rod == nil {
		return nil
	}
	b.cancelScreencastLocked()
	err := b.rod.Close()
	if b.launcher != nil {
		b.launcher.Cleanup()
	}
	b.rod, b.inc, b.page, b.launcher = nil, nil, nil, nil
	return err
}

// ResetSession drops all browser state accumulated by the current MCP session
// — cookies, localStorage/sessionStorage, cache, open pages — by disposing the
// incognito browser context and creating a fresh one with a new about:blank
// page. The Chromium process itself stays up (no relaunch), so resets are
// fast. Any active screencast is torn down first: its event loop is cancelled
// before the page it captures disappears, so a live handoff bridge simply
// stops receiving frames rather than erroring.
//
// If the browser was never launched this is a no-op — the next ensure()
// starts clean by construction.
func (b *Browser) ResetSession() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rod == nil {
		return nil
	}
	b.cancelScreencastLocked()
	if b.inc != nil {
		// Disposes the browser context: closes its pages and evaporates
		// cookies/storage/cache (rod's Close on an incognito handle maps to
		// Target.disposeBrowserContext).
		_ = b.inc.Close()
	}
	inc, err := b.rod.Incognito()
	if err == nil {
		var page *rod.Page
		if page, err = inc.Page(proto.TargetCreateTarget{URL: "about:blank"}); err == nil {
			b.inc, b.page = inc, page
			return nil
		}
	}
	// Context or page creation failed: the Chromium connection is likely
	// gone. Tear everything down so the next call relaunches from scratch.
	_ = b.rod.Close()
	if b.launcher != nil {
		b.launcher.Cleanup()
	}
	b.rod, b.inc, b.page, b.launcher = nil, nil, nil, nil
	return fmt.Errorf("browser: reset: %w", err)
}

// cancelScreencastLocked stops the active screencast event loop, if any.
// Callers must hold b.mu.
func (b *Browser) cancelScreencastLocked() {
	if b.stopScreencast != nil {
		b.stopScreencast()
		b.stopScreencast = nil
	}
}

// Goto navigates the primary page and returns (finalURL, title, PNG screenshot).
func (b *Browser) Goto(ctx context.Context, url string) (string, string, []byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensure(); err != nil {
		return "", "", nil, err
	}
	p := b.page.Context(ctx)
	if err := p.Navigate(url); err != nil {
		return "", "", nil, err
	}
	if err := p.WaitLoad(); err != nil {
		return "", "", nil, err
	}
	info, err := p.Info()
	if err != nil {
		return "", "", nil, err
	}
	shot, err := p.Screenshot(false, nil)
	if err != nil {
		return "", "", nil, err
	}
	return info.URL, info.Title, shot, nil
}

// Act performs a click/fill/press on the element matching selector and returns
// a fresh PNG screenshot.
func (b *Browser) Act(ctx context.Context, action Action, selector, value string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensure(); err != nil {
		return nil, err
	}
	p := b.page.Context(ctx)

	switch action {
	case ActClick:
		el, err := p.Element(selector)
		if err != nil {
			return nil, err
		}
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return nil, err
		}
	case ActFill:
		el, err := p.Element(selector)
		if err != nil {
			return nil, err
		}
		if err := el.Input(value); err != nil {
			return nil, err
		}
	case ActPress:
		k, ok := keymap[value]
		if !ok {
			return nil, fmt.Errorf("browser: unknown key %q", value)
		}
		if err := p.Keyboard.Press(k); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("browser: unknown action %q", action)
	}

	return p.Screenshot(false, nil)
}

// Screenshot returns a PNG of the current page.
func (b *Browser) Screenshot(ctx context.Context) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensure(); err != nil {
		return nil, err
	}
	return b.page.Context(ctx).Screenshot(false, nil)
}

// Headed reports whether the browser is currently in headed (visible) mode.
func (b *Browser) Headed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.headed
}

// SetHeaded flips the browser to headed mode (relaunching if necessary) and
// injects an overlay banner showing the human-handoff prompt. Passing an empty
// prompt returns to headless.
//
// Under Options.CI there is usually no display, so the headed relaunch is
// skipped entirely: Chromium stays headless (CDP screencast works headless)
// and only the banner is injected. This also keeps the current page alive,
// which the e2e handoff test depends on.
func (b *Browser) SetHeaded(prompt string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	want := prompt != ""
	if want && b.opts.CI {
		slog.Warn("browser: headed mode unavailable under CI — staying headless")
		if err := b.ensure(); err != nil {
			return err
		}
		return b.injectBanner(prompt)
	}
	if b.rod != nil && b.headed != want {
		// Relaunch in the new mode. Cookie jar / state persistence across
		// relaunch is handled by user-data-dir; for v1 we accept a fresh
		// profile on mode flip. TODO: persistent profile dir.
		b.cancelScreencastLocked()
		_ = b.rod.Close()
		if b.launcher != nil {
			b.launcher.Cleanup()
		}
		b.rod, b.inc, b.page, b.launcher = nil, nil, nil, nil
	}
	b.headed = want
	if err := b.ensure(); err != nil {
		return err
	}
	if want {
		return b.injectBanner(prompt)
	}
	// Reverting without a relaunch (CI banner-only mode): drop the banner.
	b.removeBanner()
	return nil
}

func (b *Browser) injectBanner(prompt string) error {
	js := `(prompt) => {
		let bar = document.getElementById('__tt_banner');
		if (!bar) {
			bar = document.createElement('div');
			bar.id = '__tt_banner';
			bar.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:2147483647;'+
				'background:#111;color:#fff;font:14px system-ui;padding:10px 14px;'+
				'display:flex;align-items:center;gap:12px;';
			bar.innerHTML = '<span id="__tt_prompt"></span>'+
				'<span style="color:#f43;font-weight:600">● REC</span>'+
				'<button id="__tt_done" style="margin-left:auto;padding:4px 12px">Done</button>';
			document.documentElement.appendChild(bar);
		}
		document.getElementById('__tt_prompt').textContent = prompt;
	}`
	_, err := b.page.Eval(js, prompt)
	return err
}

func (b *Browser) removeBanner() {
	if b.page == nil {
		return
	}
	_, _ = b.page.Eval(`() => document.getElementById('__tt_banner')?.remove()`)
}

// Frame is one screencast frame: raw JPEG bytes plus the CDP session id and
// the remote viewport size solvers need for coordinate scaling.
type Frame struct {
	Data         []byte
	SessionID    int
	DeviceWidth  float64
	DeviceHeight float64
}

// StartScreencast begins a CDP Page.startScreencast and pushes JPEG frames to
// frameCh until ctx is done. Chromium's per-frame ack is sent here, after the
// consumer takes the frame off the channel — that paces capture to the
// consumer's throughput.
//
// The screencast is also cancellable from the browser side: ResetSession
// fires the stored cancel before it disposes the page, so the event loop
// below never outlives its capture target.
func (b *Browser) StartScreencast(ctx context.Context, frameCh chan<- Frame) error {
	b.mu.Lock()
	if err := b.ensure(); err != nil {
		b.mu.Unlock()
		return err
	}
	p := b.page
	b.cancelScreencastLocked() // at most one screencast at a time
	ctx, cancel := context.WithCancel(ctx)
	b.stopScreencast = cancel
	b.mu.Unlock()

	quality, maxWidth, everyNth := 60, 1280, 1
	if err := (proto.PageStartScreencast{
		Format:        proto.PageStartScreencastFormatJpeg,
		Quality:       &quality,
		MaxWidth:      &maxWidth,
		EveryNthFrame: &everyNth,
	}).Call(p); err != nil {
		return err
	}

	// Event loop bound to ctx so it terminates on cancel; acks and the final
	// stop go to the unbound page handle.
	pc := p.Context(ctx)
	go pc.EachEvent(func(e *proto.PageScreencastFrame) {
		f := Frame{Data: e.Data, SessionID: e.SessionID}
		if e.Metadata != nil {
			f.DeviceWidth = e.Metadata.DeviceWidth
			f.DeviceHeight = e.Metadata.DeviceHeight
		}
		select {
		case frameCh <- f:
		case <-ctx.Done():
			return
		}
		_ = proto.PageScreencastFrameAck{SessionID: e.SessionID}.Call(p)
	})()

	go func() {
		<-ctx.Done()
		_ = proto.PageStopScreencast{}.Call(p)
	}()
	return nil
}

// DispatchInput forwards a raw CDP Input.* event (as JSON) from the mobile
// solver to the page. method must be one of Input.dispatchMouseEvent,
// Input.dispatchTouchEvent, Input.dispatchKeyEvent.
func (b *Browser) DispatchInput(method string, params json.RawMessage) error {
	b.mu.Lock()
	if err := b.ensure(); err != nil {
		b.mu.Unlock()
		return err
	}
	p := b.page
	b.mu.Unlock()

	switch method {
	case "Input.dispatchMouseEvent":
		var m proto.InputDispatchMouseEvent
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		return m.Call(p)
	case "Input.dispatchTouchEvent":
		var m proto.InputDispatchTouchEvent
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		return m.Call(p)
	case "Input.dispatchKeyEvent":
		var m proto.InputDispatchKeyEvent
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		return m.Call(p)
	default:
		return errors.New("browser: unsupported input method")
	}
}

var keymap = map[string]input.Key{
	"Enter":     input.Enter,
	"Tab":       input.Tab,
	"Escape":    input.Escape,
	"Backspace": input.Backspace,
	"ArrowUp":   input.ArrowUp,
	"ArrowDown": input.ArrowDown,
}
