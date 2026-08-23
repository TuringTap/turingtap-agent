//go:build !tray

// Package tray provides the system-tray icon. This is the no-op stub compiled
// when the `tray` build tag is absent (CI, headless hosts, or platforms
// without the CGO toolchain for getlantern/systray).
package tray

// Callbacks wires tray menu actions to the rest of the agent.
type Callbacks struct {
	OnOpenBrowser func()
	OnQuit        func()
}

// Run is a no-op without the tray tag; it blocks forever so main can treat it
// uniformly. Callers should run it in a goroutine.
func Run(Callbacks) { select {} }

// SetOnline is a no-op without the tray tag.
func SetOnline(bool) {}
