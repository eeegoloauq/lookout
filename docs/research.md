# lookout — prior-art research

Research report preceding implementation (SPEC.md §15.0).
Date: 2026-08-19. Target: Go 1.26, single static binary, homelab scale (tens of checks).

## 0. Method and confidence marking

Claims below are marked:

- **[verified]** — checked against a primary source during this research (source code, live
  protocol query, repository API, IANA data, Go toolchain on this machine). Date of check: 2026-08-19.
- **[secondary]** — taken from documentation, blog posts or search summaries; plausible, not
  independently confirmed.
- **[recalled]** — stated from prior knowledge, no source fetched. Treat as a lead, not a fact.

Nothing in this report should be copied into code without re-reading the cited source.

---

## 1. The five analogs

### 1.1 Summary table

| | Gatus | Uptime Kuma | healthchecks.io | Blackbox Exporter | statping-ng |
|---|---|---|---|---|---|
| Model | pull, declarative YAML | pull, UI-configured | **push** (dead man's switch) | pull, probe-on-demand | pull, UI/DB |
| Config source | file (GitOps) | SQLite via web UI | web UI + API | file (modules) + Prometheus targets | web UI |
| State/history | SQLite/Postgres/memory | SQLite/MariaDB | Postgres | none (stateless) | SQLite/Postgres/MySQL |
| Alert decision | in-process, thresholds | in-process, retries | in-process, grace period | **delegated to Prometheus** | in-process |
| Stars | 11 854 | 90 333 | 10 259 | 5 825 | 1 988 |
| Open issues | 367 | 791 | 53 | 170 | 194 |
| Last push | 2026-08-18 | 2026-08-19 | 2026-08-19 | 2026-08-12 | **2025-06-04** |
| License | Apache-2.0 | MIT | BSD-3 | Apache-2.0 | GPL-3.0 |

All repository metrics **[verified]** via GitHub API, 2026-08-19.

**statping-ng is effectively dormant**: last push 2025-06-04, ~14 months stale, 194 open issues
**[verified]**. It presents itself as "the actively maintained fork" but the commit record does not
support that. It should not be used as a design reference for anything except a cautionary note:
it is the second project in this lineage to stall (it forked statping for the same reason), and its
scope — plugins, themes, multiple SQL backends, a Vue dashboard — is exactly the scope SPEC.md §2
declares out of bounds. That correlation is worth taking seriously.

### 1.2 Gatus — the closest relative, and the one being replaced

Gatus is the most relevant prior art because SPEC.md is largely a reaction to it. Its actual
internals are worth copying in two places and rejecting in two others.

**What works, verified in source:**

*Storage layering.* Gatus does not keep raw results forever. It keeps a **ring buffer of raw
results per endpoint** (`storage.maximum-number-of-results`, default **100**) plus a separate
**hourly uptime aggregate table** **[verified]** (`storage/store/sql/specific_sqlite.go`):

```sql
CREATE TABLE IF NOT EXISTS endpoint_uptimes (
    endpoint_uptime_id    INTEGER PRIMARY KEY,
    endpoint_id           INTEGER NOT NULL REFERENCES endpoints(endpoint_id) ON DELETE CASCADE,
    hour_unix_timestamp   INTEGER NOT NULL,
    total_executions      INTEGER NOT NULL,
    successful_executions INTEGER NOT NULL,
    total_response_time   INTEGER NOT NULL,
    UNIQUE(endpoint_id, hour_unix_timestamp)
)
```

Hourly rows older than 48 h are merged into daily rows; constants **[verified]** in
`storage/store/sql/sql.go`:

```go
uptimeTotalEntriesMergeThreshold = 100                 // merge trigger
uptimeAgeCleanUpThreshold        = 32 * 24 * time.Hour // cleanup trigger
uptimeRetention                  = 30 * 24 * time.Hour // minimum kept
uptimeHourlyBuffer               = 48 * time.Hour      // hourly→daily boundary
```

Note `total_executions` + `total_response_time` rather than a precomputed average. This is not an
accident: **sums and counts merge across buckets, averages do not.** Storing an average per hour
makes the hourly→daily merge mathematically wrong unless all hours have equal counts. Copy this.

*Persistent alert state.* Gatus persists which alerts have fired **[verified]**:

```sql
CREATE TABLE IF NOT EXISTS endpoint_alerts_triggered (
    endpoint_alert_trigger_id     INTEGER PRIMARY KEY,
    endpoint_id                   INTEGER NOT NULL REFERENCES endpoints(endpoint_id) ON DELETE CASCADE,
    configuration_checksum        TEXT    NOT NULL,
    resolve_key                   TEXT    NOT NULL,
    number_of_successes_in_a_row  INTEGER NOT NULL,
    UNIQUE(endpoint_id, configuration_checksum)
)
```

This was added in response to issue **#679** — "Alerts triggered status is not persistent",
closed via PR #764 **[verified]**. Before that, a restart re-sent every active alert. SPEC.md §9
already plans a `state` table; the non-obvious detail is the `configuration_checksum` key:
**editing an alert's configuration silently resets its "already sent" state and can re-page you.**
That is a design trade-off (config change = new alert identity) which should be a conscious
decision, not an emergent one.

*Self-connectivity gate.* Gatus has a feature SPEC.md lacks entirely **[verified]**, README:

```yaml
connectivity:
  checker:
    target: 1.1.1.1:53
    interval: 60s
```

> "All endpoint executions are skipped while the connectivity checker deems connectivity to be down."

The idea is right; the implementation choice is questionable. *Skipping* executions leaves a
**silent hole** in history — the uptime denominator shrinks and the status page shows nothing
happened. Recording an explicit `unknown` result would be strictly better (see §9.2).

**Known pitfalls, from the issue tracker:**

- **#1392** — custom alerts inside "suites" ignore `alerting.custom.default-alert` settings;
  `failure-threshold: 1` silently reverts to the default 3, and `send-on-resolved: true` has no
  effect **[secondary]**. This is precisely SPEC.md's founding complaint (§1.1) in a new form: the
  alerting default is *configured* but not *applied*. The lesson generalizes — a default that is
  resolved in more than one place will eventually be resolved inconsistently.
- **#679** — alert state lost on restart (fixed, above) **[verified]**.
- **#1274 / #74** — maintenance windows: requested in Jan 2021, shipped, still generating bug
  reports about timezone handling **[secondary]**. Gatus's window model is `start` (hh:mm) +
  `duration` + `timezone` + `every: [Monday, Thursday]` **[verified]** — note it deliberately is
  *not* a cron expression.
- **#1245** — "Support for IPv6 DNS resolution for HTTP requests", open since 2025-09-04, 9 👍
  **[verified]**. Dual-stack is a real, unaddressed gap.
- **#1593** — CVE-2026-33186 via a transitive gRPC dependency **[verified]**. A monitoring tool with
  40+ notification integrations has a dependency surface that becomes its own liability.

**What Gatus fundamentally does not do:**

1. **No dependency / parent-child relationships.** Issue **#1708** ("Add endpoint dependencies /
   parent-child relationships", 2026-07-04) was **closed without implementation** **[verified]**.
   The reporter's scenario is verbatim SPEC.md §7 — a gateway drop cascading into 14+ false alerts.
   The maintainer's responses **[verified]**: *"Why not use suites?"* and *"Best I can offer for you
   is to look into the `connectivity` configuration."* So the official answer to alert cascades is
   a global internet gate, not a dependency graph. **This is the single clearest gap SPEC.md
   targets, and it is a deliberate maintainer decision rather than an oversight** — see §5.
2. **Uptime beyond 30 days.** `uptimeRetention = 30 days` is hard-coded **[verified]**. SPEC.md §10
   asks for 24h/7d/30d, which fits, but 90-day retention (§9) has no consumer.
3. **Time-based raw retention.** Retention is "last N results", not "last N days". With a 60 s
   interval and the default 100, raw history covers **100 minutes**. Anything longer is served from
   the aggregates only.
4. **A runtime-parsed condition language.** `"[STATUS] == 200"` is parsed at runtime. SPEC.md §4
   rejects this correctly — but see objection O2, the proposed replacement inherits the flaw.

### 1.3 Uptime Kuma — the popularity/architecture cautionary tale

90 333 stars, 791 open issues **[verified]**. The top open feature requests are revealing
**[verified]**:

| 👍 | # | Title | Open since |
|---|---|---|---|
| 698 | #118 | API functionality | 2021-07-27 |
| 405 | #84 | Remote Executors | 2021-07-19 |
| 340 | #128 | Basic user management | 2021-07-30 |
| 290 | #1888 | Configurable heartbeat bar range | 2022-07-12 |
| 91 | #1354 | **Configure monitors using a config file** | 2022-03-04 |

Five years of the most-requested features being structurally blocked. The root cause is the
founding architectural choice: **state, configuration and UI are one SQLite database driven by a
web UI.** There is no config file, so there is no GitOps, no `validate` command, and no reviewable
diff. SPEC.md's choice of a declarative YAML file is the correct opposite, and #1354 is the
evidence.

**Database growth is Kuma's signature failure mode** and the most directly applicable lesson.
Reported instances **[secondary]**, from issue titles and search summaries — full threads not read:

- #5187: 1 400 MB with 300 monitors at 1-minute interval; deleting history reduced the file to 5 MB.
- #1994: 1 718.5 MB with 45 monitors, 400 days retention; **reducing retention and shrinking did
  not reduce the file size**.
- #3261: 6 GB with 200 ping-only monitors at 90 days retention.
- #2470: *"Shrinking database / blocking database operations give false downtime"* — the retention
  job holds a write lock long enough that probes time out and monitors are marked down.

Two mechanisms are at work, and both are avoidable by design rather than by tuning:

1. **Raw rows were the only storage.** Kuma only added aggregate tables in the 2.0 line (PR #5075,
   "Update database migration and history retention"), and that migration itself produced
   #7274 — *"Interrupted aggregate table migration leaves Uptime Kuma stuck in migrating state"*
   **[secondary]**. Retrofitting aggregation onto a large table is a migration hazard; starting
   with it is nearly free.
2. **`DELETE` does not shrink a SQLite file** — see §6.

Additional Kuma findings relevant to lookout:

- **#7612** — *"Notifications hang indefinitely on networks with partial/broken IPv6 (Node 18 lacks
  default Happy Eyeballs)"*, closed 2026-07-20 **[verified: title/status]**. This is the alert path
  itself failing, silently and indefinitely. See §9.1.
