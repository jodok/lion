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

## 3.2 Verified endpoint reality (live-checked 2026-07-06)

Probed against a real logged-in session. The clean REST-li surface documented by
older tools (e.g. the `linkedin-api` Python library) is **partly deprecated**;
LinkedIn has moved key reads to GraphQL:

| feature       | endpoint we target                              | live result | notes |
|---------------|-------------------------------------------------|-------------|-------|
| me            | `GET /me`                                       | ✅ 200      | `data."*miniProfile"` is a URN ref; object in `included` (decoder fixed) |
| people search | ~~`/search/blended`~~ → `GET /graphql` clusters | ✅ 200      | legacy blended is **404**; use `voyagerSearchDashClusters`, decode `EntityResultViewModel` (fixed) |
| profile by id | ~~`/identity/profiles/{id}/profileView`~~       | ❌ 410 gone | modern profile is decomposed GraphQL cards keyed by member id; not yet modeled |
| connections   | `GET /relationships/connections`                | ✅ 200      | results in `data.elements`; decoder should follow refs (documented) |
| invitations   | `/relationships/invitationViews`                | ❌ 400      | needs the GraphQL invitations surface; endpoint TBD |
| messaging     | `GET /messaging/conversations`                  | ✅ 200      | results in `data.elements` |
| feed          | `GET /feed/updatesV2?q=chronFeed`               | ✅ 200      | results in `data.elements` |

### queryId maintenance (open decision)

GraphQL calls carry a `queryId` like `voyagerSearchDashClusters.a7a0567f…` whose
hash is pinned to a specific LinkedIn web-app build and **rotates over time**.
When a GraphQL call starts returning 400/"missing query", the hash must be
refreshed. All queryIds are centralized in `internal/voyager/graphql.go` so this
is a one-file change. Strategy options (owner to pick):

1. **Pin & update** (current): hardcode current hashes, refresh manually. Simple,
   but breaks silently when LinkedIn ships a new build.
2. **Runtime scrape**: parse the logged-in web app's JS bundle to extract current
   hashes on the fly. Robust, but more code and its own fragility.
3. **REST-preferred hybrid**: use the still-working REST-li endpoints wherever
   they exist (me/connections/messaging/feed) and only fall back to GraphQL for
   search/profile. Smallest GraphQL surface to maintain.

## 3.3 The transport problem: Cloudflare bot management (live-checked 2026-07-06)

This is the single most important constraint for the project, discovered by
running the real binary against a live account.

**LinkedIn fronts Voyager with Cloudflare bot management that fingerprints the
TLS handshake.** Go's standard `net/http` TLS ClientHello is detectably not
Chrome, and LinkedIn responds accordingly:

- **GraphQL endpoints (search, modern profile): blocked immediately.** The
  request gets a `302` that redirects to the *same URL* while sending
  `Set-Cookie: li_at=delete me; Expires=1970` — i.e. it wipes the session
  cookie. A no-cookie-jar client then loops until "too many redirects".
- **Simple REST endpoints (`/me`): pass at first, then get blocked** once the
  IP/session has been flagged by repeated non-browser requests.
- Adding every plausible header (referer, `x-li-track`, `x-li-lang`,
  `sec-fetch-*`, page-instance) does **not** help — it is not a header problem.

**A Chrome-impersonating TLS stack defeats the bot wall.** Using
`github.com/bogdanfinn/tls-client` (uTLS, `Chrome_124` profile) the same request
flips from `302`+session-wipe to a clean app-layer `401` JSON — i.e. it gets
past Cloudflare to LinkedIn's auth layer. So **lion's HTTP transport must
impersonate Chrome's TLS fingerprint; stdlib `net/http` is not viable** for the
protected endpoints.

**Cookies: two are not enough for GraphQL.** `li_at` + `JSESSIONID` are accepted
by `/me` but GraphQL appears to want the fuller browser cookie set
(`bcookie`, `bscookie`, `lidc`, `li_gc`, and the Cloudflare `__cf_bm`). So
`auth login` should import the *entire* linkedin.com cookie jar, not two values.

**Operational caution (learned the hard way):** hammering the API from a
non-browser client with a session cookie triggers anti-abuse and can invalidate
the session (observed: a working session went to `401` everywhere, including the
browser, after ~a dozen rapid probes). lion's own limiter is conservative, but
*development/verification* must go through the browser for shape discovery and
touch the real binary sparingly. Do not batch live probes.

### Transport decision — RESOLVED: Option A (uTLS single binary)

Chosen 2026-07-06 and implemented. lion's `Transport` seam
(`internal/voyager/transport.go`) has two impls: a stdlib fallback and
`chromeTransport` (`internal/voyager/chrome_transport.go`) on
`bogdanfinn/tls-client` (Chrome profile) which is what `App.Client` wires in.
`auth login` imports the full linkedin.com cookie jar via `--cookies` (paste the
`Cookie:` header) or `--cookies-file` (Cookie header line or Netscape
cookies.txt, filtered to linkedin.com). JSESSIONID is normalized to exactly one
quote pair at a single boundary (`auth.NormalizeCookies`).

Considered and rejected for now — **B, browser-backed transport** (drive Voyager
through a real logged-in Chrome via CDP/extension): most robust and sidesteps
the Cloudflare arms race, but lion would no longer be a standalone binary. Kept
as the fallback if Cloudflare tightening makes uTLS impractical.

**Live result (2026-07-11): Cloudflare bypass CONFIRMED.** With the Chrome-TLS
transport and the full cookie jar, `lion auth login` validated end-to-end (`/me`
→ 200, session saved) and, critically, the session is no longer wiped — the old
stdlib path got a `302` + `Set-Cookie: li_at=delete me`; the Chrome path gets
clean responses. So Option A works: the TLS bot-wall is defeated.

**Open: GraphQL returns app-level `401`.** `profile search` (GraphQL) returns
`{"data":{"status":401}}` with a *valid* session (not a bot-block, no wipe).
`/me` works with identical auth, so this is endpoint-specific. Adding `x-li-track`
did not resolve it. Leading hypothesis: the pinned `queryId` hash is stale — it
was captured from an earlier web-app build and these hashes rotate. Resolving it
needs the *current* queryId captured from a logged-in browser's network tab
(voyagerSearchDashClusters.*), then updated in `graphql.go`. Alternative
hypothesis to rule out: the throwaway account became search-restricted after the
earlier over-probing (verify search still returns 200 in-browser).

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
