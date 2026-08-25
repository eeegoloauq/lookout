// Package alert delivers state-change events to an operator.
//
// Events are written to a durable outbox first and only removed after the
// notifier confirms delivery. A monitor that can lose a notification is a
// monitor whose silence cannot be trusted.
package alert

import "context"

// Notifier sends a pre-formatted alert. There is one production
// implementation (Telegram); tests supply their own. Formatting lives
// outside the transport so a second notifier would not re-implement it.
type Notifier interface {
	Notify(ctx context.Context, text string) error
}