- **#922** — *"No down notifications for push monitor"*, with repeated `Cannot send notification to
  SMTP` in logs **[secondary]** — the failure was visible only in logs nobody reads.
- **#6284** — retry count not honoured for grouped monitors; a single transient error paged
  **[secondary]**.
- **#1137** — *"Group / Collate / Aggregate notifications"*, open since 2022-01-07 **[verified]**.
  Notification batching is requested and unimplemented. See §5.4 — this is cheap and high-value.

### 1.4 healthchecks.io — the inverted model

Push-based: the monitored job checks *in*, and absence of a check-in is the signal. Model
**[secondary]**: each check has a `period` and a `grace` time; a missed ping moves the check to
*delayed*, and expiry of `grace` moves it to *down*, which fires the integrations.

Three ideas worth stealing even though lookout is pull-based:

1. **`grace` is a second, independent time constant.** It separates "how often should this happen"
   from "how late is too late". Threshold counters conflate these.
2. **The absence of data is the alert.** A pull monitor's worst failure is being dead itself, and it
   is structurally incapable of noticing. This is why SPEC.md §12 exists — and why a *weekly*
   heartbeat is too coarse (objection O4).
3. **Cron/OnCalendar-aware schedules** — a check can be expected on a cron expression rather than a
   fixed interval, so a nightly backup is not "down" for 23 hours a day.

Relevant negative finding: the search did not surface any flapping-specific or nag-suppression
mechanism in healthchecks.io **[secondary]** — its model largely sidesteps flap by having a single
time-based criterion rather than a sequence of samples.

Also notable: 53 open issues against 10 259 stars **[verified]** — by far the healthiest
issue-to-star ratio of the five. A narrow, deliberately un-extended scope is what produces that.

### 1.5 Prometheus Blackbox Exporter — the right decomposition, the wrong ergonomics

Blackbox's defining decision is that **it does not decide anything**. It probes on demand and
returns metrics; thresholds, state, hysteresis, dependency suppression and notification all live in
Prometheus and Alertmanager. This is why it has no flap logic, no history, no alerting and no
status page — and why the parts it does have are unusually good.

**What it does that the others do not:**

*Per-phase timing.* `probe_http_duration_seconds` is labelled by phase, wired to `net/http/httptrace`
**[secondary]**: `resolve` (start→dnsDone), `connect` (dnsDone→gotConn), `tls`, `processing`
(gotConn→responseStart), `transfer` (responseStart→end). This turns "the check failed" into "the
check failed *at DNS resolution*", which is the raw material for free correlation (§5.4).

*Explicit IP-protocol control.* `preferred_ip_protocol` and `ip_protocol_fallback` **[recalled]** —
dual-stack behaviour is a configuration decision rather than whatever the resolver happened to
return.

**Its known TLS-expiry traps are directly applicable to SPEC.md §5.3:**

- `probe_ssl_earliest_cert_expiry` **is only emitted when the TLS handshake completes**
  **[secondary]**. If the certificate has *already expired*, the hostname mismatches, or the CA is
  untrusted, the handshake fails and the expiry metric is simply absent — the expiry alert goes
  silent at exactly the moment it matters. The standard mitigation is to alert on
  `probe_success == 0` as well. SPEC.md §5.3 has the identical structure and therefore the identical
  hole (objection O18).
- **#1406** — `probe_ssl_earliest_cert_expiry` follows redirects, so it can report the certificate
  of a *different host* than the one configured **[verified: title]**.
- **#637** — the metric is "inconsistent or wrong" in chain-expiry edge cases; newer
  `probe_ssl_last_chain_expiry_timestamp_seconds` and `probe_ssl_last_chain_info` were added to fix
  the semantics **[secondary]**. The lesson: "days until expiry" is under-specified — *whose* expiry,
  leaf or chain, and after how many redirects?

**Other tracker signals** **[verified: titles/dates]**: #1567 (rebuild request for
CVE-2025-68121 / CVE-2026-33186), #1580 (release needed for GO-2026-4918), #1429 (CA bundle is
stale between releases), #1510 (0.28.0 logs every probe failure at ERROR with no way to disable).
The last one matters for lookout: **a monitoring tool logging every failure at ERROR level makes its
own logs useless during an outage**, which is exactly when you read them.

**What it fundamentally cannot do:** anything stateful. No "3 in a row", no incident duration, no
dependency suppression, no "notify once". All of that is Prometheus + Alertmanager, which is
several hundred megabytes of infrastructure — the reason SPEC.md is not simply "use Blackbox".

---

## 2. Flap detection: why "N consecutive" is not enough

SPEC.md §6 specifies `failure_threshold: 3` / `success_threshold: 2`. This is the near-universal
design (Gatus, Kuma, and most commercial monitors use it). It is also insufficient, for five
distinct reasons — the first of which is disqualifying.

### 2.1 The failure classes that a consecutive counter cannot see

**A single success resets the counter.** Therefore any failure pattern that never produces N
consecutive failures is invisible **forever**, regardless of how bad availability actually is:

| Pattern | Availability | Alerts with `failure_threshold: 3` |
|---|---|---|
| `U D U D U D …` | 50 % | **0, forever** |
| `D D U D D U …` | 33 % | **0, forever** |
| `D D D D …` | 0 % | 1 |

This is not a contrived case. It is the exact signature of:

- one bad backend behind a round-robin DNS record or a 2–3 member load-balancer pool;
- one unhealthy replica that the orchestrator has not evicted;
- a resolver returning a stale A record for one of two addresses;
- a service that fails only on requests that land on a cold cache shard.

A monitor that reports 100 % uptime for a service delivering 50 % of requests successfully is worse
than no monitor, because it is trusted. **This alone justifies replacing the consecutive counter.**

### 2.2 Sensitivity and latency are welded to the same knob

With one parameter N you set both the false-alarm rate and the detection latency, and they move in
opposite directions. Expected spurious alerts per day at a 60 s interval (1440 samples), for
independent per-check failure probability `p`, approximated as `n·p^N·(1−p)`:

| p (per-check failure) | N=2 | N=3 | N=5 |
|---|---|---|---|
| 0.005 | 0.036 | 0.0002 | ~0 |
| 0.01 | 0.143 | 0.0014 | ~0 |
| 0.02 | 0.565 | 0.011 | ~0 |
| 0.05 | 3.42 | 0.171 | 0.0004 |
| 0.10 | 12.96 | 1.296 | 0.013 |

Worst-case detection latency is `N × interval`: 2 min / 3 min / 5 min. So on a link with 5 % loss,
N=2 pages you 3.4 times a day and N=5 essentially never — but N=5 also costs 5 minutes on a genuine
hard outage. There is no setting that is simultaneously fast on real outages and quiet on a lossy
link, because the mechanism has no way to tell those apart. A **rate over a window** can.

### 2.3 No notion of rate

Three failures spread over three hours and three failures in three minutes are indistinguishable to
a consecutive counter — it only sees "not consecutive" and stays silent for both. Only one of those
is a non-incident.

### 2.4 No memory across incidents

A service that flaps 40 times a day produces 40 down/up alert pairs. Nothing in the mechanism
observes that *the flapping itself is the incident*. Thresholds cannot express "this thing has been
unstable all week"; they can only express "it is bad right now".

### 2.5 Recovery is as fragile as detection

`success_threshold: 2` is the same mechanism mirrored, so the recovery decision inherits every
weakness above. Combined, the two produce the classic ping-pong: `DOWN → UP → DOWN → UP` with a
notification on every edge.

### 2.6 Mechanisms that do work

**(a) Sliding window ratio — "k of the last n".** State is a ring buffer of the last n results;
DOWN when ≥ k of them failed. Fixes §2.1 and §2.3 outright. Cost: one `uint64` bitmask per check if
n ≤ 64, plus `math/bits.OnesCount64`. This should be the **baseline**, with `failure_threshold: 3`
retained as sugar for `k=3, n=3`.

**(b) Asymmetric hysteresis.** Enter DOWN on the window criterion; leave DOWN only on a *stricter*
one — e.g. m consecutive successes **and** a minimum dwell time in DOWN. Prometheus expresses this
as `for` (entry delay) plus `keep_firing_for` (exit delay) **[secondary]**:

> "`keep_firing_for` tells Prometheus to keep an alert firing for a specified duration after the
> firing condition was last met… used to prevent flapping alerts and false resolutions."

A minimum-dwell of ~10–15 min on DOWN converts a 40-flap day into a handful of notifications with
almost no code. Note Prometheus's own known gap here: **#13957, "Persist `keep_firing_for` state
across restarts"** **[verified: title]** — the same restart-amnesia class as Gatus #679. Persist it.

**(c) Flap score with exponential decay — the BGP route-flap-damping pattern (RFC 2439).** The only
mechanism in this list that addresses §2.4. Per RFC 2439 **[secondary]**: each flap adds a penalty
to a per-route "figure of merit"; the penalty decays exponentially with a configurable half-life;
crossing a **suppress threshold** suppresses the route, and it is reusable only when the score
decays below a **reuse threshold**, bounded by a **maximum hold-down time**. Adapted to a monitor:

```
on every state transition:  score += 1.0
continuously:               score *= 0.5 ^ (Δt / half_life)      // half_life ≈ 30m
score > 3.0  →  enter FLAPPING: stop per-transition alerts, send ONE "X is flapping" alert
score < 1.0  →  leave FLAPPING: resume normal alerting, send "X stabilised"
hard cap:       never stay FLAPPING longer than max_hold (≈2h) without re-evaluating
```

Cost: one float and one timestamp per check. Nagios's `percent_state_change` is a cruder version of
the same idea — it keeps the **last 21 check results** and computes a weighted percentage in which
*"the newest possible state change carries 50 % more weight than the oldest"* **[verified]**, then
compares against `low_/high_service_flap_threshold`. Nagios's documentation does **not** publish the
default numeric thresholds **[verified — I looked and they are absent from the page]**; the commonly
cited values are 5 %/20 % for hosts and 5 %/25 % for services **[recalled, unverified]**. The decay
formulation is better than the 21-slot window because it is interval-independent: it does not
silently change meaning when a check's interval is 60 s versus 5 min.

**The critical safety rule.** BGP damping's historical failure was over-damping: legitimate
withdrawals accumulated penalties and real outages were suppressed. RIPE later recommended against
route-flap damping with default parameters for this reason **[recalled, unverified]**. The
adaptation must therefore obey: **penalise transitions, never sustained state**, and *never*
suppress an alert for a check that has been continuously DOWN longer than the hold time. Flap
damping may convert many alerts into one; it must never convert one alert into zero.

**(d) Multi-window burn rate.** Google's SRE workbook approach: require a short window *and* a long
window to both breach, so the short window confirms currency and the long confirms that it is
sustained **[secondary]**. Full error-budget machinery is overkill at homelab scale, but the
two-window idea costs nothing:

```
DOWN  if  (5 of last 5 failed)  OR  (≥ 30% of last 60 failed)
```

The first clause is fast on hard outages; the second catches §2.1's alternating patterns.

### 2.7 Recommendation for lookout

Combine (a) + (b) + (c). Concretely:

- Replace the counter with `window: 5, failures: 3` (keep `failure_threshold: N` as an alias for
  `window: N, failures: N` so SPEC.md's config stays valid).
- Add the second, long window: `≥30% of last 60`.
- Add `min_down_duration: 10m` as recovery hysteresis.
- Add the decayed flap score, with `FLAPPING` as a **state visible on the status page and in
  `/metrics`** — not merely an internal suppression flag. An operator must be able to see *why* it
  went quiet, otherwise flap damping recreates SPEC.md §1's founding bug in a new disguise.
- All four are pure functions over a result sequence, so they are exhaustively table-testable, which
  SPEC.md §13 already requires.

---

## 3. Cascading alerts and event correlation

### 3.1 How the mature systems do it

**Nagios — topology in the state model.** Parent/child host relationships produce a distinct host
state: `UP`, `DOWN`, and **`UNREACHABLE`** — the last meaning "a parent is down, so I cannot
determine this host's state" **[secondary]**. Notifications for `UNREACHABLE` are configured
separately from `DOWN`. Strength: causality is encoded in the *state*, so history records what was
actually known, and root cause is directly readable. Weakness: you must model the topology.

**Zabbix — trigger dependencies.** A trigger may depend on other triggers; while the master trigger
is in `PROBLEM`, *"the dependent trigger will not change its state"* **[secondary]**. Note the
subtlety: **Zabbix freezes the dependent trigger's state**, not just its notification. SPEC.md §7
takes the opposite and better position — child checks keep running and keep writing history, only
alerts are suppressed. That preserves the record of what actually happened, which is what you need
during the post-mortem.

**Alertmanager — label algebra, no topology.** `inhibit_rules` with `source_matchers`,
`target_matchers` and an `equal` list of labels that must match on both sides **[secondary]**.
Strength: no graph to maintain; `NodeDown` inhibits every `PodNotReady` on the same node by label
equality. Known weaknesses **[secondary / verified where noted]**:

- **It is a race.** Inhibition requires the source alert to be *actively firing*. If the parent's
  own `for` duration has not elapsed, the children are not inhibited and page anyway. This is the
  single most important flaw to design around — see §3.3.
- It suppresses **notification only**; the alerts still show as firing in the UI.
- State persistence across restarts is incomplete (prometheus#13957) **[verified: title]**.

**Gatus — nothing, deliberately.** Issue #1708 closed without implementation; the maintainer's
answer was `connectivity` **[verified]** (see §1.2). This is a meaningful data point about *why*
dependency graphs are rare: the maintainer of the closest analog considered the request and judged
that a global connectivity gate covers the realistic case at a fraction of the complexity. He is
substantially right about homelab topologies — where nearly every cascade has the same single root
(the uplink) — and substantially wrong about the general case.

### 3.2 The lag problem nobody solves

Dependency suppression as specified in SPEC.md §7 — "if the parent is DOWN, suppress children" — has
an ordering hole:

```
t=0    uplink dies; all 20 checks begin failing
t=60   check #1..#20 fail (1st)     parent "gateway" fails (1st)   → nobody is DOWN yet
t=120  checks fail (2nd)            gateway fails (2nd)            → nobody is DOWN yet
t=180  checks fail (3rd) → DOWN     gateway fails (3rd) → DOWN     → 21 simultaneous transitions
```

Parent and children reach their thresholds in the **same evaluation round**. Whether suppression
works then depends entirely on the order in which the scheduler happens to evaluate them. Worse, if
the gateway check has a longer interval (a common, reasonable choice), the children reach DOWN
*strictly before* the parent, and suppression never engages at all — you get the 20-alert storm the
feature exists to prevent, intermittently, which is the worst possible failure mode because it looks
like it works in testing.

**Fix: never send alerts synchronously.** Buffer every transition for a short window before
delivering. This is the single most valuable mechanic in this section, and it happens to solve the
problem generically:

### 3.3 Simple schemes that work, ranked by value per line of code

**S1 — Batching window (`group_wait`).** Do not send on transition. Push transitions into a buffer;
after `group_wait` (30–60 s) of no new transitions, emit **one** message containing all of them.
Alertmanager's `group_by` + `group_wait` (default 30 s) **[secondary]** is exactly this, and it is
the mechanism doing most of the real work in production Prometheus setups — inhibition gets the
attention, grouping gets the results.

Why this is the right primary mechanism for lookout:
- It solves SPEC.md §7's stated goal — *"падение шлюза = один алерт, а не двадцать"* — with **zero
  configuration and no graph**. Twenty checks failing together arrive as one message with twenty
  lines.
- It **fixes the §3.2 lag problem for free**, because the parent's transition lands in the same
  buffer as the children's regardless of evaluation order.
- It is a prerequisite for a durable alert outbox anyway (§9.1), so the queue is not extra work.
- Cost: ~40 lines. Uptime Kuma has had this open as a feature request since 2022 (#1137)
  **[verified]**.

**S2 — Self-connectivity gate, recorded as `unknown`.** Before concluding DOWN, confirm the monitor's
own egress works (a TCP dial to a stable off-box target, per Gatus's `connectivity`). If egress is
down, record results as `unknown` with a reason — **do not skip, and do not record DOWN**. Gatus
skips, leaving a silent hole (§1.2). Nectus reportedly does something similar by ICMP-probing its own
default gateway at 3× the normal rate **[secondary]**.

This matters more than a dependency graph in a homelab, because the overwhelmingly common cascade
root is "the uplink or the monitor's own host", not "service A depends on service B".

**S3 — Failure-signature correlation (derived, zero configuration).** Group buffered transitions by
a *derived* key before formatting the message:

- the **phase** that failed (`resolve` / `connect` / `tls` / `status` / `body`), per §1.5;
- the error class (`i/o timeout`, `connection refused`, `no such host`, `x509`);
- the resolved IP, or its /24;
- the target hostname's parent domain.

Twenty checks that all failed at `resolve` with `no such host` within 60 s are **one DNS incident**,
and the message should say so. This gets most of the value of an explicit dependency graph without
any configuration to maintain or keep correct, and it catches relationships nobody thought to
declare. Almost nothing in this category ships it (§4.3).

**S4 — Explicit `depends_on`.** Keep it, as SPEC.md specifies, but demote it: it is the precision
tool for the few relationships that genuinely matter (reverse proxy → the services behind it), not
the primary anti-spam mechanism. Two changes from SPEC.md: make it a **list** (objection O20), and
implement it as a filter applied *at flush time* over the S1 buffer, not at transition time.

**S5 — Root-cause ordering in the batched message.** When the batch contains a parent and its
children, or a shared signature, put the probable root first and summarise the rest:
`gateway DOWN — 19 dependent checks also failing (list below)`. Pure formatting, high perceived value.

**Recommended composition:** S1 always → S2 as a gate producing `unknown` → S3 for grouping inside
the message → S4 to drop children entirely when a parent is already DOWN → S5 for presentation.
S1+S2 alone deliver most of the benefit, and both are simpler than SPEC.md §7's graph.

---

## 4. What almost nobody does

The brief asks for concrete mechanics that are largely absent from the field, with the reason for
their absence. Each item below is a specific implementable behaviour, not a principle.

### 4.1 Treat the notification path as a monitored dependency, with a durable outbox

**Mechanic.** Alerts are not sent from the check goroutine. A state transition writes a row into an
`outbox` table **in the same transaction** that writes the state change. A single delivery worker
drains it with exponential backoff and marks rows delivered. Then three things become possible that
are otherwise impossible:

1. `/healthz` returns degraded, and `/metrics` exports
   `lookout_undelivered_alert_age_seconds`, when the oldest undelivered alert exceeds a threshold.
   **The alerting pipeline becomes observable by the thing that watches lookout.**
2. Escalation to a second transport after M minutes of failure to deliver on the first.
3. A periodic **canary**: send a real message through the real transport on a schedule; if the
   canary fails, you learn the path is broken *before* an incident needs it.

**Why it is absent.** Notifications are modelled as plugins with a `Send(alert) error` signature, and
that signature has nowhere to put durability. Once you have 40 integrations (Gatus) or 90 (Kuma),
the error return is logged and dropped because there is no per-integration state to reconcile. In
hosted products the delivery path is the vendor's own infrastructure and is assumed reliable. And
fundamentally: the failure is invisible during normal operation and only manifests during an
incident, so it never generates enough bug pressure to get fixed.

**Evidence it is a real failure mode**: Kuma #7612, notifications hanging indefinitely on broken
IPv6 **[verified: title]**; Kuma #922, repeated `Cannot send notification to SMTP` visible only in
logs **[secondary]**. SPEC.md §1's founding incident — a 6.5-hour outage that alerted nobody — is
this class of bug. **The current SPEC.md does not defend against it** (objection O3).

### 4.2 A first-class `unknown` state, excluded from the uptime denominator

**Mechanic.** Results carry three outcomes, not two: `up`, `down`, `unknown(reason)`, where reason ∈
`{monitor_starting, egress_down, config_reload, dependency_down, muted, probe_error}`. `unknown` is
excluded from **both** the numerator and the denominator of uptime, is rendered distinctly on the
status page, and has its own alert rule: *"a check has been `unknown` for > 30 min"* is itself
actionable.

**Why it is absent.** The product pressure is a single number — "99.9 %" — and a third state forces
the awkward question of what that number means when you did not look. Binary state also halves the
UI work and lets uptime be a single `AVG(success)`. The canonical monitoring vocabulary has had this
right since Nagios: exit code **3 = UNKNOWN** **[secondary]**, and host state `UNREACHABLE` is
distinct from `DOWN` **[secondary]** — the modern self-hosted generation dropped it.

SPEC.md has `unknown` for domain expiry (§5.4) only. It should be global: a lookout restart, a config
reload, a mute window and a dependency suppression must not each silently pollute uptime.

### 4.3 Record *which layer* failed, and correlate on it

**Mechanic.** Wrap the probe in `net/http/httptrace` and store the phase boundaries (`resolve`,
`connect`, `tls`, `ttfb`, `transfer`) plus the phase at which failure occurred. Two payoffs: the
alert says *"failed at TLS handshake: x509: certificate has expired"* instead of *"check failed"*,
and the correlation key of §3.3-S3 becomes available for free.

**Why it is absent.** Blackbox Exporter has it **[secondary]** because it is metrics-native and
phases are just more labels. Gatus and Kuma store a boolean plus a duration, because a richer result
model touches the schema, the API, the status page and every notification template at once. It is a
cross-cutting change that is cheap on day one and expensive on day four hundred — which is exactly
why it belongs in SPEC.md now.

### 4.4 Force connection freshness

**Mechanic.** Set `Transport.DisableKeepAlives = true` on the probe client, or force a fresh
connection every Nth probe.

**Why it matters.** Go's `http.Client` reuses keep-alive connections by default. After the first
probe, subsequent probes **skip DNS resolution, the TCP handshake and the TLS handshake entirely**.
A monitor on a warm connection will happily report green while:

- the DNS record now points somewhere else (you are still talking to the old IP);
- the certificate has been replaced with a broken one (you never re-handshake);
- the target's listener would refuse a *new* connection.

Every one of those is a real outage for users, who do not have a warm connection. This also silently
invalidates SPEC.md §5.3 — "cert expiry for free from the handshake" — because on a reused connection
**there is no handshake**, so the certificate data is stale by however long the connection has lived.

**Why it is absent.** It is an invisible default. Nobody chose connection reuse; it is what
`http.DefaultTransport` does. And disabling it makes latency graphs jump, which reads as a
regression. The cost is one TCP+TLS handshake per check per minute — utterly negligible at homelab
scale, and it is the difference between measuring the service and measuring a cached socket.

### 4.5 Probe both address families separately

**Mechanic.** When a host has both A and AAAA records, run the check twice — once forced to `tcp4`,
once to `tcp6` — and report them as distinct results.

**Why it matters.** Happy Eyeballs is designed to hide broken IPv6 from clients, and it hides it from
your monitor too. A host whose AAAA points at a dead address is broken for a real fraction of users
and perfectly green in every monitor listed here.

**Why it is absent.** It doubles the check count for a minority-traffic concern, and the tools cannot
even resolve IPv6 properly yet: Gatus **#1245** ("Support for IPv6 DNS resolution for HTTP requests",
open since 2025-09-04, 9 👍) and **#1634** ("ipv6 support for tcp/udp", open) **[verified]**; Kuma
**#7258** (IPv4/IPv6 fallback for HTTP monitors, open) **[verified]**. Blackbox is the exception,
via `preferred_ip_protocol` **[recalled]**, because it is used by people who run dual-stack
production networks.

### 4.6 Alert on certificate and DNS *change*, not only on expiry

**Mechanic.** Store a fingerprint of the leaf certificate's public key and the issuer DN; alert when
the issuer changes unexpectedly, or when the key changes outside an expected renewal window. Same
idea for the A/AAAA/NS/MX sets.

**Why it is absent.** ACME rotates certificates every ~60 days and CDNs rotate constantly, so a naive
implementation is pure noise. It only becomes useful with an *expected issuer* allowlist — that is,
it requires the operator to state an intention, and most tools avoid features that require
configuration to be non-annoying.

SPEC.md §5.5 already specifies DNS zone drift, which is genuinely rare among these tools and worth
keeping. Extending the same idea to the certificate chain is nearly free, since §5.3 already has the
chain in hand.

### 4.7 Delay-and-collapse instead of drop-on-suppress

**Mechanic.** §3.2's fix, stated as a distinct mechanic: hold a child's alert for the parent's
detection window before delivering, and if the parent goes DOWN inside that window, collapse the
child into the parent's message rather than sending it.

**Why it is absent.** It deliberately adds latency to *every* alert, which is a hard sell in a
product whose marketing metric is time-to-detect. It also requires the alert pipeline to be queued
rather than synchronous (§4.1), which most implementations are not. For a homelab, 30–60 s of added
latency in exchange for never receiving a 20-message storm is an obviously good trade — but it is a
trade a general-purpose product cannot make on the user's behalf.

### 4.8 Measure the monitor's own detection quality

**Mechanic.** Per incident, record `first_failure_ts`, `state_change_ts`, `alert_enqueued_ts`,
`alert_delivered_ts`. Export `lookout_alert_latency_seconds` and count suppressed alerts. The weekly
heartbeat (§12) then carries real content: *"N checks, M incidents, longest time-to-alert 4m12s,
14 alerts suppressed by dependency, 1 delivery retry"*.

**Why it is absent.** It is introspection with no user-facing feature value, and it makes the tool's
own weaknesses legible — which is a disincentive for a product and an advantage for a homelab tool
you have to trust unattended.

### 4.9 Config-change-aware alert identity

**Mechanic.** Decide explicitly whether editing a check's alert configuration resets its "already
notified" state, and document it.

**Why it matters.** Gatus keys `endpoint_alerts_triggered` by `configuration_checksum` **[verified]**,
so editing an alert's config *does* reset it — a config reload during an ongoing incident re-pages
you. Nobody documents this, and it is discovered during an incident, which is the worst time.

### 4.10 Verify the mute is visible

**Mechanic.** SPEC.md §8 already requires mute windows to be visible on the status page — good, and
rarer than it sounds. Extend it: a mute that is *currently active* should be included in the weekly
heartbeat, and a mute older than its declared duration should be an alert in itself. The classic
failure is a "30-minute" mute that was never lifted.

---

## 5. Domain and certificate expiry: RDAP, WHOIS, and library choice

### 5.1 Live verification of SPEC.md §5.4

All checks performed 2026-08-19 from this machine. **SPEC.md's claims hold.**

**`.ru` has no RDAP — confirmed at the authoritative source** **[verified]**. The IANA bootstrap
registry `https://data.iana.org/rdap/dns.json` (publication `2026-07-23T02:00:03Z`) contains 590
service entries covering **1200 TLDs**, and `ru` is **not among them**.

