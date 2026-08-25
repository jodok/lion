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
# Copy the whole Cookie: header for linkedin.com from a logged-in browser —
# li_at + JSESSIONID alone are not enough for the GraphQL endpoints.
./bin/lion auth login --cookies-stdin
./bin/lion profile view me --json
./bin/lion profile search "compiler engineer" --max 10 --plain
```

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
