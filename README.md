# lookout

A small uptime monitor for a homelab: HTTP checks now, DNS, TLS and domain
expiry to follow. One static binary, no runtime dependencies, no database, a
declarative YAML config.

Its founding rule: **silence means everything is fine, and silence must never
be a bug.** Alerting is on for every check unless the check says
`alert: false`, and no loss of state may turn into a false alert or a missing
one.

Status: early development. `SPEC.md` is the design (in Russian); this release
covers the check model, the HTTP probe, the threshold and instability state
machine, durable state, the recent-history ring, and Telegram alert delivery.

## Build

```sh
CGO_ENABLED=0 go build -o lookout ./cmd/lookout
```

## Use

```sh
lookout validate config.yaml    # every problem, with the line it is on
lookout run [-v] config.yaml    # probe until interrupted
```

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
  message, grouped by check group. The HTTP client speaks SOCKS5 because
  `api.telegram.org` is often unreachable directly. Token and chat id come
  from `LOOKOUT_TELEGRAM_TOKEN` and `LOOKOUT_TELEGRAM_CHAT_ID` — never from
  the config file.

The status page, the API, DNS and domain checks are not in this release.

## Tests

```sh
go vet ./... && go test -race ./...
```

Everything clock-driven is tested inside a `testing/synctest` bubble on a fake
clock, so schedules and cooldowns are exercised over simulated hours without a
single sleep.
