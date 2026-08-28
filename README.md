# lion

A task-first LinkedIn CLI. One binary, parseable output, predictable
automation — modeled on [gogcli](https://github.com/openclaw/gogcli), applied
to LinkedIn.

> ⚠️ lion talks to LinkedIn's private **Voyager** API using your browser
> session cookie. This violates LinkedIn's Terms of Service and can get an
> account restricted or banned. Use a throwaway/test account. lion is
> conservative by default (rate limiting, human-like jitter, `--dry-run`), but
> you accept the risk.

## Status

v1. See [DESIGN.md](DESIGN.md) for the architecture and roadmap.

Working today:

- `lion auth login|status|logout` — import, validate, and remove a session
- `lion profile view me` · `lion profile search <query>`
- `lion connection list|invite|remove`
- `lion message list|read|send`
- `lion feed read|post|comment|react|engagement`
- `lion schema --json` · `lion version`

**Known v1 limitations.** These fail with a clear error rather than pretending:

- **`lion sync` and paged message history.** LinkedIn retired the REST
  messaging endpoints; `message list` and `message read` now use the messaging
  GraphQL surface, but that query takes no pagination variables (`count` and
  `lastUpdatedBefore` are accepted and ignored). LinkedIn's own web app pages
  it by sync token instead, which `sync`, `export`, and history backfill have
  yet to be migrated to — they still call the retired endpoint and fail with
  HTTP 500. Rewriting them to fetch a single page would have quietly cost
  those commands their paging and resume behaviour, so the migration is left
  to its own change.
- **Participant identity changed shape.** The messaging GraphQL surface
  identifies people by `urn:li:fsd_profile:…` where the retired one returned
  `urn:li:fs_miniProfile:…`. Conversations already in a local store carry the
  old form and will not match the new one without a translation step.

- **Viewing someone else's profile by public id.** LinkedIn retired the REST
  `profileView` endpoint (HTTP 410) and its modern replacement is a set of
  GraphQL cards lion doesn't model yet. `profile view me` and
  `profile search` work; use those.
- **`connection requests` / `connection accept`.** Incoming invitations moved
  to a GraphQL surface whose query id hasn't been captured yet.
- **`message send` takes a conversation id or a profile URN**, not a bare
  person id — resolving one needs the unsupported profile-by-id lookup above.
- **Unix only** (macOS, Linux). The action-budget lock uses `flock`.

Sessions do lapse eventually — LinkedIn rotates `JSESSIONID`/`li_at`/`lidc` and
expires the Cloudflare `__cf_bm` cookie continuously — but lion writes rotated
cookies back after each successful command, so a stored session stays alive the
way a browser's does rather than decaying within minutes of login. Re-run
`lion auth login` if it does lapse.

Later: MCP server, and a Phase-2 CRM + campaign engine (inspired by
LinkedHelper and Lemlist).

## Quick start

```sh
make build
# Sign in once, in a real browser window. lion keeps the session in a
# Chromium profile it owns, so later runs need nothing pasted.
./bin/lion --browser auth login
./bin/lion --browser profile view me --json
./bin/lion --browser profile search "compiler engineer" --max 10 --plain
```

### Two transports

`--browser` (recommended) drives a real Chromium that lion owns and issues
Voyager calls as same-origin `fetch()` from inside a loaded linkedin.com
page. The TLS handshake, headers, client hints, and origin are Chrome's own,
because they are Chrome's. Sign-in happens in a visible window — lion never
types a password or answers a challenge — and every later command runs
headless, so a periodic `lion --browser sync` works from a timer. Put
`{"browser": true}` in `$LION_HOME/config.json` (or set `LION_BROWSER=1`) to
avoid passing the flag each time.

The older cookie transport is still the default: paste the whole `Cookie:`
header for linkedin.com and lion replays it over a TLS fingerprint borrowed
from uTLS.

```sh
./bin/lion auth login --cookies-stdin
```

Be aware of what it costs. That transport sends a synthesized User-Agent over
a handshake it does not match, without the header set Chrome actually emits.
LinkedIn's bot management cross-checks those signals, and a session used this
way can be **revoked account-wide within minutes** — signing you out of your
own browser, not merely failing the command. Prefer `--browser`.

Every mutation is previewed with `--dry-run` and requires `--yes` to actually
send. `--no-input` suppresses prompts but does **not** authorize a write.

## Design principles

- **stdout is data**; prompts, progress, and warnings go to stderr.
- `--json` / `--plain` (TSV) on everything; stable exit codes.
- Mutations support `--dry-run`; `--readonly` hard-blocks writes.
- `--wrap-untrusted` marks LinkedIn free text as data for downstream LLMs.

## Exit codes

`0` ok · `2` usage · `3` not authenticated · `4` rate limited · `5` not found
· `6` blocked by --readonly · `7` LinkedIn challenge required.
