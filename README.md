# lookout

A small uptime monitor for a homelab: HTTP, DNS and domain-registration
checks, TLS and registry expiry from the work already being done. One
static binary, no runtime dependencies, no database, a declarative YAML
config.

Its founding rule: **silence means everything is fine, and silence must never
be a bug.** Alerting is on for every check unless the check says
`alert: false`, and no loss of state may turn into a false alert or a missing
one.

Status: early development. `SPEC.md` is the design (in Russian); this release
covers the check model, HTTP/DNS/domain probes, the threshold and instability
state machine, durable state, the recent-history ring, Telegram alert delivery,
the status page / API / metrics, mute windows, long-term JSONL history, and a
hardened systemd unit.

## Build

```sh
CGO_ENABLED=0 go build -o lookout ./cmd/lookout
```

## Use

```sh
lookout validate config.yaml    # every problem, with the line it is on
lookout run [-v] config.yaml    # probe and serve the status page until interrupted
lookout mute --for 30m --group Public
lookout unmute
```

`lookout run` listens on `listen` in the config (default `127.0.0.1:5665`):

- `GET /` — status page: one HTML table, no JavaScript, no external assets
- `GET /api/status` — JSON of every check (versioned; this is the public contract)
- `GET /api/checks/<name>` — recent probe history from the in-memory ring
- `GET /metrics` — Prometheus text format
- `GET /healthz` — process liveness; **degraded** (HTTP 503) if alert delivery is stuck

Authorization headers from the config are never serialized. URLs that carry
userinfo have the credentials masked.

`config.example.yaml` documents the format. Secrets are never written in the
config: `${VAR}` reads them from the environment, and an unset variable is a
validation error rather than a silently empty header.

## What it does today

- **HTTP checks** with typed conditions: status (`200`, `"200-299"`, `"<500"`),
  response time, a substring of the body, and JSON paths compared to expected
  values. Conditions are compiled when the config loads, so a typo fails
  validation instead of a probe.
- **A separate `malformed` outcome** when a body path does not resolve: an API
  that changed shape is not an outage and must not read like one.
- **Thresholds** (`failure_threshold`, `success_threshold`) plus an
  **"N of the last M" window**, because a single success resets a consecutive
  counter — a check alternating up and down delivers half its requests and
  would otherwise never alert at all.
- **Durable state** in one atomically written JSON file, and 24 hours of recent
  results per check in memory.
- **A scheduler** that computes ticks from a fixed origin, offset by a hash of
  the check name: no drift behind a slow target, no thundering herd, and the
  same spread after every restart.
- **Durable alert delivery** to Telegram. A state change is written to an
  outbox in the state file before anyone tries to send it, and leaves the
  queue only after the Bot API confirms delivery. Retries use exponential
  backoff; a full queue collapses into a summary instead of dropping events.
  Events that mature inside `batch_window` (default 45s) leave as one
  message: one emoji for the worst of them, a counting headline, one line per
  check. An outage that stays open repeats on an escalating schedule
  (`reminders`, default 1h/4h/then daily). The HTTP client speaks SOCKS5 because
  `api.telegram.org` is often unreachable directly. Token and chat id come
  from `LOOKOUT_TELEGRAM_TOKEN` and `LOOKOUT_TELEGRAM_CHAT_ID` — never from
  the config file.

- **A status page and JSON API** on loopback by default. `/api/status` is
  versioned in the body (`version: 1`) so a separate start page can consume
  it. `/healthz` is 503 when the alert outbox has failed several deliveries:
  a monitor that cannot notify must look sick from the outside.
- **Prometheus metrics** for the last probe (`lookout_probe_success`,
  `lookout_probe_duration_seconds`), confirmed state (`lookout_up`), 24h
  uptime, certificate and domain days left, and outbox health
  (`lookout_undelivered_alert_age_seconds`).
- **DNS checks** (`type: dns`) for A/AAAA/MX/NS/TXT against a resolver.
  UDP queries are retried: one lost datagram is not an outage. A change
  in the answer snapshot is zone drift; the first probe is a baseline,
  not an alert.
- **TLS certificate expiry** from the HTTPS handshake of an `http` check.
  No extra request, no extra check type. Tiers 21/14/7/3 days, then daily;
  each threshold fires once until the cert is renewed.
- **Domain registration expiry** (`type: domain`): RDAP at the registry
  listed in IANA's bootstrap, WHOIS on TCP/43 otherwise (including `.ru`
  at whois.tcinet.ru, field `paid-till`). A silent registry is `unknown`,
  not down; that becomes an alert only after three days. Tiers 60/30/14/7
  days, then daily. Losing the `.ru` `DELEGATED` flag is its own event,
  fired once per transition.
- **Mute windows.** Static schedules in the config (`every` + `at` +
  `duration`) and ad-hoc `lookout mute --for 30m --group Public`, which
  talks to the running process over its HTTP listen address (loopback by
  default). Probes and history keep running; only delivery is suppressed.
  Events that fire while muted become one digest when the mute lifts —
  they are not dropped. The mute is visible on the status page and in
  `/api/status`, and it survives a restart.
- **Long-term history** — one JSON Lines record per check per UTC day
  (uptime, incidents, p50/p95). The in-progress day lives in the state
  file so a restart neither duplicates nor loses it.

## Deploy

The unit file is `contrib/systemd/lookout.service`. It runs as a dedicated
`lookout` user with `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`,
`PrivateTmp`, `RestrictAddressFamilies`, and an empty `CapabilityBoundingSet`.
Secrets come from an `EnvironmentFile` that you create with mode 0600.

```sh
CGO_ENABLED=0 go build -o lookout ./cmd/lookout
install -o root -g root -m 0755 lookout /usr/bin/lookout
useradd --system --home /var/lib/lookout --shell /usr/sbin/nologin lookout
install -d -o lookout -g lookout -m 0750 /var/lib/lookout
install -d -o root -g lookout -m 0750 /etc/lookout
# config.yaml is yours — real hosts do not belong in this repository
install -o root -g lookout -m 0640 config.yaml /etc/lookout/config.yaml
install -o root -g lookout -m 0600 contrib/lookout.env.example /etc/lookout/lookout.env
# edit /etc/lookout/lookout.env (token, chat id) then:
install -o root -g root -m 0644 contrib/systemd/lookout.service /etc/systemd/system/lookout.service
systemctl daemon-reload
systemctl enable --now lookout
```

`state.file` and `state.history` in the config should point at
`/var/lib/lookout/` so `ProtectSystem=strict` still lets lookout write.

## Tests

```sh
go vet ./... && go test -race ./...
```

Everything clock-driven is tested inside a `testing/synctest` bubble on a fake
clock, so schedules and cooldowns are exercised over simulated hours without a
single sleep.