```
$ jq -r '[.services[] | .[0][]] | index("ru") // "NOT PRESENT"' dns.json
NOT PRESENT
```

**rdap.org is a redirector** **[verified]**:

```
$ curl -o /dev/null -w "%{http_code} %{redirect_url}" https://rdap.org/domain/example.com
302 https://rdap.verisign.com/com/v1/domain/example.com
$ curl -o /dev/null -w "%{http_code}" https://rdap.org/domain/yandex.ru
404
```

**Going straight to the registry works** **[verified]** — SPEC.md's `events[]` field shape is correct:

```
$ curl -s https://rdap.verisign.com/com/v1/domain/example.com | jq -r '.events[]|"\(.eventAction): \(.eventDate)"'
registration: 1995-08-14T04:00:00Z
expiration:   2027-08-13T04:00:00Z
last changed: 2026-08-14T08:01:43Z
```

**`.ru` WHOIS over TCP/43 works and the field names are right** **[verified]**:

```
$ (printf 'yandex.ru\r\n') | nc whois.tcinet.ru 43
domain:        YANDEX.RU
state:         REGISTERED, DELEGATED, VERIFIED
registrar:     RU-CENTER-RU
created:       1997-09-23T09:45:07Z
paid-till:     2026-09-30T21:00:00Z
free-date:     2026-11-01
source:        TCI
```

