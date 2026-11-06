# Setup

## Try it

```sh
go run github.com/eeegoloauq/lookout/cmd/lookout@latest demo
```

This serves the status board with made-up data. Nothing is probed and no config is read.

## Build and configure

```sh
CGO_ENABLED=0 go build -o lookout ./cmd/lookout   # or a binary from the latest release
cp config.example.yaml config.yaml
```

Edit `config.yaml` before the first run:

- Replace the example checks with yours.
- Point `state.file`, `state.history` and `state.samples` at a directory you can write to. The
  example uses `/var/lib/lookout/`.
- Alerts need `LOOKOUT_TELEGRAM_TOKEN` and `LOOKOUT_TELEGRAM_CHAT_ID` in the environment. To run
  without alerts, set `alerting.mode: none`.
- The example also reads `LOOKOUT_BASIC_AUTH` and `LOOKOUT_PUSH_TOKEN_ZFS`. Set them or delete the
  checks that use them.

```sh
./lookout validate config.yaml   # lists every problem with its line number
./lookout run config.yaml
```

`run` refuses a config that does not validate. The page listens on `127.0.0.1:5665` by default
(`listen:`).

As a container:

```sh
docker run -d --name lookout \
  -p 5665:5665 \
  -v $PWD/config.yaml:/etc/lookout/config.yaml:ro \
  -v lookout-state:/var/lib/lookout \
  -e LOOKOUT_TELEGRAM_TOKEN -e LOOKOUT_TELEGRAM_CHAT_ID \
  ghcr.io/eeegoloauq/lookout:latest
```

Set `listen: 0.0.0.0:5665` in the config for the port mapping to reach it.

[`config.example.yaml`](../config.example.yaml) is the configuration reference: every option is there with a comment.

## Deploy with systemd

[`contrib/systemd/lookout.service`](../contrib/systemd/lookout.service) runs lookout as its own user with `ProtectSystem=strict` and no
capabilities. Secrets go in an environment file, not in the config:

```sh
install -o root -g root -m 0755 lookout /usr/bin/lookout
useradd --system --home /var/lib/lookout --shell /usr/sbin/nologin lookout
install -d -o lookout -g lookout -m 0750 /var/lib/lookout
install -d -o root   -g lookout -m 0750 /etc/lookout
install -o root -g lookout -m 0640 config.yaml /etc/lookout/config.yaml
install -o root -g lookout -m 0600 contrib/lookout.env.example /etc/lookout/lookout.env
install -o root -g root -m 0644 contrib/systemd/lookout.service /etc/systemd/system/
# add the bot token and chat id to /etc/lookout/lookout.env, then
systemctl enable --now lookout
```

The state files must be under `/var/lib/lookout/`; `ProtectSystem=strict` blocks writes elsewhere.

## Updates

`contrib/lookout-update` installs the latest release. It downloads the binary and `SHA256SUMS`,
stops on a checksum mismatch or if the new binary cannot load the running config, and restores the
previous binary if the restarted service fails `/healthz`. The last three replaced binaries are
kept in `/var/backups/lookout`.

```sh
install -o root -g root -m 0755 contrib/lookout-update /usr/local/bin/
lookout-update            # --dry-run shows what it would do
```

`REPO`, `BIN`, `CONFIG`, `SERVICE`, `HEALTH_URL`, `BACKUP_DIR` and `RELEASES` can be overridden
from the environment. For daily unattended updates, install
`contrib/systemd/lookout-update.{service,timer}` and run
`systemctl enable --now lookout-update.timer`.
