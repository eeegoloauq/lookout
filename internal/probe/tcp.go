package probe

import (
	"context"
	"net"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
)

// TCP probes checks of type tcp: it dials the address and hangs up.
//
// This is the check for everything that listens but does not speak HTTP — a
// database, a broker, an SSH daemon, a container that only exposes a port. It
// answers exactly one question, "is something accepting connections here",
// and deliberately sends no bytes: a probe that wrote a protocol greeting
// would show up in the target's logs as a broken client every minute.
type TCP struct {
	dialer *net.Dialer
}

// NewTCP returns a prober. The dialer carries no timeout of its own: the
// deadline comes from the check, through the context.
func NewTCP() *TCP { return &TCP{dialer: &net.Dialer{}} }

// Probe dials once. Like every probe it never returns an error: a refused
// connection is a result.
func (p *TCP) Probe(ctx context.Context, c config.Check) check.Result {
	res := check.Result{Name: c.Name, At: time.Now()}

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	conn, err := p.dialer.DialContext(ctx, "tcp", c.Address)
	res.Duration = time.Since(res.At)
	if err != nil {
		res.Outcome = check.OutcomeDown
		res.Err = redact(err)
		return res
	}
	// The address that actually answered, for the same reason the HTTP probe
	// records it: it is free, and it answers "am I still talking to the old
	// box" after a migration.
	res.RemoteAddr = conn.RemoteAddr().String()
	_ = conn.Close()

	res.Outcome, res.Failures = check.Evaluate(c.Expect, check.Response{Duration: res.Duration})
	return res
}