Three details SPEC.md does not capture (objection O17):

- **`paid-till` is not the death date.** `free-date` here is ~32 days later — that is when the domain
  is actually released. Alerting on `paid-till` is correct, but the message should show both.
- **`paid-till` is Moscow midnight rendered in UTC** (`21:00:00Z` = 00:00 MSK+3). Naive date
  arithmetic in local time will be off by a day. Parse the timestamp, do not slice the string.
- **`state:` is the real signal.** Losing `DELEGATED` means the domain has stopped resolving *now*.
  That is an emergency of a different order than "expires in 14 days", and it is available in the
  same response for free.

### 5.2 The TLD→WHOIS table does not need to be hard-coded

SPEC.md §5.4 specifies *"фолбэк на whois по таблице TLD→whois-сервер"*. A hard-coded table is a
maintenance liability that goes stale silently. **`whois.iana.org` returns the referral itself**
**[verified]**:

```
$ (printf 'ru\r\n')       | nc whois.iana.org 43 | grep -E '^(domain|whois|status):'
domain:       RU
whois:        whois.tcinet.ru
status:       ACTIVE

$ (printf 'xn--p1ai\r\n') | nc whois.iana.org 43 | grep '^whois:'
whois:        whois.tcinet.ru       # IDN .рф resolves correctly via punycode
```

