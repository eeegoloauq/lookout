# Series 5 — mute windows, delegation loss, JSONL history, systemd

2026-08-19. Implements SPEC.md §8, §9.3, §11 (the unit file) and the
`.ru` DELEGATED emergency that series 4 stored but did not alert on.

## What shipped

1. **Mute windows (SPEC §8)** — two ways to silence delivery without
   stopping probes. Static windows in the config are a weekday list, a
   clock (`at: "02:00"`) and a duration, optional `group` / `check`,
   timezone default UTC. Ad-hoc mutes are `lookout mute --for 30m
   --group Public` and `lookout unmute`, which POST to `/api/mute` and
   `/api/unmute` on the process's existing HTTP listen address
   (loopback by default). A mute is durable, shown on `GET /` and
   `/api/status`, and exported as `lookout_muted`.
2. **Held alerts are not dropped.** Events that fire while muted are
   counted into a digest on the hold (kind / check / group, same shape
   as the outbox overflow summary). When the mute lifts — expiry,
   `unmute`, or a process start that finds an expired hold — one
   `held` event is enqueued. Overlapping mutes transfer the digest to
   whichever cover remains. The operator sees what happened; they do
   not get one Telegram message per probe.
3. **Domain delegation loss.** The tcinet `state:` field is parsed as
   comma-separated tokens. Losing `DELEGATED` (including a first sight
   of `REGISTERED` without it) fires `undelegated` once; `DELEGATED`
   returning fires `delegated` and clears the flag. gTLD status codes
   are a different vocabulary and are ignored.
4. **Long-term history (SPEC §9.3).** One JSONL line per check per UTC
   day (date, uptime, incidents, p50/p95), appended at midnight. The
   in-progress day is accumulated in the state file, so a restart at
   23:00 then 01:00 neither duplicates nor loses the day. A truncated
   last line (crash mid-write) is skipped. `/api/status` grows
   `uptime_7d` and `uptime_30d` from this file plus today.
5. **systemd unit** at `contrib/systemd/lookout.service`: dedicated
   `lookout` user, `NoNewPrivileges`, `ProtectSystem=strict`,
   `ProtectHome`, `PrivateTmp`, `RestrictAddressFamilies`, empty
   `CapabilityBoundingSet`, secrets from `EnvironmentFile` mode 0600
   (see README). Forgejo Actions at `.forgejo/workflows/ci.yml` runs
   `go vet`, `staticcheck`, `go test -race` and a build.

## Deviations from SPEC.md

| Spec | What we did | Why |
|---|---|---|
| Cron expression + duration (§8) | Weekday list + `at:` HH:MM + duration. A `cron:` field is a load-time error with that explanation. | No third dependency for a cron parser. Gatus's maintenance windows were *not* cron (research: timezone bugs). A homelab window is a weekday and a clock, validatable at load time. |
| Unix socket *or* local HTTP for `lookout mute` | HTTP on the existing listen address only (default loopback). | The process already serves HTTP; a second socket is another path to get wrong. Binding off loopback is the operator's `listen:` choice. |
| Bearer token on mutating endpoints (research O8) | None. Auth is the listen address. | The task said to reach the running process over the HTTP surface it already serves. A token would be a third secret and is not in §8. |
| `govulncheck` in CI (§13) | vet, staticcheck, race tests, build. | The task listed those four; `govulncheck` needs network to a vulnerability DB on every run. |
| Ad-hoc mute duration unbounded | Capped at 7 days. | A forgotten mute is how an outage stays silent (research 4.10). Seven days is a vacation, not a maintenance window. |
| One JSONL line per check per day even with no samples | A day with zero samples and zero incidents is not written. | Writing `uptime: 0, samples: 0` reads as a total outage. Silence in the file means lookout was not probing that day. |

## Unverified

- A scheduled window whose timezone is not UTC, on a host without tzdata
  (`time.LoadLocation` fails at config load; we did not run against a
  stripped container).
- `lookout mute` against a process whose `listen:` is IPv6-only (`::`
  is rewritten to 127.0.0.1 for the CLI).
- A year of JSONL (the format is append-only; we did not fill a disk).
- The systemd unit on a real host (ProtectSystem / StateDirectory
  interaction is from the unit file's documented contract, not a boot).
- End-to-end mute of a live Telegram chat (forbidden in tests).
