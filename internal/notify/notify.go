// Package notify shows OS toast notifications (e.g. handoff start/end).
package notify

import (
	"github.com/gen2brain/beeep"
)

// Toast displays an OS notification.
func Toast(title, body string) error {
	return beeep.Notify(title, body, "")
}
