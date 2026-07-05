# lion — a task-first LinkedIn CLI

> One binary. Task-first commands. Predictable automation.
> Modeled on [gogcli](https://github.com/openclaw/gogcli), applied to LinkedIn.

`lion` is a command-line interface for LinkedIn. It is designed to be fast,
scriptable, and agent-friendly: parseable output on stdout, prompts and
progress on stderr, stable exit codes, `--json`/`--plain` everywhere, and a
built-in MCP server (read-only by default).

Because LinkedIn has no meaningful public API for the interesting surface
(people search, feed, messaging, profile data), `lion` talks to LinkedIn's
internal **Voyager** API using a browser session cookie (`li_at`). This is
against LinkedIn's Terms of Service and carries account-restriction risk, so
`lion` is conservative by default: rate limiting, human-like jitter, and
`--dry-run` on every mutation.

---

## 1. Goals & non-goals

**Goals**

- gogcli-grade UX: `lion [global-flags] <resource> <action> [id] [options]`.
- Cover the three pillars the owner cares about first:
  1. **Profile & connections** — view/search people, manage connections, invites.
  2. **Messaging** — read conversations, send messages, unread triage.
  3. **Feed & posting** — read feed, post, comment, react, view engagement.
- Be safe for a throwaway/test account: conservative rate limits + jitter by
  default, `--dry-run`, `--readonly`, explicit confirmation for writes.
- Be automatable: `--json`, `--plain` (TSV), stable exit codes, `--no-input`.
- Ship an MCP server so agents can drive it (read-only tools by default).

**Non-goals (for now)**

- The official LinkedIn OAuth/Marketing API (too limited to matter here).
- A GUI. A hosted service. Bulk scraping at scale.
- Jobs/companies as a first-class pillar (kept as a stretch resource).

---

## 2. Architecture

Single static Go binary. Cobra for command tree. No CGO. Layout:

```
cmd/lion/main.go            entrypoint
internal/
  cli/                      cobra commands (thin: parse flags -> call client -> render)
    root.go                 root cmd + global flags + context wiring
    auth.go                 auth login/status/logout
    profile.go              profile view/search
    connection.go           connection list/invite/remove/accept
    message.go              message list/read/send
    feed.go                 feed read/post/comment/react
    mcp.go                  mcp server
  voyager/                  Voyager API client (the hard part)
    client.go               HTTP client, headers, retry, rate limit, error mapping
    auth.go                 cookie/csrf handling
    profile.go              profile endpoints + decoders
    connection.go           connection/invitation endpoints
    messaging.go            messaging endpoints (GraphQL)
    feed.go                 feed/share endpoints
    search.go               people/company search (GraphQL)
    types.go                domain structs (Profile, Connection, Message, Post...)
    decode.go               Voyager "normalized+json" flattening helpers
  config/config.go          config file + env + flag precedence
  auth/store.go             credential storage (0600 file, optional OS keyring)
  output/output.go          render json | plain(TSV) | table | raw
  ratelimit/limiter.go      token-bucket + human jitter + per-action budgets
  mcpserver/                MCP server (wraps read-only subset)
testdata/fixtures/          recorded Voyager responses for offline tests
```

**Design rule:** `cli/*` never touches HTTP. `voyager/*` never touches
cobra/stdout. This keeps commands trivial and the client unit-testable with
fixtures (no live account needed to develop).

### 2.1 Voyager client

- Base: `https://www.linkedin.com/voyager/api/`.
- Auth cookies: `li_at` (session) and `JSESSIONID` (also the CSRF token).
- Required headers on every request:
  - `csrf-token: <JSESSIONID value, quotes stripped>`
  - `x-restli-protocol-version: 2.0.0`
  - `x-li-lang: en_US`, `x-li-track: {...}` (client version metadata)
  - `accept: application/vnd.linkedin.normalized+json+2.1`
  - a realistic desktop `user-agent`.
- Newer surfaces (search, messaging) are GraphQL: `/voyager/api/graphql`
  with `queryId` + variables. Older REST-li endpoints still exist for
  profileView, invitations, etc. Client supports both.
- **Normalization:** Voyager returns a flat `included[]` array with `$type`
  and URN references. `decode.go` builds a URN->object index and resolves
  references so the typed decoders in each resource file stay readable.
- **Error mapping** -> stable exit codes (see §4).
- Transport is injectable (`http.RoundTripper`) so tests replay fixtures.

### 2.2 Safety layer (default-on)

- `ratelimit`: token bucket per action class (read vs write vs invite) with
  randomized jitter (e.g. 3–9s between writes) to mimic human pacing.
  Conservative defaults; overridable via config/flags.
- `--dry-run`: every mutation prints the intended request and exits 0 without
  sending.
- `--readonly`: hard-blocks all mutating commands.
- Writes prompt for confirmation on a TTY unless `--yes`/`--no-input`.
- Daily/session action budgets (invites/day, messages/day) enforced locally,
  tracked in the state file.

### 2.3 Output contract (agent-friendly, from gogcli)

- **stdout** = data only. **stderr** = prompts, progress, warnings.
- `--json` structured, `--plain` TSV, default = human table.
- `--wrap-untrusted` wraps free-text fetched from LinkedIn (message bodies,
  post text) in delimiters so downstream LLMs treat it as data, not
  instructions.
- Stable exit codes documented in `--help` and `lion schema --json`.

---

## 3. Command surface (v1)

```
lion auth login              # paste li_at (+JSESSIONID) or import from browser
lion auth status             # who am I, cookie validity, rate-limit budget
lion auth logout

lion profile view [id|me]    # full profile (id = public id or urn)
lion profile search 'query' --title --company --location --max N

lion connection list [--max N] [--since DATE]
lion connection invite <id> [--note TEXT]
lion connection accept <id> | --all
lion connection remove <id>
lion connection requests [--incoming|--outgoing]

lion message list [--unread] [--max N]
lion message read <conversation-id>
lion message send <id|conversation> TEXT   # id = person -> new/existing thread

lion feed read [--max N]
lion feed post TEXT [--visibility connections|public] [--media FILE]
lion feed comment <urn> TEXT
lion feed react <urn> [--type like|celebrate|support|...]
lion feed engagement <urn>   # who liked/commented

lion mcp serve               # MCP server, read-only tools by default
lion schema --json           # machine-readable command/flag/exit-code schema
lion version
```

**Global flags:** `--json --plain --readonly --dry-run --yes --no-input
--max --account <alias> --wrap-untrusted --verbose --config PATH`.

### 3.1 Phase 2 (outreach, inspired by LinkedHelper + Lemlist)

Not in v1, but the architecture leaves room:

- **Local CRM**: SQLite of leads with tags, notes, custom fields, pipeline
  stage. `lion crm add/tag/list/export`.
- **Sequences/campaigns**: YAML-defined multi-step flows (view → invite →
  wait → message → follow-up) with reply detection that halts a lead's
  sequence. `lion campaign run <file> --dry-run`. This is where a CLI shines:
  version-controlled, diffable campaign definitions.
- **Unified inbox** view across conversations, filterable by campaign/tag.

These are deliberately separate resources so v1 stays small.

---

## 4. Exit codes (stable contract)

| code | meaning                                   |
|------|-------------------------------------------|
| 0    | success                                   |
| 1    | generic error                            |
| 2    | usage / bad flags                        |
| 3    | not authenticated / cookie expired       |
| 4    | rate limited (local budget or 429)       |
| 5    | not found                                |
| 6    | permission / blocked by --readonly       |
| 7    | LinkedIn challenge / checkpoint required |

---

## 5. Testing

- Unit tests per resource decode fixtures in `testdata/fixtures/` (recorded,
  scrubbed Voyager responses) — no live account needed.
- `voyager.Client` takes an injectable transport; tests serve fixtures.
- A `--record` mode (guarded, off by default) can capture new fixtures from a
  live session for maintainers.
- Golden-file tests for `--json`/`--plain` output stability.

---

## 6. Build & delivery plan (how this repo gets built)

Owner-driven, model-delegated. Roles per repo model policy:

- **Architecture, command UX, output contract, review** — fable-5 (taste-heavy).
- **Mechanical Go implementation of resource verticals** — sonnet-5 subagents,
  one vertical per worktree/branch, each following the `profile` reference
  vertical.
- **Independent code review of each vertical** — codex (gpt-5.5) + fable-5.
- **Iterative polish** — `/loop` runs later.

Sequence:

1. **Core (this branch):** scaffold, config, auth store, Voyager client
   (headers/retry/rate limit/decode), output package, root cmd, and the
   `profile` vertical as the reference implementation + fixtures. Open draft PR.
2. **Fan-out:** subagents implement `connection`, `message`, `feed` verticals
   on branches off core, mirroring `profile`. Codex reviews each PR.
3. **MCP + schema:** wrap the read-only subset; emit `schema --json`.
4. **Phase 2:** CRM + campaigns.

Live integration testing waits on a throwaway-account `li_at` cookie from the
owner; until then everything is validated with fixtures and `--dry-run`.
