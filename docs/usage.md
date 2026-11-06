# Checks, alerts and the status page

## Checks

- **http**: status code, response time, a substring of the body, or a JSON path compared with a
  value. A JSON path that no longer resolves reports `malformed` instead of `down`.
- **tcp**: connects to `host:port` and closes the connection without sending anything. For
  databases, brokers, SSH and other services that only expose a port.
- **dns**: A, AAAA, MX, NS or TXT against a resolver you choose. A lost UDP packet is retried. An
  answer that differs from the first one seen is reported as drift; SERVFAIL fails the check.
- **push**: a dead-man switch for jobs such as backups and renewals. The job calls lookout when it
  finishes, and the check goes down when a call does not arrive in time.
- **domain**: registration expiry over RDAP, or WHOIS where the TLD has no RDAP, checked once a
  day. A domain check is added automatically for every host your http and dns checks use; declare
  one yourself to set its interval or group.
- TLS certificate expiry is read from the handshake of an `https` http check. There is no separate
  check type for it.

A push check:

```yaml
  - name: ZFS tank
    group: Jobs
    type: push
    expect_every: 10m                   # deadline between pings
    grace: 2m                           # optional, default 0
    token: ${LOOKOUT_PUSH_TOKEN_ZFS}    # secret part of the ping URL
```

```sh
# crontab: ping after the job succeeds
0 3 * * * /usr/local/bin/backup.sh && curl -fsS "$LOOKOUT_PUSH_URL"
# or report the failure
0 3 * * * /usr/local/bin/backup.sh || curl -fsS "$LOOKOUT_PUSH_URL?status=fail&msg=backup+failed"
```

The URL is `http://<listen>/api/push/<token>`, GET or POST, and answers 204. An unknown token gets
a plain 404. `msg` is cut to 200 characters and shown on the page. The deadline is checked once a
minute and a missed ping goes through the same `failure_threshold` as other checks, so the check
turns DOWN a few minutes after the deadline. Unlike `mute`, this endpoint is not limited to
loopback, so `listen:` must be reachable from the machine running the job.

## Alerts

A check goes down after a set number of consecutive failures and comes back after a set number of
consecutive successes. A second detector catches a service that keeps flapping between the two.

Alerts go to Telegram:

- Changes within a short window are sent as one message.
- An outage that stays open is repeated on the schedule you set.
- Once a week lookout sends a message that it is still running.
- Every event is written to a durable queue first and removed only after Telegram confirms
  delivery.

Quiet hours are set with `mute:` in the config, or ad hoc with
`lookout mute --for 2h --group Public`. Checks keep running while muted. What happened during the
mute is sent as one summary when it ends.

## Status page and API

<picture>
  <source media="(prefers-color-scheme: light)" srcset="detail-light.png">
  <img alt="A check expanded to show why it failed" src="detail-dark.png">
</picture>

Each row opens to show what the check watches, when it failed and with what error, uptime over
three windows, the response time distribution, the last 30 days and the last 24 hours. The page is
server-rendered HTML with no framework and no CDN assets.

- `/api/status`: versioned JSON.
- `/metrics`: Prometheus text format.
- `/healthz`: 503 when alerts are queued and not delivered.

## Limits

- All probes run from the lookout host, so it cannot report its own host going down. Use an
  external monitor for that.
- Alerts go to Telegram only.
- Checks are added in the config, not on the page.
- The page reloads itself with an inline script, or with a meta refresh when JavaScript is off.
  Rows open through a checkbox and `:has()`; browsers without `:has()` show the board but cannot
  open rows.
