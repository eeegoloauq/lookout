# lookout

A lightweight uptime monitor in Go: HTTP, TCP and DNS checks, a status page, Telegram alerts.

<picture>
  <source media="(prefers-color-scheme: light)" srcset="docs/board-light.png">
  <img alt="The lookout status board" src="docs/board-dark.png">
</picture>

## Try it

```sh
go run github.com/eeegoloauq/lookout/cmd/lookout@latest demo
```

The board above on `127.0.0.1:5665`, filled with invented data. Nothing is
probed and no configuration is read.

## Run it

```sh
CGO_ENABLED=0 go build -o lookout ./cmd/lookout
cp config.example.yaml config.yaml    # edit it
./lookout validate config.yaml
./lookout run config.yaml
```

Or as a container:

```sh
docker run -d --name lookout \
  -p 5665:5665 \
  -v $PWD/config.yaml:/etc/lookout/config.yaml:ro \
  -v lookout-state:/var/lib/lookout \
  -e LOOKOUT_TELEGRAM_TOKEN -e LOOKOUT_TELEGRAM_CHAT_ID \
  ghcr.io/eeegoloauq/lookout:latest
```

Configuration is one YAML file, and `config.example.yaml` is its reference:
every option is in it with a comment saying what it is for. `validate` reports
every problem at once with the line each one is on, and `run` refuses to start
on a config that does not pass.

## What it checks

**http** — status code, response time, a substring of the body, or a JSON path
compared against a value. A body path that stops resolving reports `malformed`
rather than `down`.

**tcp** — dials a `host:port` and hangs up without sending anything: a
database, a broker, an SSH daemon, a container that exposes nothing but a port.

**dns** — A, AAAA, MX, NS or TXT against a resolver you name. A lost UDP packet
is retried rather than counted, and a changed answer set is reported as drift
against the first answer seen. SERVFAIL fails the check instead of counting as
drift.

**TLS and domain expiry** — certificate dates are read out of the handshake an
`https` check already performs, and registration dates are looked up once a day
over RDAP (WHOIS where there is no RDAP) for the names your checks already
point at. Neither needs a second block of config.

## When something breaks

A check goes down after a few consecutive failures and comes back after a few
consecutive successes; both counts are yours to set. A second detector catches
the service that alternates rather than fails, which a consecutive counter
never sees.

The message goes to Telegram. Everything that changed within a short window
leaves as one message, an outage that stays open repeats on a schedule you
choose, and once a week lookout reports that it is still running. Every event
is written to a durable queue before anything tries to send it, and leaves that
queue only once Telegram confirms it arrived.

Quiet hours are a schedule in the config, or `lookout mute --for 2h --group
Public` when you are about to break something on purpose. Probes keep running
either way; only delivery stops, and what happened during the mute arrives as
one summary when it lifts.

## The page

<picture>
  <source media="(prefers-color-scheme: light)" srcset="docs/detail-light.png">
  <img alt="A check expanded to show why it failed" src="docs/detail-dark.png">
</picture>

Every row opens into what the check watches, when it broke, what it said,
uptime over three windows, how the response time is spread, a month of days
one square each, and a bar of the last 24 hours. It is server-rendered HTML
with no framework and nothing loaded from a CDN.

Alongside it, `/api/status` is versioned JSON, `/metrics` is Prometheus text,
and `/healthz` returns 503 when alerts are piling up undelivered.

## Deploying

`contrib/systemd/lookout.service` runs it as its own user with
`ProtectSystem=strict` and an empty capability set. Secrets come from an
environment file, never from the config:

```sh
install -o root -g root -m 0755 lookout /usr/bin/lookout
useradd --system --home /var/lib/lookout --shell /usr/sbin/nologin lookout
install -d -o lookout -g lookout -m 0750 /var/lib/lookout
install -d -o root   -g lookout -m 0750 /etc/lookout
install -o root -g lookout -m 0640 config.yaml /etc/lookout/config.yaml
install -o root -g lookout -m 0600 contrib/lookout.env.example /etc/lookout/lookout.env
install -o root -g root -m 0644 contrib/systemd/lookout.service /etc/systemd/system/
# put the bot token and chat id in /etc/lookout/lookout.env, then
systemctl enable --now lookout
```

Point `state.file` and its neighbours at `/var/lib/lookout/`, or
`ProtectSystem=strict` will not let lookout write them.

### Staying current

`contrib/lookout-update` moves an installed lookout to the latest release. It
fetches the published binary and `SHA256SUMS`, refuses to install on a checksum
mismatch or a binary that will not load the running config, and puts the
previous binary back if the restarted service does not answer `/healthz`. The
three most recent binaries it replaced stay in `/var/backups/lookout`.

```sh
install -o root -g root -m 0755 contrib/lookout-update /usr/local/bin/
lookout-update            # or --dry-run to see what it would do
```

`REPO`, `BIN`, `CONFIG`, `SERVICE`, `HEALTH_URL`, `BACKUP_DIR` and `RELEASES`
are read from the environment, so a fork or a second instance needs no edit.
For unattended updates, install `contrib/systemd/lookout-update.{service,timer}`
and `systemctl enable --now lookout-update.timer`; it runs daily with a couple
of hours of jitter.

## Limits

Every probe leaves from the machine lookout runs on, so it cannot see its own
host go down; an external monitor is a different job.

Notifications go to Telegram and nowhere else. Checks are added by editing the
config, not through the page.

The status page renders without JavaScript, so a row opens through a checkbox
and `:has()`. A browser without `:has()` shows the whole board and never opens
a row.

## Contributing

`docs/design.md` covers the decisions that look strange from outside: no
database, no keep-alives, two statuses instead of three. Read it before
proposing a change to one of them.

```sh
go vet ./... && go test -race ./...
```

MIT.
