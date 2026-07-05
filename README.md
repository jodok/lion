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

Early development. See [DESIGN.md](DESIGN.md) for the architecture and roadmap.

Working today:

- `lion auth login|status|logout` — store & validate a session cookie
- `lion profile view [id|me]` — view a profile
- `lion profile search <query>` — people search

In progress (delegated verticals): connections, messaging, feed/posting.
Later: MCP server, `schema --json`, and a Phase-2 CRM + campaign engine
(inspired by LinkedHelper and Lemlist).

## Quick start

```sh
make build
# Get li_at + JSESSIONID from a logged-in browser's cookies for linkedin.com
./bin/lion auth login
./bin/lion profile view me --json
./bin/lion profile search "compiler engineer" --max 10 --plain
```

## Design principles

- **stdout is data**; prompts, progress, and warnings go to stderr.
- `--json` / `--plain` (TSV) on everything; stable exit codes.
- Mutations support `--dry-run`; `--readonly` hard-blocks writes.
- `--wrap-untrusted` marks LinkedIn free text as data for downstream LLMs.

## Exit codes

`0` ok · `2` usage · `3` not authenticated · `4` rate limited · `5` not found
· `6` blocked by --readonly · `7` LinkedIn challenge required.
