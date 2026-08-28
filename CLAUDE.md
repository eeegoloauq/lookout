# lookout

Uptime monitor. One Go binary, no database, no runtime deps beyond two modules.
Config is YAML, state is one JSON file, alerts go to Telegram.

## Layout

```
cmd/lookout       CLI: validate, run, demo, mute, unmute, test-alert, version
internal/config   YAML -> Check structs, and every validation error the user gets
internal/probe    http, tcp, dns, domain — one file each, all return check.Result
internal/check    Result type and condition evaluation (status, body, latency, rcode)
internal/state    up/down machine, thresholds, incident log, alert outbox, durable store
internal/monitor  the scheduler; owns state + history, folds results in, emits events
internal/alert    event -> message text -> Telegram, with batching and retries
internal/history  24h ring in memory, per-day JSONL, sample seed file
internal/web      status page, /api/status, /metrics, /healthz
internal/registry RDAP and WHOIS parsing
internal/demo     synthetic board for `lookout demo` and for the screenshots
internal/mute     scheduled and ad-hoc silence windows
```

Data flows one way. `monitor` calls a `probe`, gets a `check.Result`, hands it to
`state.Machine`, which returns events. Events go to the outbox, the outbox goes to
`alert`. Nothing calls back up the stack.

## Rules

A probe never returns an error. A failed probe is a `check.Result` with an
outcome, because "the check failed" and "the monitor is broken" are different
events and must not share a code path.

Silence means everything is fine, so silence must never be a bug. Alerting is on
for every check unless the config says `alert: false`. Never add a code path that
can swallow an event.

`lookout validate` catches what would otherwise crash `run`. A config error at
2am means monitoring is down. If you add a config field, add its validation and
its error message in the same change.

No database. It was considered and rejected — see docs/design.md. Do not add one,
do not add a cache layer, do not add a queue.

Comments say why, not what. If a line looks wrong but is deliberate, the comment
is what stops the next person from "fixing" it. Match that; the codebase is
consistent about it.

Keep-alives are off in the HTTP probe on purpose. A reused connection skips DNS,
TCP and TLS, which are the layers the check exists to cover.

The status page has no JavaScript, no external asset and no build step. The six
inline lines only pause the reload timer while a row is open. Keep it that way.

## Working on it

```sh
go vet ./... && go test -race ./...
go run ./cmd/lookout demo                       # board on 127.0.0.1:5665, probes nothing
go run ./cmd/lookout demo -write /tmp/page.html # same board as a file
go run ./cmd/lookout validate config.example.yaml
```

Anything on a clock is tested in a `testing/synctest` bubble on a fake clock.
Write new time-dependent tests the same way. There are no `time.Sleep` calls in
the test suite and there should not be.

Tests must not name a real host, address or domain. Use the reserved ones:
`example.com`, `.example`, `.invalid`, `.lan`, `198.51.100.0/24`. This repo is
public.

Commit messages are a sentence saying what changed and why, in English, present
tense, no prefix tags. Look at `git log` before writing one.

## Releases

A release is a tag. Pushing `vX.Y.Z` runs `.github/workflows/release.yml`, and the
release body is the annotated tag's own message with the commit list GitHub
generates appended under it. A lightweight tag, or `-m ""`, therefore ships a
release with no note at all — the message is written at tag time, and it is public
the moment the workflow finishes.

Write it for someone who runs this, not for someone reading the diff: what changed
in behaviour, and what they have to do on upgrade. The generated commit list below
already covers the tree, so repeating it there is waste.

The generated half always spans from the previous tag, whatever its level. A major
does not roll its minors up: tag `v2.0.0` after `v1.4.0` and the list covers
`v1.4.0..v2.0.0` and nothing before it. If a major should read as a summary of the
whole line, that summary goes in the tag message by hand — nothing infers it.