So the discovery chain becomes, with no hard-coded data at all:

```
1. IANA RDAP bootstrap (data.iana.org/rdap/dns.json, cached in DB, refresh weekly)
   → registry RDAP base URL → GET {base}/domain/{name} → events[eventAction=="expiration"]
2. on 404 / absent TLD:
   whois.iana.org:43 → "whois:" referral (cached in DB, long TTL)
   → that server:43 → parse per-TLD text
3. per-TLD text parsers, one small function each, table-tested on fixtures (SPEC.md §13 already
   requires this)
```

This removes rdap.org from the critical path — which SPEC.md §1.3 ("ноль внешних сервисов") requires
and §5.4 violates (objection O1) — and removes the static TLD table. Both bootstrap responses are
cacheable for weeks; the extra cost is one IANA fetch per TLD per month.

### 5.3 Library candidates

| Library | Stars | Last push | Open | License | OSV |
|---|---|---|---|---|---|
| `github.com/openrdap/rdap` | 409 | 2026-07-16 | 11 | MIT | none known |
| `github.com/likexian/whois` | 493 | 2026-07-03 | 10 | Apache-2.0 | none known |
| `github.com/domainr/whois` | 419 | 2026-07-17 | 4 | MIT | none known |

All metrics **[verified]** via GitHub API and the OSV API, 2026-08-19.

**openrdap/rdap — freshness is misleading; check the shape of the history.** `pushed_at` says
2026-07-16, which looks healthy. The commit log says otherwise **[verified]**:

```
2026-07-16  feat: derive CLI version from build info and add binary release workflow (#52)
2026-06-23  Rename 'Registrar WHOIS Server' to 'WHOIS Server' (#46)
2026-06-23  chore: modernize, optimize, and document the RDAP library (#49)
2026-06-05  Use XDG_CACHE_HOME for the bootstrap cache directory (#48)
2026-06-05  Implement AS search requests (#27)
2026-06-05  Change the registry accessor functions in bootstrap/client ... (#40)
2026-06-05  Spelling (#45)
2026-06-05  Create rdap.1 (#33)
2024-05-17  Fix indenting in --help message         ← 25-month gap
```

That is a **project dormant for 25 months, revived in June 2026 by a new maintainer** clearing a
backlog of years-old community PRs. Assessment:

- *Positive:* MIT, no known vulnerabilities, correct full bootstrap implementation, someone is
  actively caring for it right now.
- *Risk:* the revival is roughly two months old and appears to rest on one person. A single dormancy
  cycle has already happened. The `chore: modernize` commit and the `#46` rename indicate the public
  API is currently in motion — pinning a version is essential, and an upgrade may not be trivial.
- *Scope mismatch:* it implements the entire RDAP object model (entities, nameservers, autnums,
  vCard/jCard parsing, search queries, the whole CLI). lookout needs **one field**:
  `events[] where eventAction == "expiration"`.

**Recommendation: take no library for this.** SPEC.md §3's rule — *"Реально нужен, или это пара
своих строк?"* — answers it. The concrete implementation is:

- RDAP: `net/http` GET + `encoding/json` into a 6-line struct. The bootstrap file is one more GET
  plus a `map[string]string` built once and cached in the DB.
- WHOIS: raw `net.Dial("tcp", host+":43")`, write `name\r\n`, `io.ReadAll` with a
  `SetDeadline` and a size cap, then `bufio.Scanner` over the lines. SPEC.md already specifies raw
  `net.Dial`, and it is right.

That is on the order of 150 lines total, versus three dependencies whose combined surface is two
orders of magnitude larger and whose maintenance you cannot influence. It is also the only option
that keeps the SPEC.md §3 dependency list at two entries.

If a library is taken anyway: `openrdap/rdap` pinned to an exact version, re-evaluated in six months
to see whether the revival held.

### 5.4 The certificate-expiry trap (SPEC.md §5.3)

"Free from the handshake" is correct and worth doing, but it inherits Blackbox Exporter's silent
failure (§1.5): **if the certificate is already expired, or the chain is untrusted, or the hostname
mismatches, the handshake fails — so there is no certificate data, and the expiry alert never
fires.** You get a generic "check down" at the exact moment the specific message would have been
most useful.

Mitigation: install a `tls.Config.VerifyPeerCertificate` callback that captures
`rawCerts[0]` **before** returning the verification error. The leaf is then available for the alert
message even on a failed handshake, without weakening verification for the check verdict itself.
Also note §4.4: on a reused keep-alive connection there is no handshake at all, so the captured
certificate is as old as the connection.

---

## 6. SQLite: retention and aggregation without file growth

### 6.1 The arithmetic that forces the design

Raw results for a modest homelab — 30 checks at a 60 s interval — with per-row size estimated
between 70 B (id, check_id, timestamp, status, duration, success) and 250 B (the same plus an error
string and a body excerpt), including index entries:

| Retention | Rows | @70 B | @150 B | @250 B |
|---|---|---|---|---|
| 30 d | 1 296 000 | 91 MB | 194 MB | 324 MB |
| **90 d (SPEC.md §9)** | **3 888 000** | **272 MB** | **583 MB** | **972 MB** |
| 365 d | 15 768 000 | 1.1 GB | 2.4 GB | 3.9 GB |

The 250 B column is the realistic one once errors are stored — it is consistent with Uptime Kuma's
reported 6 GB for 200 monitors at 90 days **[secondary]**, which back-solves to roughly 230 B/row.

The same information as **hourly buckets for 7 days, then daily buckets**:

