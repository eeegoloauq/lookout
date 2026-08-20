# Why lookout is built this way

Most of these decisions cost something. This is the record of what they cost and
what they bought, so that a change here is an argument rather than a preference.

## Silence is the product

The monitor this one replaced ran with alerting that looked configured and was
not. An outage went six and a half hours before a human noticed. Nothing was
broken; a default was wrong, and a wrong default is invisible.

So alerting is on for every check, and turning it off takes a line of config that
says so. `alert: false` is a thing you write, never a thing you forget. Every
piece of the delivery path is built around the same idea: a state change is
written to a durable outbox before anything tries to send it, and it leaves the
queue only when the Telegram API confirms it arrived. A crash between the two
resends. A dropped message is the one bug this program cannot have.

The same reasoning produced `alerting.heartbeat`. A monitor that has died is
indistinguishable from a monitor with nothing to report, so once a week it says
it is alive. If that message stops arriving, the silence has become information.

## No database

lookout keeps confirmed state in one JSON file, written atomically, and 24 hours
of recent probes in memory, seeded on start from a compact JSONL file. Long-term
history is one line per check per UTC day.

SQLite was the obvious alternative and was rejected. The data is a few hundred
kilobytes and is written by exactly one process; a database would buy indexes
nobody queries, a file format nobody can read with `cat`, and a corruption mode
that needs a recovery procedure. Losing the state file costs history, not
correctness — lookout re-learns every check's state within a few probes.

The cost is real: there is no query language, and retention is a truncation
rather than a `DELETE`. If you ever want ad-hoc analysis of a year of data, the
JSONL is what you would load somewhere else.

## Thresholds are not enough

Consecutive-failure counting is the standard way to avoid alerting on one lost
packet, and it has a hole: any single success resets the counter. A check
alternating up, down, up, down fails half its requests and never alerts.

So there is a second detector, "N failures in the last M probes", with its own
cooldown. It is what catches the flapping service, and it is why the page has an
UNSTABLE state that is neither up nor down.

There is deliberately no third status for "degraded". Latency that is high but
under the threshold is a number on the page, not a state. Three states are
already at the edge of what a glance can carry.

## Probes tell the truth about the whole path

Keep-alives are disabled in the HTTP probe. A reused connection skips DNS
resolution, the TCP handshake and the TLS handshake — the layers a check exists
to cover — and the certificate expiry lookout reads out of the handshake would
never be observed again after the first probe. Every check pays for a full
connection every time. On a homelab that is a rounding error; on ten thousand
checks it would not be.

Probes ignore `HTTP_PROXY` and the rest of the environment. The set of hosts
lookout talks to is closed and explicit: the target, the resolver in the config,
the RDAP or WHOIS server for a domain, and the Telegram API. An inherited proxy
variable would silently reroute every probe and make a green check meaningless.

A `malformed` outcome is separate from `down`. A body that no longer contains the
field the check asserts on means the other side changed shape, which is not the
same event as the service being unreachable and must not read the same in an
alert.

A `tcp` check sends nothing. It dials, notes what answered and closes. Writing a
protocol greeting would make the target log a broken client every minute for as
long as the check exists.

## Expiry comes free or not at all

TLS expiry is read from the handshake an `https` check already performs. There is
no certificate check type, because there is no extra work to justify one.

Domain registration is the exception that needs its own probe, and it is derived
rather than declared: lookout works out the registrable name behind every check's
host, using the ICANN section of the public suffix list, and watches each name
once a day. Ten checks under one domain make one registry query and one warning.
A registry that does not answer is `unknown`, not down; that only becomes an
alert after three days of silence.

## The page is a board

It renders on the server, ships no JavaScript worth the name, loads no font, no
icon set and no stylesheet from anywhere, and reloads itself once a minute. A
status page that cannot render when a CDN is down is not a status page.

It shows one line per check and opens into the detail — what it watches, when it
broke, why, uptime over three windows, the latency spread, and a bar of the last
24 hours. Each row is shown in the units it actually measures: a domain
registration has no response time and no daily uptime, so it leads with the date
it runs out.

There is no way to add a check from the UI, and there will not be. The config is
in git; a check that exists only in a running process is a check nobody can
review, diff or restore.

## Deliberately not here

Graphs beyond the 24-hour bar. Multi-step scenarios. A second notification
channel. Multi-user anything. Watching the monitor from outside its own network —
which is a real gap, not an argument that it does not matter: every probe leaves
from one machine, and if that machine is the thing that is down, nobody is
watching.
