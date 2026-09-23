# lookout

Lookout is a single-binary uptime monitor for HTTP, TCP, DNS, and missed job heartbeats. It serves a status board and API, keeps state in files, and sends Telegram alerts. It is meant for operators who want checks configured in YAML and a board they can inspect quickly.

## Commands

```sh
CGO_ENABLED=0 go build -o lookout ./cmd/lookout
./lookout validate config.yaml
./lookout run config.yaml
go run ./cmd/lookout demo
go vet ./...
go test -race ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
gofmt -l .
```

CI also validates `config.example.yaml` with placeholder secrets. Run that validation when changing configuration. Use `testing/synctest` for clock-sensitive tests where it applies; existing tests also use real sleeps, so do not assume the suite is sleep-free.

## Deploy and rollback

Version tags trigger release binaries, checksums, and a container image. The systemd installation and optional update timer are described in [README.md](README.md). `contrib/lookout-update` verifies the binary checksum, validates the running config, restarts the service, checks `/healthz`, and restores the previous binary if health fails. For a manual rollback, install a retained prior binary and restart the service, then check `/healthz`. No container rollout or rollback automation is tracked here.

## Decisions and gotchas

- Alerting defaults on. State changes enter a durable outbox before Telegram delivery and leave only after confirmation. Do not bypass it or silently discard events.
- `validate` must reject invalid configuration before `run` starts. Add validation and a useful error with each new setting.
- HTTP probes disable keep-alives so each check observes DNS, TCP, and TLS again. Probes also ignore inherited proxy settings.
- Push tokens appear in request URLs and may be sent from other hosts. Keep them out of logs and status output. Mute actions are loopback-only.
- The status board is server-rendered and uses a small inline script to defer reload while a row is open; without JavaScript, a meta refresh reloads it.
- File storage instead of a database, the full connection probe, and the board's scope are deliberate choices; see [docs/design.md](docs/design.md) before changing them. No database, cache layer or queue.
- A probe never returns an error: a failure is a `check.Result` with a reason.
- Tests never name a real host, address or domain — use reserved example ones (public repo).

Planned larger work: `ROADMAP.md`.