| | Rows | Size |
|---|---|---|
| hourly, 7 d (30 checks × 24 × 7) | 5 040 | |
| daily, next 83 d (30 × 83) | 2 490 | |
| **total** | **7 530** | **≈ 0.5 MB** |

**Three orders of magnitude.** This is the whole answer to "history without file growth", and it is
why Gatus's database stays small while Kuma's reaches gigabytes.

### 6.2 Schema

```sql
-- raw, short retention (7d): needed for the "last N points" API and incident forensics
CREATE TABLE results (
  id        INTEGER PRIMARY KEY,
  check_id  INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
  ts        INTEGER NOT NULL,          -- unix seconds
  outcome   INTEGER NOT NULL,          -- 0=up 1=down 2=unknown   (§4.2)
  phase     INTEGER NOT NULL,          -- failing phase           (§4.3)
  status    INTEGER NOT NULL,
  duration  INTEGER NOT NULL,          -- ms
  err       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX results_check_ts ON results (check_id, ts);

-- hourly rollup (90d), and an identical daily rollup (unbounded)
CREATE TABLE uptime_hourly (
  check_id     INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
  bucket       INTEGER NOT NULL,       -- unix ts floored to the hour
  total        INTEGER NOT NULL,       -- denominator EXCLUDING unknown
  ok           INTEGER NOT NULL,
  unknown      INTEGER NOT NULL,       -- counted separately, never in total
  sum_duration INTEGER NOT NULL,       -- SUM, not AVG
  max_duration INTEGER NOT NULL,
  PRIMARY KEY (check_id, bucket)
) WITHOUT ROWID;
```

Four non-obvious points:

1. **Store `sum` and `count`, never `avg`.** Averages cannot be merged across buckets of unequal
   size. Gatus stores `total_response_time` + `total_executions` for exactly this reason
   **[verified]**. The same applies to any downsampling step you might add later.
2. **Percentiles do not merge at all.** If p95 is ever wanted, it must be fixed-boundary histogram
   buckets stored per rollup row, or a t-digest. `avg` + `max` is a conscious information loss —
   make it a documented decision rather than a discovery.
3. **`unknown` is counted but excluded from `total`** (§4.2), so uptime means "of the checks that
   actually ran".
4. `WITHOUT ROWID` on the rollup tables: the primary key *is* the access path, and it saves a
   duplicate index.

### 6.3 Why `DELETE` does not shrink the file, and what to do

**[verified]**, SQLite documentation for `VACUUM`:

- Deleting rows marks pages free for **reuse**; the file does not shrink. This is Kuma #1994 —
  reducing retention from 400 to 30 days left the file at 1 718.5 MB **[secondary]**.
- `VACUUM` rebuilds the file and requires **up to 2× the database size** in free space plus an
  exclusive lock, and fails if any transaction or unfinalized statement holds a read lock.
- `VACUUM INTO 'new.db'` writes a compacted copy without overwriting the original, avoiding the
  long exclusive lock on the live file.
- `auto_vacuum` reclaims incrementally but does **not** compact partially-filled pages and can
  increase fragmentation. In WAL mode, `auto_vacuum` is the only property `VACUUM` can change.

**The recommendation is to make `VACUUM` unnecessary.** If retention is enforced by rollups, the row
count reaches a steady state, deleted pages are continuously reused by new inserts, and the file
plateaus at a few tens of megabytes and stays there. Compaction is then a one-off recovery tool
(`VACUUM INTO` + atomic rename, offline), not a scheduled job. This is a design property, not a
tuning knob — and it is the property Uptime Kuma lacked.

### 6.4 Two operational traps

**Trap 1: the retention job blocks the probes.** This is literally Kuma #2470 — *"Shrinking database
/ blocking database operations give false downtime"* **[secondary]**. A single
`DELETE FROM results WHERE ts < ?` over millions of rows holds the write lock for seconds; probe
writes hit `SQLITE_BUSY`, and — in a naive implementation — a failed write is reported as a failed
check. Two mitigations, both required:

```sql
DELETE FROM results WHERE id IN (
  SELECT id FROM results WHERE ts < ? LIMIT 5000
);  -- loop until 0 rows affected, with a pause between batches
```

and: **a storage error must never be reported as a check failure.** It is an `unknown` result plus a
degraded `/healthz`. Getting this wrong turns a database maintenance window into a false outage —
the monitoring equivalent of a self-inflicted wound.

**Trap 2: the `-wal` file grows without bound.** `wal_autocheckpoint` defaults to 1000 pages
**[recalled]**, but a **long-lived read transaction prevents checkpointing entirely**. A status-page
handler that holds a read transaction open while streaming a response can let `-wal` grow to
gigabytes even though the main file is small. Keep read transactions short; buffer the response
before writing it.

### 6.5 Connection handling with `modernc.org/sqlite`

`modernc.org/sqlite` **[verified]**: no known OSV vulnerabilities, actively developed (last push
2026-08-18). Pure Go, so `CGO_ENABLED=0` holds — SPEC.md §3's reasoning is sound.

Practices, **[secondary]** except where noted:

- Pragmas must be set **per connection**, not once — `busy_timeout` in particular is a
  per-connection setting. Use a DSN that carries them, or a connection hook.
- `journal_mode=WAL`, `busy_timeout=5000`, `synchronous=NORMAL`, `foreign_keys=ON`.
- **SQLite allows exactly one writer at a time**, in WAL mode as well as rollback mode. The most
  effective way to avoid `SQLITE_BUSY` is a **write pool of one connection**
  (`db.SetMaxOpenConns(1)`) plus a separate read pool — more effective than raising `busy_timeout`.
- **Never upgrade a transaction from read to write.** Use `BEGIN IMMEDIATE` when a transaction will
  write; otherwise SQLite can only report the conflict at upgrade time, when it is too late to back
  off cleanly.
- SPEC.md §9 says *"потеря БД не должна мешать старту"* — good, and it should be tested: start with
  a missing file, a zero-byte file, and a corrupt file, and confirm the process still runs checks
  and still alerts.

---

## 7. Naming: the canonical vocabulary

The domain has an established vocabulary, and inventing a parallel one costs comprehension for
nothing. Terms actually used by the analogs:

| Concept | Nagios/Icinga | Prometheus | Gatus | Kuma | healthchecks | suggested |
|---|---|---|---|---|---|---|
| the thing being tested | host / service | target | **endpoint** | monitor | check | **check** |
| one execution | check / plugin run | **probe** | execution | heartbeat | ping | **probe** |
| one outcome | check result | sample | **result** | heartbeat | ping | **result** |
| a state transition | state change | — | **event** | important event | flip | **event** |
| an assertion | threshold | rule | **condition** | keyword | — | **condition** |
| an outage | problem | alert | — | — | — | **incident** |

**States.** The canonical set is the Monitoring Plugins exit codes **[secondary]**: `0 OK`,
`1 WARNING`, `2 CRITICAL`, `3 UNKNOWN`. Two further distinctions from Nagios are worth adopting by
name because lookout's design needs both concepts anyway and will otherwise invent worse names:

- **SOFT vs HARD state** **[recalled]** — SOFT means "failing but the threshold is not yet met", HARD
  means "confirmed". SPEC.md §6's threshold machine produces exactly this distinction and currently
  has no word for it. Notifications fire on HARD transitions only.
- **DOWN vs UNREACHABLE** **[secondary]** — "it is broken" vs "a dependency is broken so I cannot
  tell". This is §4.2's `unknown` and §3's dependency suppression, already named, decades ago.

Prometheus's alert lifecycle is the other canonical trio and maps 1:1 onto SPEC.md §6:
**`inactive → pending → firing`** **[recalled]**, where `pending` is the `for` duration — the same
thing as Nagios SOFT. Using `pending` and `firing` makes the state machine legible to anyone who has
seen Prometheus, which is most people.

Suggested state set for lookout:

```
outcome (per result):  up | down | unknown
state   (per check):   up | pending | down | flapping        (+ reason for unknown)
```

**Go naming.** Standard library convention, not domain convention:

- Single-method interfaces are named for the verb plus `-er`: `Prober` with `Probe(ctx) Result`, or
  `Checker` with `Check(ctx) Result`. `Notifier` with `Notify(ctx, Alert) error` — SPEC.md §6
  already names this correctly.
- Do not prefix getters with `Get`.
- Package names: short, singular, lowercase, no underscores — `check`, `probe`, `alert`, `store`,
  `config`. Avoid `util`, `common`, `helpers`, `manager`, and stuttering (`check.CheckResult` should
  be `check.Result`).
- Return `Result` by value; it is small and copying beats aliasing a shared struct across goroutines.

---

## 8. Go idioms: scheduler and graceful shutdown

### 8.1 The scheduler

**Do not use `for { doWork(); time.Sleep(interval) }`.** The period becomes
`interval + work_duration`, so every check drifts, and checks drift by *different* amounts depending
on how slow their targets are. Uptime percentages computed from an assumed sample rate are then
subtly wrong.

**Compute the next tick from an absolute origin.** With a per-check goroutine and a `time.Timer`:

