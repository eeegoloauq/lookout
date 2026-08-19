# Series 4 — DNS, zone drift, TLS and domain expiry

2026-08-19. Implements SPEC.md §5.2–§5.5 and the expiry tiers of §6.

## What shipped

1. **DNS checks (`type: dns`)** — `query_type` A/AAAA/MX/NS/TXT, conditions
   on `rcode` (default NOERROR) and `answers_contain`. Three UDP attempts:
   a decoded response, including NXDOMAIN/SERVFAIL, is final; only transport
   loss is retried. That is the Gatus failure mode from §5.2.
2. **Zone drift (§5.5)** — each decoded DNS answer set is snapshotted
   (rcode + sorted records) in durable state. The first probe is a baseline.
   A later different snapshot is `EventDrift`. This is how a registrar wipe
   during a transfer reads: `NOERROR` + MX → `NXDOMAIN`.
3. **TLS expiry (§5.3)** — the leaf `NotAfter` is taken from the HTTPS
   handshake of an `http` check. No extra request, no `tls` check type.
   `VerifyPeerCertificate` captures the leaf *before* verification, so an
   already-expired or mismatched cert still produces a date (research O16)
   while the probe itself stays down.
4. **Domain expiry (`type: domain`, §5.4)** — IANA RDAP bootstrap
   (`https://data.iana.org/rdap/dns.json`, cached weekly in the state file)
   → registry RDAP `events[eventAction==expiration]`. TLDs absent from the
   bootstrap (verified: `ru`, `su`, `xn--p1ai`) fall back to WHOIS on TCP/43.
   Built-in table: those three → `whois.tcinet.ru`. Other TLDs ask
   `whois.iana.org` for the `whois:` referral and cache it. The `.ru` parser
   reads `paid-till`, `free-date` and `state`. An unrecognised reply is an
   error naming the keys that were looked for and the keys that were seen —
   never a silent zero time.
5. **Tiers (§6)** — cert 21/14/7/3 then daily; domain 60/30/14/7 then daily.
   Each numbered threshold fires once (bitmask in durable state). Daily
   notices fire once per UTC day after the last numbered tier. A later
   `NotAfter` / `paid-till` (renewal) clears the bits. Source silence is
   `unknown`, not down; `EventStale` only after 72 hours.
6. **`/api/status` and `/metrics`** expose `cert_days_left` and
   `domain_days_left` as nullable / omitted-when-unknown. 0 means “expires
   within 24h”, not “we have no idea”.

## Live facts checked on this machine (2026-08-19)

Not copied from the research report:

- IANA bootstrap publication `2026-07-23T02:00:03Z`, 590 services / 1200 TLDs.
  `com` → `https://rdap.verisign.com/com/v1/`, `dev` →
  `https://pubapi.registry.google/rdap/`. `ru`, `su`, `xn--p1ai`, `example`
  are **absent**.
- RDAP `GET https://rdap.verisign.com/com/v1/domain/example.com` (reserved
  name) returns `events[].eventAction == expiration` at
  `2027-08-13T04:00:00Z`.
- `whois.tcinet.ru:43` for `example.ru` returns `paid-till:`, `free-date:`,
  `state:`. `paid-till` is a full RFC3339 timestamp (`21:00:00Z` = Moscow
  midnight).
- `whois.iana.org:43` for `ru` and `xn--p1ai` returns
  `whois: whois.tcinet.ru`.
- Verisign WHOIS uses `Registry Expiry Date:` — covered by the same parser.

Those live replies are **not** in the repository as named targets. Fixtures
reuse the field shapes with `service.example`.

## Deviations from SPEC.md

| Spec | What we did | Why |
|---|---|---|
| Default DNS resolver `8.8.8.8` (§5.2 / §9.2) | Empty `resolver:` uses the first `nameserver` in `/etc/resolv.conf`. No public resolver is hard-coded. | Research O1: a homelab monitor that phones 8.8.8.8 is asking a different question and leaking the query pattern. `resolver:` remains the opt-in for a public nameserver. |
| WHOIS via a static TLD→server table only | Table for `ru`/`su`/`xn--p1ai`, then IANA referral for everything else, both cached. | Research §5.2: a frozen table goes stale silently. The required tcinet path is still first-class. |
| Domain poll “once a day + jitter” | Default interval 24h. The existing name-hash phase offset is the jitter. Interval `< 1h` is rejected. | A 60s `defaults.interval` would otherwise scrape registries. The scheduler already spreads the first tick. |
| DNS codec “stdlib” | `golang.org/x/net/dns/dnsmessage` from the already-required `x/net` module. No new module. | Stdlib has no DNS message codec. Hand-rolling A/MX/NS/TXT packing is the worse surface. |
| IDN | Config must be written as punycode (`xn--…`). Non-ASCII names are a load-time error. | `x/net/idna` pulls in `golang.org/x/text`, a third dependency §3 does not allow. |
| Zone drift “once an hour” | Compared on every successful DNS decode (default every 5m). | The snapshot is free once the query has been made. Extra hourly gating would hide a wipe for up to an hour. |
| `unknown` only mentioned for domains | `OutcomeUnknown` is first-class: it does not move up/down streaks and is excluded from the uptime denominator. | Spec §5.4 plus research O6. HTTP/DNS still only produce up/down/malformed. |

## Deferred (out of this series, as asked)

- Mute windows (§8)
- systemd unit / deploy
- Long-term JSONL history (§9.3)
- TCP fallback when a DNS reply has TC set
- Alerting on `.ru` `state:` losing `DELEGATED` as a separate emergency
  (the field is stored and shown; it does not have its own event yet)
- External vantage point via SOCKS5 for probes

## Unverified

- A live weekly bootstrap *refresh* (the 7-day TTL is tested with a fake
  clock / injected `now` only indirectly; the fetch itself is tested against
  `httptest`).
- WHOIS text from registries other than tcinet and Verisign. The parser
  lists the keys it accepts; an unknown format fails loudly and will need
  another key, not a silent default.
- IPv6-only resolvers and IPv6 WHOIS listeners (the code uses
  `net.JoinHostPort`; tests bind `127.0.0.1`).
- A real `httptest` client against an expired cert on platforms with a
  different x509 policy than this machine’s Go 1.26.
- End-to-end lookout process against a live TLD (forbidden in tests; not
  run here either).
