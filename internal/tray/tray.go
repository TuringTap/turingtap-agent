//go:build tray

// Package tray provides the system-tray icon. It requires CGO (GTK on Linux,
// AppKit on macOS) and is therefore gated behind the `tray` build tag so
// `go build ./...` succeeds on hosts without the native toolchain.
package tray

import (
	"github.com/getlantern/systray"
)

// Callbacks wires tray menu actions to the rest of the agent.
type Callbacks struct {
	OnOpenBrowser func()
	OnQuit        func()
}

var (
	setOnlineFn func(bool)
)

// Run starts the systray event loop and blocks until Quit.
func Run(cb Callbacks) {
	systray.Run(func() { onReady(cb) }, func() {})
}

// SetOnline updates the tray title/tooltip to reflect relay connectivity.
func SetOnline(online bool) {
	if setOnlineFn != nil {
		setOnlineFn(online)
	}
}

func onReady(cb Callbacks) {
	systray.SetIcon(iconOffline)
	systray.SetTitle("TuringTap")
	systray.SetTooltip("TuringTap Agent — offline")

	mStatus := systray.AddMenuItem("Offline", "Relay connection status")
	mStatus.Disable()
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("Open browser", "Launch headed Chromium (recorder)")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Stop turingtap-agent")

	setOnlineFn = func(online bool) {
		if online {
			systray.SetIcon(iconOnline)
			systray.SetTooltip("TuringTap Agent — online")
			mStatus.SetTitle("Online")
		} else {
			systray.SetIcon(iconOffline)
			systray.SetTooltip("TuringTap Agent — offline")
			mStatus.SetTitle("Offline")
		}
	}

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				if cb.OnOpenBrowser != nil {
					cb.OnOpenBrowser()
				}
			case <-mQuit.ClickedCh:
				if cb.OnQuit != nil {
					cb.OnQuit()
				}
				systray.Quit()
				return
			}
		}
	}()
}