```go
next := origin.Add(interval)                    // origin fixed at start, not time.Now()
for {
    timer.Reset(time.Until(next))
    select {
    case <-ctx.Done():
        return
    case <-timer.C:
    }
    runProbe(ctx)
    for !next.After(time.Now()) {               // skip missed ticks after a long stall
        next = next.Add(interval)
    }
}
```

`time.Ticker` is also acceptable — it drops ticks rather than queueing them when the receiver is
slow, which is the desirable backpressure behaviour — but an explicit `next` makes the missed-tick
policy visible instead of implicit.

**Deterministic phase offset, not random jitter.** SPEC.md §5.1 puts every HTTP check on 60 s, so
all of them fire at the same instant, hammering the LAN in bursts and leaving it idle in between:

```go
origin := start.Add(time.Duration(fnv32(check.Name)) % interval)
```

Hashing the check name spreads the herd **and is stable across restarts**, so a restart does not
re-align everything into a new thundering herd. Random jitter is worse here: it re-randomises on
every restart and makes tick timestamps non-reproducible in tests. Reserve random jitter for
SPEC.md §5.4's daily domain checks, where the goal is being polite to a third party rather than
smoothing local load.

**Bound concurrency.** Per-check goroutines are fine at homelab scale (tens), but a network stall
can leave dozens of probes in flight simultaneously. A buffered-channel semaphore is stdlib-only and
sufficient:

```go
sem := make(chan struct{}, maxConcurrent)
sem <- struct{}{}; defer func() { <-sem }()
```

`golang.org/x/sync/errgroup` with `SetLimit` is more ergonomic but adds a dependency for something
that is three lines — and SPEC.md §3's rule points at the three lines.

**Timeout must be strictly less than interval.** Enforce it in the validator (SPEC.md §5), not at
runtime. Otherwise probes overlap and the results are not independent samples.

