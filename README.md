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
- `lion bookmark list` — posts you've saved on LinkedIn
- `lion doctor` — self-checks: home, transport, browser, store, session
- `lion schema --json` · `lion version`

**Known v1 limitations.** These fail with a clear error rather than pretending:

- **Incremental sync trusts LinkedIn's delta stream.** `lion sync` stores the
  sync token each run and asks only for what changed since, which is what
  makes a periodic sync cheap. If the delta ever drifts from what is stored —
  a conversation that changed without being reported, say — `lion sync --full`
  ignores the stored tokens and takes a complete snapshot. A token the server
  rejects is dropped automatically and the run falls back to a full snapshot.
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
./bin/lion auth login
./bin/lion profile view me --json
./bin/lion profile search "compiler engineer" --max 10 --plain
```

### Transports

lion drives a real Chromium it owns, and issues Voyager calls as same-origin
`fetch()` from inside a loaded linkedin.com page — so the TLS handshake,
headers, client hints, and origin are Chrome's own, because they are Chrome's.
Sign-in happens in a visible window; lion never types a password or answers a
challenge. Every later command runs headless against the profile it saved, so
a periodic `lion sync` works from a timer.

The older cookie transport is **deprecated**. It replays a pasted cookie jar
over a synthesized TLS fingerprint, and LinkedIn cross-checks those signals:
a session used that way can be **revoked account-wide within minutes**,
signing you out of your own browser. It is still reachable with
`--cookie-transport`, or by passing any of the cookie options to
`auth login`, and both print a warning.

```sh
lion auth login                                  # browser, the default
pbpaste | lion auth login --cookies-stdin        # deprecated cookie path
```

Upgrading from a cookie-only setup: your stored credential is not used
automatically. Run `lion auth login` once to sign in through a browser, or
pass `--cookie-transport` to keep the old behaviour — lion says as much if it
finds stored cookies and no browser session.

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