### 8.2 Shutdown

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
```

`signal.NotifyContext` (Go 1.16+) is the idiom; a manual `signal.Notify` + channel + goroutine is
strictly more code for the same behaviour.

Shutdown order matters and is easy to get wrong:

1. cancel the root context — schedulers stop issuing new probes;
2. `srv.Shutdown(shutdownCtx)` with its **own** bounded context (the root one is already cancelled;
   passing a cancelled context makes `Shutdown` return immediately and abort in-flight requests);
3. `wg.Wait()` for in-flight probes, bounded by a deadline;
4. **flush the alert outbox** (§4.1) — dropping a pending alert on shutdown is precisely the
   SPEC.md §1 failure mode, in a code path nobody tests;
5. checkpoint and close the database.

Per-probe contexts derive from the root with `context.WithTimeout`, so cancellation propagates into
`net/http` and every dial without any custom plumbing.

### 8.3 Testing time-driven logic: `testing/synctest`

**[verified on this machine]** — `testing/synctest` is in the Go 1.26 standard library
(`go doc testing/synctest`):

> "The Test function runs a function in an isolated 'bubble'… Within a bubble, the time package uses
> a fake clock… Time in a bubble only advances when every goroutine in the bubble is durably
> blocked. For example, this test runs immediately rather than taking two seconds."

This is the single most valuable testing recommendation in this report. Everything hard to test in
lookout is clock-driven: the flap state machine (§2), the notification tiers (SPEC.md §6, which must
fire *exactly once* per threshold), the batching window (§3.3-S1), the retention job, the domain
cache TTL, and the scheduler's drift behaviour. With `synctest` these become deterministic,
millisecond-fast unit tests over simulated days. Without it, they become either slow tests or an
injected `Clock` interface threaded through every constructor — more code, and a permanent tax on
readability.

SPEC.md §13 should require it explicitly. It also directly serves the mandated test that a check
without `alert:` alerts, and the tier tests that "не должно быть повторов".

Note the documented constraint: *"Avoid using the network. Use a fake network implementation as
needed."* So probes must be testable against `httptest` with an injectable `*http.Client`, which is
good design regardless.

---

## 9. Objections to SPEC.md

Agreeing with everything would make this report useless. Ordered by how much damage the issue does.

### 9.1 Contradictions with the spec's own principles

**O1 — §1.3 "ноль внешних сервисов" is violated by §5.4 and §5.2.** `rdap.org` is a third-party
redirector on the critical path (**[verified]**: it answers 302 and nothing else), and the default
DNS resolver `8.8.8.8` is a third-party service that also receives a copy of your query pattern.
*Fix:* use `data.iana.org/rdap/dns.json` plus the registry directly, and `whois.iana.org` for WHOIS
referrals (§5.2 — both verified working). Default the DNS check to the **LAN resolver**, with a
public resolver as an explicit, opt-in comparison target — for a homelab, "is my own resolver
answering correctly" is the question worth asking, and it is not the one §5.2 currently asks.

**O2 — §4 rejects a runtime mini-language, then introduces one.** The stated reason for rejecting
`"[STATUS] == 200"` is that it *"парсится в рантайме и падает на опечатке"*. But
`body: {".result.source.online": true}` is a path expression parsed at runtime with the same typo
exposure — and a worse failure direction: a mistyped path yields "not found", which naturally
evaluates as a failed condition, i.e. **a config typo becomes a false alert**, and false alerts are
what §1 is about. *Fix:* (a) the validator must reject syntactically invalid paths at
`lookout validate` time; (b) a path that does not resolve at runtime must produce `unknown`, not
`down`; (c) say so in the spec, because the current text implies a guarantee the design does not
provide.

### 9.2 Gaps that reopen the founding bug

**O3 — §6 has no durable alert outbox. This is the most serious omission.** §1 says silence must
never be a bug, but the design's last mile is a single HTTP call to a service §6 itself admits may
be unreachable. If it fails, the alert is gone and nothing records that it was gone. *Fix:* §4.1 —
an `outbox` table written in the same transaction as the state change, a retry worker with backoff,
`lookout_undelivered_alert_age_seconds` in `/metrics`, `/healthz` degraded while the outbox is
stuck, escalation to the secondary notifier after M minutes. Evidence this is the real-world failure
mode: Kuma #7612, #922 (§1.3).

**O4 — §12's weekly heartbeat is far too slow, and shares fate with what it watches.** Worst-case
discovery that lookout is dead is ~7 days. Worse, if it goes to the same Telegram chat over the same
transport as the alerts, then the transport failing kills the alerts *and* the heartbeat
simultaneously — the sentinel is inside the blast radius. *Fix:* daily at minimum, and push it to an
**external** dead-man's-switch with healthchecks.io semantics (period + grace), so absence is the
signal rather than presence. This is the one place a small external dependency genuinely buys
something §1.3 cannot: an outside observer.

**O5 — no notification batching (§6/§7).** Without §3.3-S1, the dependency graph is the *only*
anti-spam mechanism, and it only helps for relationships someone remembered to declare correctly.
Add the batching window; it is simpler than the graph, solves §7's stated goal generically, and fixes
the §3.2 ordering race that the graph alone does not.

**O6 — no global `unknown` state.** §5.4 has `unknown` for domains only. Restarts, config reloads,
mute windows and dependency suppression all currently have to be recorded as up, down, or nothing —
all three of which are wrong. Make it a first-class outcome (§4.2).

**O7 — §5.1 has no phase offset.** Every 60 s check fires simultaneously. Add
`hash(name) % interval` (§8.1).

**O8 — §11 has no authentication.** "Слушает только LAN" is a deployment note, not a security
boundary — a homelab LAN contains IoT devices, guests and containers. §8's `lookout mute` endpoint
*mutates alerting state*, and muting a monitor is the ideal first step for anything malicious.
*Fix:* a static bearer token from the environment on all mutating endpoints; keep `GET /` and
`/api/status` open if you like. The unix-socket option in §8 is the better default — prefer it and
make the HTTP variant opt-in.

### 9.3 Dependency risk

**O9 — §3 sanctions `gopkg.in/yaml.v3`, which is dead.** **[verified]**: the repository is archived
(`archived: true`), the last real code commit was **2022-05-27**, and the README's first line is:

> **# THIS PROJECT IS UNMAINTAINED**
> *"…I cannot just 'hand off' maintenance to an individual or to a small group either, due to the
> likelyhood of the project going back into an unmaintained, unstable, or even abused state."*

So there will be no successor under that import path, by the author's explicit decision. It carries
one historical advisory (CVE-2022-28948 / GO-2022-0603, fixed in v3.0.1) **[verified via OSV]**;
the point is not that vulnerability but that the next one will not be fixed. This directly
contradicts the CLAUDE.md operating rule that new vulnerabilities are caught by a daily `osv-scan`
and then patched — for this package there is nothing to patch to.

*Options, in preference order:* (1) `github.com/goccy/go-yaml` — MIT, no known OSV vulnerabilities,
last push 2026-04-11, v1.19.2 **[verified]**; but a much larger surface, 212 open issues, and
documented behavioural differences from yaml.v3 that will surface in edge cases. (2) Keep yaml.v3
pinned as a **deliberate, documented** frozen dependency — defensible for a YAML subset this small,
since a parser that never changes also never regresses, but it must be a stated decision with a
review date. (3) Reconsider the format: SPEC.md's config uses only maps, lists, strings and scalars.
Whichever is chosen, **§3 must stop citing an unmaintained package as the sanctioned choice** without
comment.

**O10 — §3 does not declare the SOCKS5 dependency that §6 requires.** **[verified on this machine]**:
SOCKS5 is not in the standard library, and Go's vendored `golang.org/x/net` contains only
`dns http http2 idna lif nettest` — no `proxy`. So §6's mandatory SOCKS5 support needs either
`golang.org/x/net/proxy` (a third dependency, undeclared, and `golang.org/x/net` has the longest
advisory history of anything on the list **[verified via OSV]**) or ~80 lines of hand-written
RFC 1928 CONNECT. Given SPEC.md §3's own test — *"Реально нужен, или это пара своих строк?"* — the
hand-written handshake is the consistent answer for a single client with username/password auth.
Either way, the spec must say which.

### 9.4 Over-scoped for v1

**O11 — §9's 90-day raw retention has no consumer and costs 0.3–1.0 GB.** §10 exposes 24h/7d/30d
only. *Fix:* raw results 7 days, hourly rollups 90 days, daily rollups indefinitely — ~0.5 MB total
(§6.1). Also aligns 90-day retention with something that actually reads it.

**O12 — §5.6 (external vantage point via SOCKS5) should be v2.** It adds a per-check dialer path, an
`external:` config surface, and the dependency question of O10, to serve a case §2 already declares
out of scope ("агенты на других хостах"). In v1, SOCKS5 is genuinely needed for **one** thing: the
notifier's HTTP client (§6), which is a single client with a single proxy. Ship that; defer the
per-check proxy.

**O13 — §4's `tls` check type is described as *"только если нужен"*.** §2 says "не пишем на будущее
и не оставляем заглушек". Apply that rule here: drop `tls` from v1 until an actual SMTP/IMAP target
exists. §5.3 already covers every HTTPS endpoint for free.

**O14 — §5.5's hourly DNS zone drift is 24× more often than the data changes.** Zone records change
on the order of months. Hourly buys nothing and the alert semantics are unstated — a legitimate NS
change would page you at 3 a.m. *Fix:* daily, with an explicit `expected:` set in the config so that
"changed" means "changed from what I declared", not "changed from what it was an hour ago". Note the
underlying feature is genuinely rare and worth keeping — this is a criticism of the interval and the
semantics, not the idea.

### 9.5 Correctness details

**O15 — §5.4: `paid-till` is not the deletion date.** **[verified]** on `yandex.ru`:
`paid-till: 2026-09-30T21:00:00Z`, `free-date: 2026-11-01` — 32 days of grace. Also, `paid-till` is
Moscow midnight expressed in UTC, so naive local-time date arithmetic is off by a day. And `state:`
losing `DELEGATED` is a *current* outage, categorically different from a future expiry, available in
the same response. Show all three.

**O16 — §5.3's certificate expiry fails silently exactly when it matters.** If the certificate is
already expired or the chain is untrusted, the handshake fails, so there is no certificate to read
and the alert says "check down" rather than "certificate expired" (§5.4, and Blackbox's #637/#1406
are the same bug class). *Fix:* capture `rawCerts[0]` in a `VerifyPeerCertificate` callback before
returning the verification error.

**O17 — §5.3 is additionally void on reused connections.** "Free from the handshake" assumes a
handshake happens. Go's default transport reuses keep-alive connections, so after the first probe
there is none (§4.4). Set `DisableKeepAlives: true` on the probe client — and note that this also
restores DNS and TCP coverage that the spec currently assumes it has.

**O18 — §4's `depends_on` is a scalar; it needs to be a list.** Real dependencies are conjunctive:
a service depends on the gateway *and* the DNS resolver. Changing the YAML shape after deployment is
a breaking config change; making it a list now costs nothing.

**O19 — §10 says "версионировать" without saying how.** Ship `/api/v1/status` from the first commit.
Deciding later means either a flag day or two code paths.

**O20 — §13's mandatory test tests the wrong half of the founding lesson.** "A check without an
explicit `alert:` must alert" verifies the config **default**. But the Gatus incident in §1 was not a
missing default — the alert was configured and still did not arrive. *Add two tests:* (a) end-to-end,
a state transition results in a **delivered** notification through a fake notifier; (b) a notifier
that returns errors results in retries, a non-empty outbox, and a **visibly degraded** `/healthz` —
i.e. the system notices its own silence. That is the actual lesson.

### 9.6 What I would cut from v1

Applying §2's own rule strictly: drop the `tls` check type (O13), drop §5.6's external vantage point
(O12), drop 90-day raw retention (O11), and reduce §5.5 to daily (O14). Spend the freed effort on
the alert outbox (O3), the batching window (O5), and the global `unknown` state (O6) — three
mechanics that directly serve §1's founding principle and that none of the five analogs implement
well.

---

## 10. Sources

Primary sources verified during this research (2026-08-19):

- Gatus source — [`storage/store/sql/specific_sqlite.go`](https://raw.githubusercontent.com/TwiN/gatus/master/storage/store/sql/specific_sqlite.go), [`storage/store/sql/sql.go`](https://raw.githubusercontent.com/TwiN/gatus/master/storage/store/sql/sql.go), [`README.md`](https://raw.githubusercontent.com/TwiN/gatus/master/README.md)
- [Gatus #1708 — endpoint dependencies (closed without implementation)](https://github.com/TwiN/gatus/issues/1708) · [#679 — alert state not persistent](https://github.com/TwiN/gatus/issues/679) · [#1245 — IPv6 DNS resolution](https://github.com/TwiN/gatus/issues/1245)
- [IANA RDAP bootstrap `dns.json`](https://data.iana.org/rdap/dns.json) — publication 2026-07-23, 1200 TLDs, `.ru` absent
- `whois.tcinet.ru:43`, `whois.iana.org:43` — live TCP/43 queries
- [`rdap.verisign.com`](https://rdap.verisign.com/com/v1/domain/example.com), `rdap.org` redirect behaviour
- [go-yaml/yaml README — "THIS PROJECT IS UNMAINTAINED"](https://raw.githubusercontent.com/go-yaml/yaml/v3/README.md)
- GitHub REST API (repo metrics, commit history, issue search) and [OSV API](https://api.osv.dev/) for all listed packages
- Go 1.26.6 toolchain on this machine — `go doc testing/synctest`, vendored `golang.org/x/net` contents
- [SQLite — VACUUM](https://www.sqlite.org/lang_vacuum.html)
- [Nagios Core — Detection and Handling of State Flapping](https://assets.nagios.com/downloads/nagioscore/docs/nagioscore/4/en/flapping.html)

Secondary sources:

- [Uptime Kuma #1994](https://github.com/louislam/uptime-kuma/issues/1994) · [#2470](https://github.com/louislam/uptime-kuma/issues/2470) · [#3261](https://github.com/louislam/uptime-kuma/issues/3261) · [#5187](https://github.com/louislam/uptime-kuma/issues/5187) · [#1137](https://github.com/louislam/uptime-kuma/issues/1137) · [#7612](https://github.com/louislam/uptime-kuma/issues/7612)
- [blackbox_exporter #1406](https://github.com/prometheus/blackbox_exporter/issues/1406) · [#637](https://github.com/prometheus/blackbox_exporter/issues/637) · [The various phases of Prometheus Blackbox's HTTP probe](https://utcc.utoronto.ca/~cks/space/blog/sysadmin/PrometheusBlackboxHTTPDurations)
- [Prometheus — Alerting rules (`for`, `keep_firing_for`)](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/) · [prometheus #13957](https://github.com/prometheus/prometheus/issues/13957)
- [Alertmanager configuration — `inhibit_rules`, `group_by`, `group_wait`](https://prometheus.io/docs/alerting/latest/configuration/)
- [Zabbix — Trigger dependencies](https://www.zabbix.com/documentation/current/en/manual/config/triggers/dependencies) · [Nagios — Determining Status and Reachability of Network Hosts](https://assets.nagios.com/downloads/nagioscore/docs/nagioscore/3/en/networkreachability.html)
- [RFC 2439 — BGP Route Flap Damping](https://www.rfc-editor.org/rfc/rfc2439.html)
- [Google SRE Workbook — Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/)
- [Monitoring Plugins — Development Guidelines](https://www.monitoring-plugins.org/doc/guidelines.html)
- [healthchecks.io — FAQ](https://healthchecks.io/faq/) and [docs](https://healthchecks.io/docs/)
- [SQLITE_BUSY despite a timeout — Bert Hubert](https://berthub.eu/articles/posts/a-brief-post-on-sqlite3-database-locked-despite-timeout/)

Explicitly **unverified** claims, repeated here so they are not mistaken for facts: Nagios's default
low/high flap thresholds (5 %/20 % host, 5 %/25 % service) — the official page does not publish them;
RIPE's recommendation against BGP route-flap damping with default parameters; Blackbox Exporter's
`preferred_ip_protocol` / `ip_protocol_fallback` semantics; SQLite's `wal_autocheckpoint` default of
1000 pages; Prometheus's `inactive → pending → firing` naming; Nagios's SOFT/HARD state naming.
Each should be re-checked before it influences an implementation decision.
